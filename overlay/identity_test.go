package overlay

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stencil-hq/ovnfs/internal/ident"
)

// idOf returns the identity the overlay reports for p, failing the test
// without one.
func idOf(t *testing.T, f *FS, p string) ident.ID {
	t.Helper()
	info, err := f.Lstat(p)
	must(t, err)
	id, ok := ident.Of(info)
	if !ok {
		t.Fatalf("%s has no identity", p)
	}
	return id
}

// assertLocated checks that Locate finds id at want ("" with found false:
// nowhere).
func assertLocated(t *testing.T, f *FS, id ident.ID, want string, found bool) {
	t.Helper()
	if got, ok := f.Locate(id); ok != found || got != want {
		t.Fatalf("Locate(%#x) = %q, %v; want %q, %v", id, got, ok, want, found)
	}
}

// memories runs a test without and with memory files.
func memories(t *testing.T, test func(t *testing.T, mem func() *Memory)) {
	for _, inMemory := range []bool{false, true} {
		t.Run(fmt.Sprintf("memory=%v", inMemory), func(t *testing.T) {
			test(t, func() *Memory {
				if !inMemory {
					return nil
				}
				return NewMemory(1<<20, 64<<10, 0)
			})
		})
	}
}

// TestCopyUpKeepsIdentity checks that a lower entry keeps its identity when
// a write-open, a truncating open, chmod or rename copies it up, and when its
// lower parent directory is renamed; that Locate follows it; and that a
// reopened overlay reports the same identities.
func TestCopyUpKeepsIdentity(t *testing.T) {
	memories(t, func(t *testing.T, mem func() *Memory) {
		lower := tree(t, map[string]string{"d/append": "a", "d/trunc": "t", "d/chmod": "c", "d/move": "m", "d/sub/x": "x"})
		upper := filepath.Join(t.TempDir(), "upper")
		f, err := New(upper, nil, dirBottom{lower}, mem())
		must(t, err)
		before := map[string]ident.ID{}
		for _, p := range []string{"", "d", "d/append", "d/trunc", "d/chmod", "d/move", "d/sub", "d/sub/x"} {
			before[p] = idOf(t, f, p)
		}
		listed, err := f.ReadDir("d")
		must(t, err)
		for _, info := range listed {
			if id, _ := ident.Of(info); id != before["d/"+info.Name()] {
				t.Fatalf("listed %s as %#x, looked up as %#x", info.Name(), id, before["d/"+info.Name()])
			}
		}
		assertLocated(t, f, before[""], "", true)

		write(t, f, "d/append", "+", os.O_WRONLY|os.O_APPEND)
		write(t, f, "d/trunc", "new", os.O_WRONLY|os.O_TRUNC)
		must(t, f.Chmod("d/chmod", 0o600))
		must(t, f.Rename("d/move", "d/moved"))
		must(t, f.Rename("d/sub", "renamed"))
		after := map[string]string{"": "", "d": "d", "d/append": "d/append", "d/trunc": "d/trunc", "d/chmod": "d/chmod", "d/move": "d/moved", "d/sub": "renamed", "d/sub/x": "renamed/x"}
		check := func(f *FS) {
			t.Helper()
			for was, now := range after {
				if id := idOf(t, f, now); id != before[was] {
					t.Fatalf("%s (was %s) has identity %#x, had %#x", now, was, id, before[was])
				}
				assertLocated(t, f, before[was], now, true)
			}
		}
		check(f)
		if got := read(t, f, "d/append"); got != "a+" {
			t.Fatalf("d/append reads %q", got)
		}

		must(t, f.Flush())
		reopened, err := New(upper, nil, dirBottom{lower}, mem())
		must(t, err)
		check(reopened)

		must(t, reopened.Remove("renamed/x"))
		assertLocated(t, reopened, before["d/sub/x"], "", false)
		must(t, reopened.Remove("d/moved"))
		assertLocated(t, reopened, before["d/move"], "", false)
	})
}

