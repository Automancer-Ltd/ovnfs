package nfs

import (
	"os"
	"runtime"
	"testing"

	"github.com/stencil-hq/ovnfs/internal/unixstat"
)

// accessInfo is a backend entry with independently specified permission
// classes. It avoids needing privileges to test files owned by other users.
type accessInfo struct {
	fakeInfo
	mode os.FileMode
	stat any
}

func (i accessInfo) Mode() os.FileMode { return i.mode }
func (i accessInfo) Sys() any          { return i.stat }

func TestAccessPermissionClass(t *testing.T) {
	user := accessIdentity{uid: 50, gid: 20, groups: []int{80}}
	for _, tc := range []struct {
		name string
		stat any
		want os.FileMode
	}{
		{"owner_before_group", unixstat.Info{UID: 50, GID: 20}, 6},
		{"primary_group", unixstat.Info{UID: 60, GID: 20}, 4},
		{"supplementary_group", unixstat.Info{UID: 60, GID: 80}, 4},
		{"other", unixstat.Info{UID: 60, GID: 90}, 1},
		{"virtual_owner", nil, 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := user.permissions(accessInfo{mode: 0o641, stat: tc.stat}); got != tc.want {
				t.Errorf("rwx = %o, want %o", got, tc.want)
			}
		})
	}
}

// TestAccessWire asks both protocols about the same object after chmod. READ
// on a directory lists names; LOOKUP searches them. Changing entries needs
// write and search together. EXECUTE is meaningful only on a file.
func TestAccessWire(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("host chmod does not implement Unix permission classes")
	}
	fs := newTestFS(t)
	v4write(t, fs, "file", "contents")
	if err := os.Mkdir(fs.host("dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	v3 := v3server(t, fs)
	_, addr := v4serve(t, fs, nil)
	v4 := v4dial(t, addr)
	const rw = access4Read | access4Modify | access4Extend
	const file = rw | access4Execute
	const dir = rw | access4Lookup | access4Delete
	for _, tc := range []struct {
		name, path string
		mode       os.FileMode
		request    uint32
		supported  uint32
		granted    uint32
		asRoot     uint32
	}{
		{"nonexecutable", "file", 0o644, access4Execute, access4Execute, 0, 0},
		{"regular", "file", 0o644, 0xff, file, rw, rw},
		{"executable", "file", 0o755, access4All, file, file, file},
		{"owner_not_other", "file", 0o601, access4Execute, access4Execute, 0, access4Execute},
		{"read_only", "file", 0o444, access4All, file, access4Read, rw},
		{"writable_directory", "dir", 0o755, access4All, dir, dir, dir},
		{"list_without_search", "dir", 0o666, access4All, dir, access4Read, dir},
		{"search_without_write", "dir", 0o111, access4All, dir, access4Lookup, dir},
		{"request_subset", "dir", 0o700, access4Lookup, access4Lookup, access4Lookup, access4Lookup},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.Chmod(fs.host(tc.path), tc.mode); err != nil {
				t.Fatal(err)
			}
			fid := fs.FileID(tc.path)
			want := tc.granted
			if os.Geteuid() == 0 {
				want = tc.asRoot
			}
			st, d := v3.v3call(v3Access, func(e *encoder) { e.fh(fid, 0); e.u32(tc.request) })
			if _, attrs := postOpOf(t, d); st != stOK || attrs {
				t.Fatalf("v3 ACCESS: status %d, attributes %v", st, attrs)
			}
			if got := d.u32(); got != want || d.err != nil || len(d.buf) != 0 {
				t.Errorf("v3 ACCESS %04o request %#x: granted %#x, want %#x", tc.mode, tc.request, got, want)
			}
			r := v4.run(v4PutFH(fid), v4Access(tc.request))
			r.skip(op4PutFH)
			d = r.next(op4Access, stOK)
			if supported, got := d.u32(), d.u32(); supported != tc.supported || got != want {
				t.Errorf("v4 ACCESS %04o request %#x: supported %#x, granted %#x; want %#x, %#x", tc.mode, tc.request, supported, got, tc.supported, want)
			}
			r.end()
		})
	}
}
