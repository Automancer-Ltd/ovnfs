package overlay

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// memStack opens an overlay with a memory budget over a bottom holding files.
func memStack(t *testing.T, files map[string]string) *FS {
	t.Helper()
	if files == nil {
		files = map[string]string{"keep": "keep"}
	}
	f, err := New(t.TempDir(), nil, dirBottom{tree(t, files)}, NewMemory(1<<20, 64<<10, 0))
	must(t, err)
	return f
}

// crashView opens a second overlay on f's upper with no memory tier, as a
// daemon restart after a crash would see it.
func crashView(t *testing.T, f *FS) *FS {
	t.Helper()
	g, err := New(f.upper, f.layers, f.bottom, nil)
	must(t, err)
	return g
}

// upperFile reads p from f's upper directory on disk.
func upperFile(t *testing.T, f *FS, p string) (string, error) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.upper, p))
	return string(data), err
}

// diskDstMemSrc returns an overlay whose upper holds dst with "old" on disk
// and whose src holds "new" in memory.
func diskDstMemSrc(t *testing.T) *FS {
	t.Helper()
	f := memStack(t, nil)
	write(t, f, "dst", "old", os.O_CREATE|os.O_WRONLY)
	must(t, f.Flush())
	write(t, f, "src", "new", os.O_CREATE|os.O_WRONLY)
	if f.files["src"] == nil || f.files["dst"] != nil {
		t.Fatal("fixture: want src in memory and dst on disk")
	}
	return f
}

// TestRenameMemoryOverDiskKeepsDurableCopy keeps a file at dst on disk when a
// memory file replaces it, so a crash shows the new content, not no file.
func TestRenameMemoryOverDiskKeepsDurableCopy(t *testing.T) {
	f := diskDstMemSrc(t)
	must(t, f.Rename("src", "dst"))
	g := crashView(t, f)
	if got := read(t, g, "dst"); got != "new" {
		t.Fatalf("crash view dst = %q, want new", got)
	}
	assertGone(t, g, "src")
}

// TestRenameMemoryOverDiskSpillFailureKeepsDestination leaves both names as
// they were when the source cannot reach the disk.
func TestRenameMemoryOverDiskSpillFailureKeepsDestination(t *testing.T) {
	f := diskDstMemSrc(t)
	must(t, os.RemoveAll(f.work))
	if err := f.Rename("src", "dst"); err == nil {
		t.Fatal("Rename succeeded with no work directory")
	}
	if got := read(t, f, "dst"); got != "old" {
		t.Fatalf("dst = %q, want old", got)
	}
	if got, err := upperFile(t, f, "dst"); err != nil || got != "old" {
		t.Fatalf("upper dst = %q, %v, want old", got, err)
	}
	if got := read(t, f, "src"); got != "new" {
		t.Fatalf("src = %q, want new", got)
	}
}

// TestRenameMemoryOverDiskWithOpenHandle sends later writes of an open
// handle to the renamed file on disk.
func TestRenameMemoryOverDiskWithOpenHandle(t *testing.T) {
	f := memStack(t, nil)
	write(t, f, "dst", "old", os.O_CREATE|os.O_WRONLY)
	must(t, f.Flush())
	h, err := f.OpenFile("src", os.O_CREATE|os.O_WRONLY, 0o644)
	must(t, err)
	defer h.Close()
	_, err = h.Write([]byte("new"))
	must(t, err)
	must(t, f.Rename("src", "dst"))
	_, err = h.Write([]byte(" more"))
	must(t, err)
	if got, err := upperFile(t, f, "dst"); err != nil || got != "new more" {
		t.Fatalf("upper dst = %q, %v, want %q", got, err, "new more")
	}
}

// TestRenameMemoryOverLowerKeepsMemory keeps a memory source in memory when
// the destination has no copy in the upper.
func TestRenameMemoryOverLowerKeepsMemory(t *testing.T) {
	f := memStack(t, map[string]string{"dst": "lower"})
	write(t, f, "src", "new", os.O_CREATE|os.O_WRONLY)
	must(t, f.Rename("src", "dst"))
	if got := read(t, f, "dst"); got != "new" {
		t.Fatalf("dst = %q, want new", got)
	}
	if !f.Unflushed() {
		t.Fatal("dst spilled, want it in memory")
	}
	for _, p := range []string{"src", "dst"} {
		if _, err := os.Lstat(filepath.Join(f.upper, p)); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("upper %s: %v, want not-exist", p, err)
		}
	}
}

// TestMemoryCreateKeepsWhiteoutUntilSpill keeps the deletion marker of a
// lower file until the memory file that replaces it reaches the disk, so a
// crash shows the name as deleted, not the old lower content.
func TestMemoryCreateKeepsWhiteoutUntilSpill(t *testing.T) {
	f := memStack(t, map[string]string{"f": "lower"})
	must(t, f.Remove("f"))
	write(t, f, "f", "new", os.O_CREATE|os.O_WRONLY)
	if f.files["f"] == nil {
		t.Fatal("fixture: want f in memory")
	}
	assertGone(t, crashView(t, f), "f")
	must(t, f.Flush())
	if got := read(t, crashView(t, f), "f"); got != "new" {
		t.Fatalf("after Flush f = %q, want new", got)
	}
	if _, err := os.Lstat(filepath.Join(f.upper, whPrefix+"f")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("marker after Flush: %v, want not-exist", err)
	}
}

// TestMemoryUnlinkKeepsWhiteout keeps the marker when a memory file over a
// deleted lower file goes away before it spills.
func TestMemoryUnlinkKeepsWhiteout(t *testing.T) {
	f := memStack(t, map[string]string{"f": "lower"})
	must(t, f.Remove("f"))
	write(t, f, "f", "new", os.O_CREATE|os.O_WRONLY)
	must(t, f.Remove("f"))
	assertGone(t, crashView(t, f), "f")
	if _, err := os.Lstat(filepath.Join(f.upper, whPrefix+"f")); err != nil {
		t.Fatalf("marker: %v, want present", err)
	}
}