// TestMemoryFileKeepsIdentity checks that a new memory file keeps its
// Synthetic identity through a rename and its spill, after a reopen, and
// that a reopened overlay never hands it out again.
func TestMemoryFileKeepsIdentity(t *testing.T) {
	upper := filepath.Join(t.TempDir(), "upper")
	f, err := New(upper, nil, dirBottom{t.TempDir()}, NewMemory(1<<20, 64<<10, 0))
	must(t, err)
	write(t, f, "new", "data", os.O_WRONLY|os.O_CREATE)
	if f.files["new"] == nil {
		t.Fatal("new file is not in memory")
	}
	id := idOf(t, f, "new")
	if id.Space() != ident.Synthetic {
		t.Fatalf("memory file identity %#x is not synthetic", id)
	}
	must(t, f.Rename("new", "renamed"))
	if got := idOf(t, f, "renamed"); got != id {
		t.Fatalf("renamed memory file has identity %#x, had %#x", got, id)
	}
	assertLocated(t, f, id, "renamed", true)
	h, err := f.Open("renamed")
	must(t, err)
	must(t, f.Flush())
	if f.files["renamed"] != nil {
		t.Fatal("flush left the file in memory")
	}
	if got := idOf(t, f, "renamed"); got != id {
		t.Fatalf("spilled file has identity %#x, had %#x", got, id)
	}
	if got := h.(ident.Identifier).Identity(); got != id {
		t.Fatalf("handle identity %#x, file %#x", got, id)
	}
	must(t, h.Close())
	assertLocated(t, f, id, "renamed", true)

	reopened, err := New(upper, nil, dirBottom{t.TempDir()}, NewMemory(1<<20, 64<<10, 0))
	must(t, err)
	if got := idOf(t, reopened, "renamed"); got != id {
		t.Fatalf("reopened file has identity %#x, had %#x", got, id)
	}
	assertLocated(t, reopened, id, "renamed", true)
	write(t, reopened, "next", "x", os.O_WRONLY|os.O_CREATE)
	if next := idOf(t, reopened, "next"); next.Space() != ident.Synthetic || next.Value() <= id.Value() {
		t.Fatalf("reopened overlay allocated %#x after %#x", next, id)
	}
}

// TestIdentitySurvivesFreezeAndStore checks that identities of copies, new
// files, directories and spilled memory files survive freezing the upper,
// storing the layer, restoring it, and a later copy-up and rename from the
// stored layer, and that Locate finds them at every step.
func TestIdentitySurvivesFreezeAndStore(t *testing.T) {
	memories(t, func(t *testing.T, mem func() *Memory) {
		bottom := dirBottom{tree(t, map[string]string{"lower": "l", "dir/kept": "k"})}
		f, err := New(filepath.Join(t.TempDir(), "upper"), nil, bottom, mem())
		must(t, err)
		write(t, f, "lower", "+", os.O_WRONLY|os.O_APPEND)
		write(t, f, "dir/new", "n", os.O_WRONLY|os.O_CREATE)
		must(t, f.MkdirAll("made/deep", 0o755))
		paths := []string{"", "lower", "dir", "dir/kept", "dir/new", "made", "made/deep"}
		want := map[string]ident.ID{}
		for _, p := range paths {
			want[p] = idOf(t, f, p)
		}
		must(t, f.Flush())
		l := freezeForTest(t, f)
		check := func(f *FS) {
			t.Helper()
			for _, p := range paths {
				if got := idOf(t, f, p); got != want[p] {
					t.Fatalf("%s has identity %#x, had %#x", p, got, want[p])
				}
				assertLocated(t, f, want[p], p, true)
			}
		}
		check(must2(t)(New("", []*Layer{l}, bottom, nil)))
		reindexed, err := OpenLayer(l.Dir(), nil)
		must(t, err)
		check(must2(t)(New("", []*Layer{reindexed}, bottom, nil)))

		blobs := memBlobs{"bl": []byte("l+"), "bn": []byte("n")}
		must(t, l.Store(map[string]string{"lower": "bl", "dir/new": "bn"}, "", blobs))
		check(must2(t)(New("", []*Layer{l}, bottom, nil)))
		restored, err := RestoreLayer(l.Dir(), l.Metadata(), blobs)
		must(t, err)
		next, err := New(filepath.Join(t.TempDir(), "upper"), []*Layer{restored}, bottom, mem())
		must(t, err)
		check(next)
		write(t, next, "lower", "!", os.O_WRONLY|os.O_APPEND)
		must(t, next.Rename("dir/new", "made/new"))
		if got := read(t, next, "lower"); got != "l+!" {
			t.Fatalf("lower reads %q", got)
		}
		for p, now := range map[string]string{"lower": "lower", "dir/new": "made/new"} {
			if got := idOf(t, next, now); got != want[p] {
				t.Fatalf("%s copied up from the stored layer has identity %#x, had %#x", now, got, want[p])
			}
			assertLocated(t, next, want[p], now, true)
		}
	})
}

