//go:build !windows

package kfs

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stencil-hq/ovnfs/admin"
	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/checkout"
	"github.com/stencil-hq/ovnfs/export"
	"github.com/stencil-hq/ovnfs/overlay"
	"github.com/stencil-hq/ovnfs/stats"
)

// fixture is a real registry and export with checkouts made from one seeded folder.
type fixture struct {
	e     *export.Export
	reg   *checkout.Registry
	stats *stats.Recorder // the checkout's file calls, as the daemon records them
	src   string
	state string
}

// newFixture opens a registry and export on a temp state dir, with a memory
// tier that keeps written files until they are synced or flushed, and
// creates the checkouts names from one folder holding pkg/lib.go.
func newFixture(t *testing.T, names ...string) *fixture {
	t.Helper()
	return newFixtureWith(t, overlay.NewMemory(16<<20, 256<<10, 0), names...)
}

// newFixtureWith is newFixture with the memory tier mem.
func newFixtureWith(t *testing.T, mem *overlay.Memory, names ...string) *fixture {
	t.Helper()
	dir := t.TempDir()
	f := &fixture{src: filepath.Join(dir, "src"), state: filepath.Join(dir, "state")}
	if err := os.MkdirAll(filepath.Join(f.src, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.src, "pkg", "lib.go"), []byte("package pkg\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec, err := stats.Open(filepath.Join(dir, "stats"), t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rec.Close)
	f.stats = rec
	reg, err := checkout.Open(checkout.Config{
		Stats:  rec,
		State:  f.state,
		Mount:  filepath.Join(dir, "mount"),
		Memory: mem,
		Retire: func(name string, slot uint16) {
			if f.e != nil {
				f.e.Retire(name, slot)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := reg.Close(); err != nil {
			t.Errorf("close registry: %v", err)
		}
	})
	f.reg = reg
	f.e = export.New(export.NewRouter(reg), f.state)
	for _, name := range names {
		if _, err := reg.New(context.Background(), api.NewArgs{Dir: f.src, Name: name}); err != nil {
			t.Fatalf("new checkout %s: %v", name, err)
		}
	}
	return f
}

// serve serves checkout name of f on a fresh socket and returns its path.
func (f *fixture) serve(t *testing.T, name string, writable bool) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "kfs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "k.sock")
	l, err := admin.Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- Serve(l, Share{Export: f.e, Checkout: name, Writable: writable}, Options{}) }()
	t.Cleanup(func() {
		l.Close()
		if err := <-done; err != nil {
			t.Errorf("serve: %v", err)
		}
	})
	return sock
}

// dial connects a client that the test closes.
func dial(t *testing.T, sock string) *Client {
	t.Helper()
	c, err := Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// must returns v, failing the test passed to its result on err.
func must[T any](v T, err error) func(*testing.T) T {
	return func(t *testing.T) T {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
}

// wantErrno fails unless err is errno.
func wantErrno(t *testing.T, what string, err error, errno Errno) {
	t.Helper()
	if !errors.Is(err, errno) {
		t.Fatalf("%s: %v, want %v", what, err, errno)
	}
}

// names returns the names of entries.
func names(entries []Dirent) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Name
	}
	return out
}

// TestOps drives every op's success path through the reference client.
func TestOps(t *testing.T) {
	f := newFixture(t, "vm")
	c := dial(t, f.serve(t, "vm", true))

	h := must(c.Hello(7))(t)
	if h.Version != ProtoVersion || h.Flags != 0 || h.MaxIO != MaxIO || h.Root.ID != RootID || !h.Root.IsDir() {
		t.Fatalf("hello = %+v", h)
	}
	epoch := c.Epoch
	if got := must(c.Poll())(t); got != epoch {
		t.Fatalf("poll epoch %x, hello's %x", got, epoch)
	}
	if st := must(c.Statfs())(t); st.Bsize == 0 || st.Blocks == 0 || st.Namelen != NameMax {
		t.Fatalf("statfs = %+v", st)
	}
	if a := must(c.Getattr(RootID))(t); a.ID != RootID || !a.IsDir() {
		t.Fatalf("getattr root = %+v", a)
	}

	pkg := must(c.Lookup(RootID, "pkg"))(t)
	lib := must(c.Lookup(pkg.ID, "lib.go"))(t)
	if lib.Mode != sIFREG|0o644 || lib.Size != 12 || lib.Nlink != 1 || lib.MtimeNs == 0 || lib.Blocks != 1 {
		t.Fatalf("lookup lib.go = %+v", lib)
	}
	info, err := os.Stat(filepath.Join(f.src, "pkg", "lib.go"))
	if err != nil {
		t.Fatal(err)
	}
	if lib.MtimeNs != uint64(info.ModTime().UnixNano()) {
		t.Fatalf("mtime %d, source has %d", lib.MtimeNs, info.ModTime().UnixNano())
	}
	if got := must(c.Read(lib.ID, 0, 64))(t); string(got) != "package pkg\n" {
		t.Fatalf("read = %q", got)
	}
	if got := must(c.Read(lib.ID, 8, 64))(t); string(got) != "pkg\n" {
		t.Fatalf("read at 8 = %q", got)
	}
	root, err := c.ReaddirAll(RootID, 4096)
	if err != nil || !slices.Equal(names(root), []string{"pkg"}) || root[0].Attr != pkg {
		t.Fatalf("readdir root = %+v, %v", root, err)
	}

	out := must(c.Mkdir(RootID, "out", 0o750))(t)
	if out.Mode != sIFDIR|0o750 {
		t.Fatalf("mkdir mode %o", out.Mode)
	}
	a := must(c.Create(out.ID, "a.txt", 0o640, CreateExcl))(t)
	if a.Mode != sIFREG|0o640 || a.Size != 0 {
		t.Fatalf("create = %+v", a)
	}
	if w := must(c.Write(a.ID, 0, []byte("from the guest")))(t); w.Size != 14 || w.ID != a.ID {
		t.Fatalf("write = %+v", w)
	}
	if err := c.Fsync(a.ID); err != nil {
		t.Fatalf("fsync: %v", err)
	}
	if got := must(c.Read(a.ID, 5, 100))(t); string(got) != "the guest" {
		t.Fatalf("read back = %q", got)
	}
	if again := must(c.Create(out.ID, "a.txt", 0o600, 0))(t); again.ID != a.ID || again.Size != 14 {
		t.Fatalf("create of an existing file = %+v", again)
	}
	if trunc := must(c.Create(out.ID, "a.txt", 0o600, CreateTrunc))(t); trunc.ID != a.ID || trunc.Size != 0 {
		t.Fatalf("create trunc = %+v", trunc)
	}
	must(c.Write(a.ID, 0, []byte("0123456789")))(t)

	mtime := uint64(time.Date(2020, 1, 2, 3, 4, 5, 6789, time.UTC).UnixNano())
	sa := must(c.Setattr(a.ID, SetattrReq{Valid: SetSize | SetMode | SetMtime, Size: 4, Mode: 0o4755, MtimeNs: mtime}))(t)
	if sa.Size != 4 || sa.Mode != sIFREG|0o4755 || sa.MtimeNs != mtime {
		t.Fatalf("setattr = %+v, want size 4 mode %o mtime %d", sa, sIFREG|0o4755, mtime)
	}
	if got := must(c.Read(a.ID, 0, 100))(t); string(got) != "0123" {
		t.Fatalf("read after truncate = %q", got)
	}
	before := time.Now().Add(-time.Second).UnixNano()
	if now := must(c.Setattr(a.ID, SetattrReq{Valid: SetMtimeNow}))(t); now.MtimeNs < uint64(before) {
		t.Fatalf("setattr mtime now = %d, before %d", now.MtimeNs, before)
	}
	own := must(c.Getattr(a.ID))(t)
	if chown := must(c.Setattr(a.ID, SetattrReq{Valid: SetUID | SetGID, UID: own.UID, GID: own.GID}))(t); chown.UID != own.UID {
		t.Fatalf("setattr owner = %+v", chown)
	}

	link := must(c.Symlink(out.ID, "link", "../pkg/lib.go"))(t)
	if !link.IsSymlink() || link.Size != uint64(len("../pkg/lib.go")) {
		t.Fatalf("symlink = %+v", link)
	}
	if target := must(c.Readlink(link.ID))(t); target != "../pkg/lib.go" {
		t.Fatalf("readlink = %q", target)
	}
	if got := must(c.Lookup(out.ID, "link"))(t); got != link {
		t.Fatalf("lookup of a symlink = %+v, want it unfollowed %+v", got, link)
	}
	hard := must(c.Link(a.ID, RootID, "hard"))(t)
	if hard.ID != a.ID || hard.Nlink != 2 {
		t.Fatalf("link = %+v", hard)
	}
	fifo := must(c.Mknod(out.ID, "fifo", sIFIFO|0o600, 0))(t)
	if fifo.Mode != sIFIFO|0o600 {
		t.Fatalf("mknod fifo mode %o", fifo.Mode)
	}
	sock := must(c.Mknod(out.ID, "sock", sIFSOCK|0o600, 0))(t)
	if sock.Mode&sIFMT != sIFSOCK {
		t.Fatalf("mknod socket mode %o", sock.Mode)
	}
	listed, err := c.ReaddirAll(out.ID, 4096)
	if err != nil || !slices.Equal(names(listed), []string{"a.txt", "fifo", "link", "sock"}) {
		t.Fatalf("readdir out = %v, %v", names(listed), err)
	}

	if err := c.Rename(out.ID, "a.txt", RootID, "b.txt", 0); err != nil {
		t.Fatalf("rename: %v", err)
	}
	for _, name := range []string{"fifo", "link", "sock"} {
		if err := c.Unlink(out.ID, name); err != nil {
			t.Fatalf("unlink %s: %v", name, err)
		}
	}
	if err := c.Rmdir(RootID, "out"); err != nil {
		t.Fatalf("rmdir: %v", err)
	}
	wantErrno(t, "getattr removed dir", must2(c.Getattr(out.ID)), ESTALE)
	if err := c.Unlink(RootID, "hard"); err != nil {
		t.Fatalf("unlink hard link: %v", err)
	}
	if b := must(c.Lookup(RootID, "b.txt"))(t); b.ID != a.ID || b.Nlink != 1 || b.Size != 4 {
		t.Fatalf("renamed file = %+v", b)
	}
	if c.Epoch != epoch {
		t.Fatalf("the share's own requests changed the epoch from %x to %x", epoch, c.Epoch)
	}
}

// must2 returns only the error of a two-value call.
func must2[T any](_ T, err error) error { return err }

// TestConfinement checks that ids outside the served checkout are stale.
func TestConfinement(t *testing.T) {
	f := newFixture(t, "vm", "other")
	c := dial(t, f.serve(t, "vm", true))
	o := dial(t, f.serve(t, "other", true))
	foreign := must(o.Lookup(RootID, "pkg"))(t)
	own := must(c.Lookup(RootID, "pkg"))(t)
	if foreign.ID == own.ID {
		t.Fatalf("two checkouts share id %d", own.ID)
	}
	wantErrno(t, "getattr of another checkout's id", must2(c.Getattr(foreign.ID)), ESTALE)
	wantErrno(t, "lookup in another checkout's dir", must2(c.Lookup(foreign.ID, "lib.go")), ESTALE)
	wantErrno(t, "create in another checkout's dir", must2(c.Create(foreign.ID, "x", 0o644, 0)), ESTALE)
	otherRoot := f.e.Nodes().ID("other")
	wantErrno(t, "getattr of another checkout's root", must2(c.Getattr(otherRoot)), ESTALE)
	wantErrno(t, "getattr of the export's marker", must2(c.Getattr(2)), ESTALE)
	wantErrno(t, "getattr of an id that names nothing", must2(c.Getattr(own.ID+1<<30)), ESTALE)
	if _, err := os.Stat(filepath.Join(f.state, "x")); err == nil {
		t.Fatal("create escaped")
	}
}

// TestReaddirPaging pages through a directory while it changes: a name added
// behind the page shows up, a removed one does not, and the listing resumes
// behind a name that no longer exists.
func TestReaddirPaging(t *testing.T) {
	f := newFixture(t, "vm")
	sock := f.serve(t, "vm", true)
	c, other := dial(t, sock), dial(t, sock)
	dir := must(c.Mkdir(RootID, "d", 0o755))(t)
	var want []string
	for i := range 40 {
		name := "f" + string(rune('a'+i/26)) + string(rune('a'+i%26))
		must(c.Create(dir.ID, name, 0o644, CreateExcl))(t)
		want = append(want, name)
	}
	// Room for a few entries per page.
	const page = ReaddirRepSize + 4*80
	first, eof, err := c.Readdir(dir.ID, "", page)
	if err != nil || eof || len(first) < 2 || !slices.Equal(names(first), want[:len(first)]) {
		t.Fatalf("first page = %v, eof %v, %v", names(first), eof, err)
	}
	last := first[len(first)-1].Name
	second, _, err := c.Readdir(dir.ID, last, page)
	if err != nil || second[0].Name != want[len(first)] {
		t.Fatalf("second page = %v, %v", names(second), err)
	}

	// Another connection adds a name ahead, removes one ahead and the last
	// name returned.
	resume := second[len(second)-1].Name
	must(other.Create(dir.ID, "fzz", 0o644, CreateExcl))(t)
	if err := other.Unlink(dir.ID, "fbn"); err != nil {
		t.Fatal(err)
	}
	if err := other.Unlink(dir.ID, resume); err != nil {
		t.Fatal(err)
	}
	var rest []string
	for after := resume; ; {
		entries, eof, err := c.Readdir(dir.ID, after, page)
		if err != nil {
			t.Fatal(err)
		}
		rest = append(rest, names(entries)...)
		if eof {
			break
		}
		after = entries[len(entries)-1].Name
	}
	var wantRest []string
	for _, name := range append(want, "fzz") {
		if name > resume && name != "fbn" {
			wantRest = append(wantRest, name)
		}
	}
	if !slices.Equal(rest, wantRest) {
		t.Fatalf("pages after the change = %v, want %v", rest, wantRest)
	}

	// A page holds whole entries only, and at least one.
	one, _, err := c.Readdir(dir.ID, "", ReaddirRepSize)
	if err != nil || len(one) != 1 {
		t.Fatalf("a page too small for an entry = %v, %v", names(one), err)
	}
	sorted := names(must(c.ReaddirAll(dir.ID, MaxIO))(t))
	if !sort.StringsAreSorted(sorted) {
		t.Fatalf("listing not sorted: %v", sorted)
	}
}

// TestIDsFollowRename checks that a file id, and the file kept open for it,
// follow the file through renames.
func TestIDsFollowRename(t *testing.T) {
	f := newFixture(t, "vm")
	c := dial(t, f.serve(t, "vm", true))
	a := must(c.Mkdir(RootID, "a", 0o755))(t)
	b := must(c.Mkdir(RootID, "b", 0o755))(t)
	file := must(c.Create(a.ID, "x", 0o644, CreateExcl))(t)
	must(c.Write(file.ID, 0, []byte("one ")))(t)
	if err := c.Rename(a.ID, "x", b.ID, "y", 0); err != nil {
		t.Fatal(err)
	}
	must(c.Write(file.ID, 4, []byte("two")))(t)
	if got := must(c.Read(file.ID, 0, 64))(t); string(got) != "one two" {
		t.Fatalf("read after rename = %q", got)
	}
	if y := must(c.Lookup(b.ID, "y"))(t); y.ID != file.ID || y.Size != 7 {
		t.Fatalf("renamed entry = %+v, want id %d", y, file.ID)
	}
	if err := c.Rename(RootID, "a", b.ID, "a", 0); err != nil {
		t.Fatal(err)
	}
	if got := must(c.Getattr(a.ID))(t); got.ID != a.ID || !got.IsDir() {
		t.Fatalf("moved dir = %+v", got)
	}
	// A name replaced by a rename leaves its old id stale.
	z := must(c.Create(b.ID, "z", 0o644, CreateExcl))(t)
	must(c.Write(z.ID, 0, []byte("z")))(t)
	if err := c.Rename(b.ID, "y", b.ID, "z", 0); err != nil {
		t.Fatal(err)
	}
	wantErrno(t, "read of a replaced file", must2(c.Read(z.ID, 0, 1)), ESTALE)
	if got := must(c.Read(file.ID, 0, 64))(t); string(got) != "one two" {
		t.Fatalf("read of the replacing file = %q", got)
	}
}

// onDisk reports whether a file with base name name and content data exists
// anywhere under dir.
func onDisk(t *testing.T, dir, name string, data []byte) bool {
	t.Helper()
	found := false
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != name {
			return nil
		}
		if b, err := os.ReadFile(p); err == nil && string(b) == string(data) {
			found = true
		}
		return nil
	})
	return found
}

