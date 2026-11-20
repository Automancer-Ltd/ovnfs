//go:build darwin || linux

package overlay

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stencil-hq/ovnfs/internal/ident"
)

// links describes the names of one file as the overlay shows them: they
// must share an identity and show len(names) links.
func links(t *testing.T, f *FS, names ...string) ident.ID {
	t.Helper()
	var id ident.ID
	for _, p := range names {
		info, err := f.Lstat(p)
		must(t, err)
		got, _ := ident.Of(info)
		if id == 0 {
			id = got
		}
		if got != id || hardLinks(info) != uint64(len(names)) {
			t.Fatalf("%s: identity %v (want %v), %d links (want %d)", p, got, id, hardLinks(info), len(names))
		}
	}
	return id
}

// onDisk fails unless the upper holds names as one inode with that many
// links and contents want.
func onDisk(t *testing.T, upper, want string, names ...string) {
	t.Helper()
	var first os.FileInfo
	for _, p := range names {
		info, err := os.Lstat(filepath.Join(upper, p))
		must(t, err)
		if first == nil {
			first = info
		} else if !os.SameFile(first, info) {
			t.Fatalf("%s is not the inode of %s", p, names[0])
		}
		if n := info.Sys().(*syscall.Stat_t).Nlink; uint64(n) != uint64(len(names)) {
			t.Fatalf("%s has %d links on disk, want %d", p, n, len(names))
		}
		data, err := os.ReadFile(filepath.Join(upper, p))
		must(t, err)
		if string(data) != want {
			t.Fatalf("%s holds %q on disk, want %q", p, data, want)
		}
	}
}

// absentOnDisk fails when the upper holds any of names.
func absentOnDisk(t *testing.T, upper string, names ...string) {
	t.Helper()
	for _, p := range names {
		if _, err := os.Lstat(filepath.Join(upper, p)); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%s is on disk: %v", p, err)
		}
	}
}

// TestMemoryHardLinks links a memory file in memory and checks that its
// names share everything, survive renames, renames over them, directory
// renames above them, deletion markers and unlinks with open handles, and
// reach disk as one inode.
func TestMemoryHardLinks(t *testing.T) {
	low := t.TempDir()
	must(t, os.WriteFile(filepath.Join(low, "x"), []byte("lower"), 0o644))
	upper := t.TempDir()
	f, err := New(upper, nil, dirBottom{low}, NewMemory(64<<20, 256<<10, 0))
	must(t, err)
	must(t, f.MkdirAll("d", 0o755))
	f.Settle()
	must(t, writeFile(f, "a", []byte("hello"), os.O_WRONLY|os.O_CREATE|os.O_TRUNC))
	must(t, f.Link("a", "d/b"))
	absentOnDisk(t, upper, "a", "d/b")
	id := links(t, f, "a", "d/b")

	w, err := f.OpenFile("d/b", os.O_RDWR, 0)
	must(t, err)
	_, err = w.Write([]byte("J"))
	must(t, err)
	must(t, w.Close())
	if got := read(t, f, "a"); got != "Jello" {
		t.Fatalf("a reads %q after a write through d/b", got)
	}
	must(t, f.Chmod("a", 0o600))
	if info, _ := f.Lstat("d/b"); info.Mode().Perm() != 0o600 {
		t.Fatalf("d/b has mode %v after a chmod of a", info.Mode())
	}

	// A name over a lower file keeps its deletion marker until it spills.
	must(t, f.Remove("x"))
	must(t, f.Link("a", "x"))
	links(t, f, "a", "d/b", "x")
	must(t, f.Rename("d/b", "d/c"))
	must(t, f.Rename("d", "e"))
	links(t, f, "a", "e/c", "x")
	must(t, writeFile(f, "o", []byte("other"), os.O_WRONLY|os.O_CREATE|os.O_TRUNC))
	must(t, f.Rename("o", "a"))
	if got := links(t, f, "e/c", "x"); got != id {
		t.Fatalf("identity %v after a rename over a name, want %v", got, id)
	}
	must(t, f.Rename("x", "e/c")) // two names of one file: nothing to do
	links(t, f, "e/c", "x")
	if p, ok := f.Locate(id); !ok || p != "e/c" && p != "x" {
		t.Fatalf("Locate = %q, %v", p, ok)
	}

	r, err := f.Open("x")
	must(t, err)
	must(t, f.Remove("e/c"))
	links(t, f, "x")
	buf := make([]byte, 5)
	if _, err := r.Read(buf); err != nil || string(buf) != "Jello" {
		t.Fatalf("open handle reads %q, %v", buf, err)
	}
	must(t, r.Close())
	must(t, f.Link("x", "e/y"))
	absentOnDisk(t, upper, "x", "e/y")

	must(t, f.Flush())
	onDisk(t, upper, "Jello", "x", "e/y")
	onDisk(t, upper, "other", "a")
	if _, err := os.Lstat(f.whiteoutPath("x")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the deletion marker of x stayed: %v", err)
	}
	g, err := New(upper, nil, dirBottom{low}, nil)
	must(t, err)
	if got := links(t, g, "x", "e/y"); got != id {
		t.Fatalf("reopened, the names show identity %v, want %v", got, id)
	}
	if got := read(t, g, "x"); got != "Jello" {
		t.Fatalf("reopened, x reads %q", got)
	}
}

