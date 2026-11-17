package overlay

import (
	"fmt"
	"io"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// coolIndex drops f's upper index as the registry's idle check does.
func coolIndex(t *testing.T, f *FS) {
	t.Helper()
	f.Cool(time.Now().Add(time.Hour))
	if indexLoaded(f) {
		t.Fatal("upper index stayed loaded")
	}
}

// indexLoaded reports whether f holds its whole upper index.
func indexLoaded(f *FS) bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.index != nil
}

// observe renders what a protocol reads about p through every read path; it
// opens only regular files, which a cold read serves without the index.
func observe(f *FS, p string) string {
	var b strings.Builder
	info, err := f.Lstat(p)
	if err != nil {
		b.WriteString("lstat: missing; ")
	} else {
		fmt.Fprintf(&b, "lstat: %s %v %d %d; ", info.Name(), info.Mode(), info.Size(), info.ModTime().UnixNano())
	}
	regular := err == nil && info.Mode().IsRegular()
	if info, err := f.Stat(p); err != nil {
		b.WriteString("stat: missing; ")
	} else {
		fmt.Fprintf(&b, "stat: %v %d; ", info.Mode(), info.Size())
	}
	if target, err := f.Readlink(p); err == nil {
		fmt.Fprintf(&b, "link: %s; ", target)
	}
	_, upper := f.UpperFile(p)
	fmt.Fprintf(&b, "bottom: %v; upper: %v; ", f.InBottom(p), upper)
	if items, err := f.ReadDir(p); err == nil {
		for _, item := range items {
			fmt.Fprintf(&b, "%s %v %d, ", item.Name(), item.Mode(), item.Size())
		}
	}
	if !regular {
		return b.String()
	}
	if h, err := f.Open(p); err != nil {
		fmt.Fprintf(&b, "open: %v", err)
	} else {
		data, err := io.ReadAll(h)
		fmt.Fprintf(&b, "open: %s %q %v", h.Name(), data, err == nil)
		h.Close()
	}
	return b.String()
}

// TestColdReadsMatchIndexedReads: with the upper index dropped, and on an
// overlay just opened over an existing upper, reads answer as the index does
// across whiteouts, opaque directories, symlinks, renames and hard links.
func TestColdReadsMatchIndexedReads(t *testing.T) {
	f := stackFixture(t)
	must(t, f.MkdirAll("u/a/b", 0o755))
	write(t, f, "u/a/b/c", "deep", os.O_CREATE|os.O_WRONLY)
	write(t, f, "d/top", "rewritten", os.O_TRUNC|os.O_WRONLY)
	must(t, f.Remove("d/base"))
	for _, p := range []string{"d/sub/base", "d/sub/low", "d/sub/mid", "d/sub/top", "d/sub"} {
		must(t, f.Remove(p))
	}
	must(t, f.MkdirAll("d/sub", 0o755))
	write(t, f, "d/sub/fresh", "fresh", os.O_CREATE|os.O_WRONLY)
	must(t, f.Symlink("d", "linkdir"))
	must(t, f.Symlink("d/top", "ln"))
	write(t, f, "file", "plain", os.O_CREATE|os.O_WRONLY)
	must(t, f.Rename("opaque", "moved"))
	must(t, f.Link("u/a/b/c", "hard"))
	paths := []string{"", "d/base", "d/sub/base", "d/gone", "opaque", "opaque/top", "moved/base", "block/child", "deleted",
		"through/base", "linkdir/top", "linkdir/sub/fresh", "ln/x", "file/x", "missing", "missing/x", ".wh.deleted", "/file", "u/a//b/c"}
	var walk func(string)
	walk = func(d string) {
		items, err := f.ReadDir(d)
		must(t, err)
		for _, item := range items {
			paths = append(paths, path.Join(d, item.Name()))
			if item.IsDir() {
				walk(path.Join(d, item.Name()))
			}
		}
	}
	walk("")
	check := func(name string, fs *FS) {
		t.Helper()
		cold := make(map[string]string, len(paths))
		for _, p := range paths {
			cold[p] = observe(fs, p)
		}
		if indexLoaded(fs) {
			t.Fatalf("%s: a read loaded the upper index", name)
		}
		fs.mu.Lock()
		must(t, fs.loadIndex())
		fs.mu.Unlock()
		for _, p := range paths {
			if indexed := observe(fs, p); cold[p] != indexed {
				t.Errorf("%s %q:\n cold    %s\n indexed %s", name, p, cold[p], indexed)
			}
		}
	}
	coolIndex(t, f)
	check("cooled", f)
	// Reopening recreates the staging directory, so it follows the cooled check.
	fresh, err := New(f.upper, f.layers, f.bottom, nil)
	must(t, err)
	check("reopened", fresh)
}