// TestWriteNotSynced checks that WRITE leaves data where unsynced writes
// live (the memory tier), visible to another connection, and that FSYNC
// moves it to disk.
func TestWriteNotSynced(t *testing.T) {
	f := newFixture(t, "vm")
	sock := f.serve(t, "vm", true)
	c, other := dial(t, sock), dial(t, sock)
	file := must(c.Create(RootID, "unsynced.txt", 0o644, CreateExcl))(t)
	data := []byte("not yet durable")
	must(c.Write(file.ID, 0, data))(t)
	if got := must(other.Read(file.ID, 0, 64))(t); string(got) != string(data) {
		t.Fatalf("second connection reads %q", got)
	}
	if onDisk(t, f.state, "unsynced.txt", data) {
		t.Fatal("WRITE synced the file to disk")
	}
	if err := other.Fsync(file.ID); err != nil {
		t.Fatal(err)
	}
	if !onDisk(t, f.state, "unsynced.txt", data) {
		t.Fatal("FSYNC left the file off disk")
	}
	// Writes through one connection drop readers another one keeps.
	must(c.Write(file.ID, 0, []byte("NOT")))(t)
	if got := must(other.Read(file.ID, 0, 64))(t); string(got) != "NOT yet durable" {
		t.Fatalf("second connection reads %q after a write", got)
	}
	// A base file read before a write is read afresh after it (copy-up).
	lib := must(c.Lookup(must(c.Lookup(RootID, "pkg"))(t).ID, "lib.go"))(t)
	must(other.Read(lib.ID, 0, 64))(t)
	must(c.Write(lib.ID, 0, []byte("PACKAGE")))(t)
	if got := must(other.Read(lib.ID, 0, 64))(t); string(got) != "PACKAGE pkg\n" {
		t.Fatalf("read after copy-up = %q", got)
	}
}