// TestMemoryHardLinksSpill moves a file with several names to disk by
// eviction and checks the names end up as one inode.
func TestMemoryHardLinksSpill(t *testing.T) {
	upper := t.TempDir()
	f, err := New(upper, nil, nil, NewMemory(8<<10, 4<<10, 0))
	must(t, err)
	must(t, f.MkdirAll("d", 0o755))
	must(t, writeFile(f, "a", []byte("first"), os.O_WRONLY|os.O_CREATE|os.O_TRUNC))
	must(t, f.Link("a", "d/b"))
	must(t, f.Link("a", "c"))
	for _, p := range []string{"f1", "f2", "f3", "f4"} {
		must(t, writeFile(f, p, make([]byte, 3<<10), os.O_WRONLY|os.O_CREATE|os.O_TRUNC))
	}
	f.Settle()
	onDisk(t, upper, "first", "a", "d/b", "c")
	links(t, f, "a", "d/b", "c")
}

// TestMemoryHardLinksCrash spills a file with several names while the
// queue stands still and checks what a crash then leaves: the first name
// with all its contents and no other copy; the others once the queue
// moves.
func TestMemoryHardLinksCrash(t *testing.T) {
	upper := t.TempDir()
	f, err := New(upper, nil, nil, NewMemory(64<<20, 256<<10, 0))
	must(t, err)
	must(t, f.MkdirAll("d", 0o755))
	f.Settle()
	must(t, writeFile(f, "a", []byte("contents"), os.O_WRONLY|os.O_CREATE|os.O_TRUNC))
	must(t, f.Link("a", "b"))
	must(t, f.Link("a", "d/c"))
	id := links(t, f, "a", "b", "d/c")

	release := holdApplier(f)
	m := f.files["a"]
	m.promote("a")
	must(t, f.spill(m))
	crashed := filepath.Join(t.TempDir(), "crashed")
	copyTree(t, upper, crashed)
	release()

	onDisk(t, crashed, "contents", "a")
	absentOnDisk(t, crashed, "b", "d/c")
	g, err := New(crashed, nil, nil, nil)
	must(t, err)
	if got := links(t, g, "a"); got != id {
		t.Fatalf("after the crash, a has identity %v, want %v", got, id)
	}
	if got := read(t, g, "a"); got != "contents" {
		t.Fatalf("after the crash, a reads %q", got)
	}
	f.Settle()
	onDisk(t, upper, "contents", "a", "b", "d/c")
}