// must2 fails the test on an error of a two-value call and returns its value.
func must2(t *testing.T) func(*FS, error) *FS {
	return func(f *FS, err error) *FS {
		t.Helper()
		must(t, err)
		return f
	}
}

// TestLegacyManifestIdentities checks that a version 1 manifest, which
// recorded no identities, gives each entry the identity of the inode its
// native stat recorded, as its raw layer did.
func TestLegacyManifestIdentities(t *testing.T) {
	src := tree(t, map[string]string{"a": "a", "d/b": "b"})
	must(t, os.Link(filepath.Join(src, "a"), filepath.Join(src, "hl")))
	l, err := IndexLayer(src)
	must(t, err)
	m, err := l.manifest(nil, "")
	must(t, err)
	// A version 1 manifest named a hard-link group by its first entry.
	m.Version = 1
	first, linked := map[uint64]int{}, map[uint64]bool{}
	for i, e := range m.Entries {
		if _, seen := first[e.ID]; seen {
			linked[e.ID] = true
		} else {
			first[e.ID] = i
		}
	}
	for i := range m.Entries {
		if id := m.Entries[i].ID; linked[id] {
			m.Entries[i].Link = first[id] + 1
		}
		m.Entries[i].ID = 0
	}
	data, err := encodeManifest(m)
	must(t, err)
	restored, err := RestoreLayer(src, data, memBlobs{})
	must(t, err)
	for _, p := range []string{"", "a", "d", "d/b"} {
		raw, _ := l.Get(p)
		got, _ := restored.Get(p)
		id, ok := ident.Of(got.Info)
		if !ok {
			t.Fatalf("%s has no identity", p)
		}
		if runtime.GOOS == "windows" {
			if id.Space() != ident.Synthetic {
				t.Fatalf("%s has identity %#x", p, id)
			}
			continue
		}
		if rawID, _ := ident.Of(raw.Info); id != rawID {
			t.Fatalf("%s has identity %#x, its raw file %#x", p, id, rawID)
		}
	}
	a, _ := restored.Get("a")
	hl, _ := restored.Get("hl")
	if !sameEntry(a.Info, hl.Info) {
		t.Fatal("hard links lost their shared identity")
	}
}

// TestHardLinkCopyLeavesIdentity checks that copying up one name of a hard
// link of a frozen layer file gives the copy its own identity, which the
// other name keeps.
func TestHardLinkCopyLeavesIdentity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows file information has no link count")
	}
	src := tree(t, map[string]string{"a": "a"})
	must(t, os.Link(filepath.Join(src, "a"), filepath.Join(src, "b")))
	l, err := IndexLayer(src)
	must(t, err)
	f, err := New(filepath.Join(t.TempDir(), "upper"), []*Layer{l}, nil, nil)
	must(t, err)
	shared := idOf(t, f, "b")
	write(t, f, "a", "+", os.O_WRONLY|os.O_APPEND)
	if id := idOf(t, f, "a"); id == shared {
		t.Fatal("the copy kept the identity its hard link still has")
	}
	if id := idOf(t, f, "b"); id != shared {
		t.Fatalf("b has identity %#x, had %#x", id, shared)
	}
	assertLocated(t, f, shared, "b", true)
}