// TestRenameNoreplace checks that RENAME_NOREPLACE refuses an existing name and leaves both files.
func TestRenameNoreplace(t *testing.T) {
	f := newFixture(t, "vm")
	c := dial(t, f.serve(t, "vm", true))
	x := must(c.Create(RootID, "x", 0o644, CreateExcl))(t)
	y := must(c.Create(RootID, "y", 0o644, CreateExcl))(t)
	wantErrno(t, "rename noreplace", c.Rename(RootID, "x", RootID, "y", RenameNoreplace), EEXIST)
	if got := must(c.Lookup(RootID, "y"))(t); got.ID != y.ID {
		t.Fatalf("y replaced: %+v", got)
	}
	if err := c.Rename(RootID, "x", RootID, "z", RenameNoreplace); err != nil {
		t.Fatalf("rename noreplace to a free name: %v", err)
	}
	if got := must(c.Lookup(RootID, "z"))(t); got.ID != x.ID {
		t.Fatalf("z = %+v, want id %d", got, x.ID)
	}
}

// TestErrors checks the errnos ops answer through the protocol.
func TestErrors(t *testing.T) {
	f := newFixture(t, "vm")
	sock := f.serve(t, "vm", true)
	c := dial(t, sock)
	pkg := must(c.Lookup(RootID, "pkg"))(t)
	lib := must(c.Lookup(pkg.ID, "lib.go"))(t)
	wantErrno(t, "lookup missing", must2(c.Lookup(RootID, "missing")), ENOENT)
	wantErrno(t, "lookup in a file", must2(c.Lookup(lib.ID, "x")), ENOTDIR)
	wantErrno(t, "mkdir existing", must2(c.Mkdir(RootID, "pkg", 0o755)), EEXIST)
	wantErrno(t, "create excl existing", must2(c.Create(pkg.ID, "lib.go", 0o644, CreateExcl)), EEXIST)
	wantErrno(t, "create over a dir", must2(c.Create(RootID, "pkg", 0o644, 0)), EISDIR)
	wantErrno(t, "read a dir", must2(c.Read(pkg.ID, 0, 1)), EISDIR)
	wantErrno(t, "unlink a dir", c.Unlink(RootID, "pkg"), EISDIR)
	wantErrno(t, "rmdir a file", c.Rmdir(pkg.ID, "lib.go"), ENOTDIR)
	wantErrno(t, "rmdir non-empty", c.Rmdir(RootID, "pkg"), ENOTEMPTY)
	wantErrno(t, "name with a slash", must2(c.Lookup(RootID, "a/b")), EINVAL)
	wantErrno(t, "dot dot", must2(c.Lookup(RootID, "..")), EINVAL)
	wantErrno(t, "long name", must2(c.Lookup(RootID, strings.Repeat("n", NameMax+1))), ENAMETOOLONG)
	wantErrno(t, "readlink of a file", must2(c.Readlink(lib.ID)), EINVAL)
	wantErrno(t, "mknod device", must2(c.Mknod(RootID, "dev", sIFCHR|0o600, 0x0101)), EPERM)
	wantErrno(t, "read past max_io", must2(c.Read(lib.ID, 0, MaxIO+1)), EINVAL)
	wantErrno(t, "write past the largest offset", must2(c.Write(lib.ID, 1<<63-2, []byte("xx"))), EFBIG)
	wantErrno(t, "link a dir", must2(c.Link(pkg.ID, RootID, "pkg2")), EPERM)
	wantErrno(t, "rename a dir into itself", c.Rename(RootID, "pkg", pkg.ID, "inner", 0), EINVAL)
	if _, err := c.Call(Op(99), RootID); !errors.Is(err, EINVAL) {
		t.Fatalf("unknown op: %v", err)
	}
	if _, err := c.Call(OpGetattr, RootID, []byte{1}); !errors.Is(err, EINVAL) {
		t.Fatalf("trailing body bytes: %v", err)
	}
	if _, err := c.Call(OpSetattr, RootID, []byte{1, 2}); !errors.Is(err, EINVAL) {
		t.Fatalf("short body: %v", err)
	}
	// A malformed request leaves the connection usable.
	if _, err := c.Getattr(RootID); err != nil {
		t.Fatal(err)
	}

	ro := dial(t, f.serve(t, "vm", false))
	if h := must(ro.Hello(ProtoVersion))(t); h.Flags&HelloRdonly == 0 {
		t.Fatalf("read-only hello flags %x", h.Flags)
	}
	wantErrno(t, "create on a read-only share", must2(ro.Create(RootID, "x", 0o644, 0)), EROFS)
	wantErrno(t, "write on a read-only share", must2(ro.Write(lib.ID, 0, []byte("x"))), EROFS)
	if got := must(ro.Read(lib.ID, 0, 64))(t); string(got) != "package pkg\n" {
		t.Fatalf("read on a read-only share = %q", got)
	}
}