// TestUpperFileWhileIndexCold keeps lease policy for an upper the overlay has
// not indexed since it opened.
func TestUpperFileWhileIndexCold(t *testing.T) {
	upper := tree(t, map[string]string{"mine": "data", "dir/child": "x"})
	f, err := New(upper, nil, dirBottom{tree(t, map[string]string{"lower": "l"})}, nil)
	must(t, err)
	for p, want := range map[string]bool{"mine": true, "dir/child": true, "dir": false, "lower": false, "missing": false, "": false} {
		if _, ok := f.UpperFile(p); ok != want {
			t.Errorf("UpperFile(%q) = %v, want %v", p, ok, want)
		}
	}
}

// TestColdHandlePinsAndBindsIndex: a read opened while the index is cold is
// bound to its inode once a mutation loads the index, and keeps it loaded.
func TestColdHandlePinsAndBindsIndex(t *testing.T) {
	f := setup(t, nil)
	write(t, f, "a", "data", os.O_CREATE|os.O_WRONLY)
	must(t, f.Link("a", "b"))
	coolIndex(t, f)
	h, err := f.Open("a")
	must(t, err)
	write(t, f, "other", "x", os.O_CREATE|os.O_WRONLY)
	f.Cool(time.Now().Add(time.Hour))
	if !indexLoaded(f) {
		t.Fatal("index dropped under an open handle")
	}
	must(t, h.(interface{ Chmod(os.FileMode) error }).Chmod(0o600))
	info, err := f.Lstat("b")
	must(t, err)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("hard link mode after descriptor chmod = %v", info.Mode())
	}
	must(t, h.Close())
	coolIndex(t, f)
}

// TestColdFreezeKeepsRenameHints freezes an upper whose index was dropped.
func TestColdFreezeKeepsRenameHints(t *testing.T) {
	f := setup(t, nil)
	write(t, f, "a", "data", os.O_CREATE|os.O_WRONLY)
	must(t, f.Rename("a", "b"))
	coolIndex(t, f)
	l := freezeForTest(t, f)
	if e, _ := l.Get("b"); e.Origin != "a" {
		t.Fatalf("frozen rename hint = %q", e.Origin)
	}
	assertFrozenIndexMatchesDisk(t, l)
}

// TestColdReadsRaceCooling races reads and writes with idle drops.
func TestColdReadsRaceCooling(t *testing.T) {
	f := stackFixture(t)
	must(t, f.MkdirAll("u/a", 0o755))
	write(t, f, "u/a/file", "seed", os.O_CREATE|os.O_WRONLY)
	var wg sync.WaitGroup
	for g := range 4 {
		wg.Go(func() {
			for i := range 100 {
				switch g {
				case 0:
					f.Cool(time.Now().Add(time.Hour))
				case 1:
					p := fmt.Sprintf("u/w%d", i%5)
					h, err := f.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
					if err == nil {
						err = h.Close()
					}
					if err == nil {
						err = f.Rename(p, p+"r")
					}
					if err != nil {
						t.Error(err)
						return
					}
				default:
					for _, p := range []string{"", "u", "u/a", "u/a/file", "d", "d/top", "opaque", "missing"} {
						f.Lstat(p)
						f.ReadDir(p)
						f.UpperFile(p)
					}
					h, err := f.Open("u/a/file")
					if err != nil {
						t.Error(err)
						return
					}
					data, err := io.ReadAll(h)
					h.Close()
					if err != nil || string(data) != "seed" {
						t.Errorf("read %q, %v", data, err)
						return
					}
				}
			}
		})
	}
	wg.Wait()
}

// TestRestoredLayerReloadsAfterCool decodes a dropped stored-layer index
// again, keeping hard-link identity across decodes.
func TestRestoredLayerReloadsAfterCool(t *testing.T) {
	src := tree(t, map[string]string{"a/x": "x", "b": "b"})
	must(t, os.Link(filepath.Join(src, "a/x"), filepath.Join(src, "hl")))
	l, err := IndexLayer(src)
	must(t, err)
	blobs := memBlobs{"bx": []byte("x"), "bb": []byte("b")}
	must(t, l.Store(map[string]string{"a/x": "bx", "b": "bb"}, "", blobs))
	r, err := RestoreLayer(l.Dir(), l.Metadata(), blobs)
	must(t, err)
	before, _ := r.Get("a/x")
	for range 3 { // the first check sees the Get
		r.Cool(2)
	}
	if r.resident.Load() != nil {
		t.Fatal("idle stored-layer index kept")
	}
	f, err := New(t.TempDir(), []*Layer{r}, nil, nil)
	must(t, err)
	if got := read(t, f, "a/x"); got != "x" {
		t.Fatalf("stored file = %q", got)
	}
	assertNames(t, f, "a", "x")
	x, _ := r.Get("a/x")
	hl, _ := r.Get("hl")
	if !sameEntry(before.Info, x.Info) || !sameEntry(x.Info, hl.Info) {
		t.Fatal("hard links lost their identity across decodes")
	}
	if got := slices.Sorted(maps.Keys(r.Children(""))); !slices.Equal(got, []string{"a", "b", "hl"}) {
		t.Fatalf("children = %v", got)
	}
}