// imitateReuse makes overlays opened during the test treat their volume as
// one that reuses inode numbers (ext4, NTFS) and whose symlinks cannot hold
// attributes (Linux), and makes a host file whose base name keys reported
// the inode the map gives, as if the host had handed out that number again.
func imitateReuse(t *testing.T, reported map[string]inodeKey) {
	inode, hold, reuse := inodeOf, canHoldAttr, volumeReusesInodes
	t.Cleanup(func() { inodeOf, canHoldAttr, volumeReusesInodes = inode, hold, reuse })
	volumeReusesInodes = func(string) (bool, error) { return true, nil }
	canHoldAttr = func(mode os.FileMode) bool { return mode.IsRegular() || mode.IsDir() }
	inodeOf = func(name string, info os.FileInfo) (inodeKey, error) {
		if key, ok := reported[filepath.Base(name)]; ok {
			return key, nil
		}
		return fileKey(name, info)
	}
}

// keyOf returns the backing inode the overlay reports for p.
func keyOf(t *testing.T, f *FS, p string) inodeKey {
	t.Helper()
	info, err := f.Lstat(p)
	must(t, err)
	key, ok := hostKey(info)
	if !ok {
		t.Fatalf("%s has no backing inode", p)
	}
	return key
}

// TestReusedInodeGetsNewIdentity removes a file and a symlink while their
// identities are held, then creates entries the host gives the same inode
// numbers: the new entries must have identities of their own, now and after
// a reopen, and the held ones must resolve to nothing.
func TestReusedInodeGetsNewIdentity(t *testing.T) {
	reported := map[string]inodeKey{}
	imitateReuse(t, reported)
	upper := filepath.Join(t.TempDir(), "upper")
	must(t, os.MkdirAll(upper, 0o755))
	must(t, os.WriteFile(filepath.Join(upper, "legacy"), []byte("from before"), 0o644))
	f, err := New(upper, nil, dirBottom{t.TempDir()}, nil)
	must(t, err)
	if id := idOf(t, f, "legacy"); id.Space() != ident.Synthetic {
		t.Fatalf("an entry without identity got %#x", id)
	}
	links := runtime.GOOS != "windows"   // creating symlinks may need a privilege there
	names := map[string]string{"a": "b"} // removed → created with its inode
	write(t, f, "a", "old", os.O_WRONLY|os.O_CREATE)
	if links {
		must(t, f.Symlink("target", "s"))
		names["s"] = "t"
	}
	held := map[string]ident.ID{}
	for old, fresh := range names {
		held[old] = idOf(t, f, old)
		if held[old].Space() != ident.Synthetic {
			t.Fatalf("%s has identity %#x, not one of its own", old, held[old])
		}
		reported[fresh] = keyOf(t, f, old)
		must(t, f.Remove(old))
	}
	write(t, f, "b", "new", os.O_WRONLY|os.O_CREATE)
	if links {
		must(t, f.Symlink("target", "t"))
	}
	ids := map[string]ident.ID{}
	check := func(f *FS) {
		t.Helper()
		for old, fresh := range names {
			if got := keyOf(t, f, fresh); got != reported[fresh] {
				t.Fatalf("%s reports inode %v, not the reused %v", fresh, got, reported[fresh])
			}
			id := idOf(t, f, fresh)
			if id == held[old] {
				t.Fatalf("%s took the identity %#x of removed %s", fresh, id, old)
			}
			if ids[fresh] != 0 && id != ids[fresh] {
				t.Fatalf("%s has identity %#x, had %#x", fresh, id, ids[fresh])
			}
			ids[fresh] = id
			assertLocated(t, f, held[old], "", false)
			assertLocated(t, f, id, fresh, true)
		}
	}
	check(f)
	reopened, err := New(upper, nil, dirBottom{t.TempDir()}, nil)
	must(t, err)
	check(reopened)
	write(t, reopened, "c", "c", os.O_WRONLY|os.O_CREATE) // loads the index, pruning the table
	check(reopened)
}