// TestMalformedFrame checks that a frame over the size limit closes the connection.
func TestMalformedFrame(t *testing.T) {
	f := newFixture(t, "vm")
	c := dial(t, f.serve(t, "vm", true))
	frame := le.AppendUint32(nil, MaxFrame+1)
	if _, err := c.conn.Write(frame); err != nil {
		t.Fatal(err)
	}
	c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("server answered an oversized frame")
	} else if errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatal("server kept the connection open")
	}
}

// TestEpoch checks that the epoch changes with daemon-side changes of the
// checkout and between servers, not with the share's own requests.
func TestEpoch(t *testing.T) {
	f := newFixture(t, "vm", "other")
	c := dial(t, f.serve(t, "vm", true))
	first := must(c.Poll())(t)
	must(c.Create(RootID, "own", 0o644, CreateExcl))(t)
	if got := must(c.Poll())(t); got != first {
		t.Fatalf("own create changed the epoch: %x -> %x", first, got)
	}
	f.e.Invalidate("other", []string{"pkg/lib.go"}, nil)
	if got := must(c.Poll())(t); got != first {
		t.Fatalf("another checkout's change changed the epoch: %x -> %x", first, got)
	}
	f.e.Invalidate("vm", []string{"pkg/lib.go"}, nil)
	second := must(c.Poll())(t)
	if second == first {
		t.Fatal("daemon-side change kept the epoch")
	}
	again := dial(t, f.serve(t, "vm", true))
	if got := must(again.Poll())(t); got == second || got == first {
		t.Fatalf("a second server repeats epoch %x", got)
	}
	if err := f.reg.Rm(context.Background(), api.RmArgs{Checkout: "vm"}); err != nil {
		t.Fatal(err)
	}
	if got, err := c.Poll(); err != nil || got == second {
		t.Fatalf("retiring the checkout kept epoch %x: %v", got, err)
	}
}

// TestPermissionsAreTheGuests checks that files the guest made read-only
// still take its writes: the guest kernel checked permissions at its open.
func TestPermissionsAreTheGuests(t *testing.T) {
	f := newFixture(t, "vm")
	c := dial(t, f.serve(t, "vm", true))
	obj := must(c.Create(RootID, "object", 0o444, CreateExcl))(t)
	must(c.Write(obj.ID, 0, []byte("blob")))(t)
	ro := must(c.Create(RootID, "ro", 0o644, CreateExcl))(t)
	must(c.Setattr(ro.ID, SetattrReq{Valid: SetMode, Mode: 0o444}))(t)
	if err := c.Fsync(obj.ID); err != nil {
		t.Fatal(err)
	}
	// A fresh connection keeps no file: it must open the 0444 files itself.
	fresh := dial(t, f.serve(t, "vm", true))
	for _, id := range []uint64{obj.ID, ro.ID} {
		if a := must(fresh.Write(id, 0, []byte("late")))(t); a.Mode != sIFREG|0o444 {
			t.Fatalf("mode after a write past it = %o", a.Mode)
		}
	}
	if got := must(fresh.Read(ro.ID, 0, 64))(t); string(got) != "late" {
		t.Fatalf("read = %q", got)
	}
}

// TestGetattrLatency measures GETATTR round trips over the Unix socket and
// logs their median, the cost of one uncached stat to the guest.
func TestGetattrLatency(t *testing.T) {
	if testing.Short() {
		t.Skip("measures 10,000 round trips")
	}
	f := newFixture(t, "vm")
	c := dial(t, f.serve(t, "vm", true))
	lib := must(c.Lookup(must(c.Lookup(RootID, "pkg"))(t).ID, "lib.go"))(t)
	const n = 10000
	took := make([]time.Duration, n)
	for i := range took {
		start := time.Now()
		if _, err := c.Getattr(lib.ID); err != nil {
			t.Fatal(err)
		}
		took[i] = time.Since(start)
	}
	slices.Sort(took)
	t.Logf("GETATTR round trip over %d calls: median %v, p90 %v, p99 %v", n, took[n/2], took[n*9/10], took[n*99/100])
}

// TestMutationsRecall checks that the share's changes recall other
// protocols' cached copies of what they change, by file id, as the export's
// Nodes view does for its own changes.
func TestMutationsRecall(t *testing.T) {
	f := newFixture(t, "vm")
	var mu sync.Mutex
	recalled := map[uint64]int{}
	f.e.OnRecall(func(r export.Recall) {
		mu.Lock()
		defer mu.Unlock()
		for id := r.First; id <= r.Last && r.Last-r.First < 16; id++ {
			recalled[id]++
		}
	})
	count := func(id uint64) int {
		mu.Lock()
		defer mu.Unlock()
		return recalled[id]
	}
	c := dial(t, f.serve(t, "vm", true))
	lib := must(c.Lookup(must(c.Lookup(RootID, "pkg"))(t).ID, "lib.go"))(t)
	step := func(what string, id uint64, fn func() error) {
		t.Helper()
		before := count(id)
		if err := fn(); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if count(id) == before {
			t.Fatalf("%s recalled nothing of %d", what, id)
		}
	}
	step("write", lib.ID, func() error { _, err := c.Write(lib.ID, 0, []byte("x")); return err })
	step("setattr", lib.ID, func() error {
		_, err := c.Setattr(lib.ID, SetattrReq{Valid: SetMode, Mode: 0o600})
		return err
	})
	step("link", lib.ID, func() error { _, err := c.Link(lib.ID, RootID, "alias"); return err })
	step("rename", lib.ID, func() error { return c.Rename(RootID, "alias", RootID, "moved", 0) })
	victim := must(c.Create(RootID, "victim", 0o644, CreateExcl))(t)
	step("rename over", victim.ID, func() error { return c.Rename(RootID, "moved", RootID, "victim", 0) })
	step("unlink", lib.ID, func() error { return c.Unlink(RootID, "victim") })
}
