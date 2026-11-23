package checkout

import (
	"io"
	"maps"
	"path"
	"slices"
	"strings"
	"testing"

	"github.com/stencil-hq/ovnfs/overlay"
	"github.com/stencil-hq/ovnfs/store"
)

// emptyDir marks an empty-directory leaf in the expected leaves of a fold.
const emptyDir = "<dir>"

// foldRedirects folds layers built by specLayer (oldest first) over the
// bench's parent with the snapshot hash and with the read-only tree of diff,
// plan and attach. It checks that both give the same leaves, the leaves the
// merged overlay view shows outside non-source paths (names build,
// node_modules, *.tmp, and the paths in skip), and that diff reads each leaf's
// content from where the tree says. The snapshot becomes the next parent; the
// leaves are returned as path → content, emptyDir for empty directories.
func (hb *hashBench) foldRedirects(name string, skip []string, specs ...map[string]string) map[string]string {
	t := hb.t
	t.Helper()
	layers := make([]*overlay.Layer, len(specs))
	for i, s := range specs {
		d := t.TempDir()
		specLayer(t, d, s)
		l, err := overlay.IndexLayer(d)
		if err != nil {
			t.Fatal(err)
		}
		layers[i] = l
		hb.stack = append([]*overlay.Layer{l}, hb.stack...)
	}
	view, err := overlay.New("", hb.stack, hb.b.view.Bottom(nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	root, _, err := hashSnapshot(hashJob{Base: hb.b, Parent: hb.parent, Layers: layers, View: view})
	if err != nil {
		t.Fatalf("%s: snapshot hash: %v", name, err)
	}
	snapped, err := hb.st.Flatten(root)
	if err != nil {
		t.Fatal(err)
	}
	tr, err := newTree(hb.b, nil, hb.parent, overlayView{view})
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range layers {
		if err := tr.apply(l); err != nil {
			t.Fatalf("%s: tree: %v", name, err)
		}
	}
	files := tr.finish()
	if !maps.Equal(snapped, files) {
		t.Fatalf("%s: snapshot leaves %v, tree leaves %v", name, snapped, files)
	}
	shown := mergedLeaves(t, view, skip)
	got := make(map[string]string, len(files))
	for p, f := range files {
		if f.Mode == store.ModeDir {
			got[p] = emptyDir
			continue
		}
		data, err := (&Registry{}).content(&checkoutRecord{base: hb.b}, f, tr.supplier(p))
		if err != nil {
			t.Fatalf("%s: content of %s from %+v: %v", name, p, tr.supplier(p), err)
		}
		if store.HashBytes("blob", data) != f.ID {
			t.Fatalf("%s: content of %s from %+v is %q, not its blob %s", name, p, tr.supplier(p), data, f.ID)
		}
		got[p] = string(data)
	}
	if !maps.Equal(got, shown) {
		t.Fatalf("%s: folded leaves %v, merged view shows %v", name, got, shown)
	}
	hb.parent = root
	return got
}

// mergedLeaves lists the regular files (with content) and empty directories
// the merged view shows, leaving out paths below the test rules' non-source
// names, excluded names and skip.
func mergedLeaves(t *testing.T, view *overlay.FS, skip []string) map[string]string {
	t.Helper()
	leaves := map[string]string{}
	var walk func(dir string)
	walk = func(dir string) {
		items, err := view.ReadDir(dir)
		if err != nil {
			t.Fatalf("list %q: %v", dir, err)
		}
		if len(items) == 0 && dir != "" {
			leaves[dir] = emptyDir
		}
		for _, info := range items {
			p := path.Join(dir, info.Name())
			if n := info.Name(); n == "build" || n == "node_modules" || strings.HasSuffix(n, ".tmp") || hashExcluded(p) || slices.Contains(skip, p) {
				continue
			}
			if info.IsDir() {
				walk(p)
				continue
			}
			f, err := view.Open(p)
			if err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(f)
			f.Close()
			if err != nil {
				t.Fatal(err)
			}
			leaves[p] = string(data)
		}
	}
	walk("")
	return leaves
}

// baseLeaves are the newHashBench capture's source leaves.
func baseLeaves(extra map[string]string, drop ...string) map[string]string {
	want := map[string]string{"a/x": "1", "a/b/y": "2", "a/empty": emptyDir, "b": "3", "c/d/e": "4", "keep": emptyDir}
	for _, p := range drop {
		for q := range want {
			if q == p || strings.HasPrefix(q, p+"/") {
				delete(want, q)
			}
		}
	}
	maps.Copy(want, extra)
	return want
}

// TestFoldRedirectedDirectories checks that snapshot hashes and the trees of
// diff, plan and attach show a redirected directory as the merged view does:
// its origin's leaves at the new name, under the layer's own entries there.
func TestFoldRedirectedDirectories(t *testing.T) {
	for _, c := range []struct {
		name   string
		skip   []string
		rounds [][]map[string]string // snapshots, each of layers oldest first
		want   map[string]string
	}{{
		name:   "base directories move",
		rounds: [][]map[string]string{{{"a2": "redirect:a", "a": "wh", "k2": "redirect:keep", "keep": "wh"}}},
		want:   baseLeaves(map[string]string{"a2/x": "1", "a2/b/y": "2", "a2/empty": emptyDir, "k2": emptyDir}, "a", "keep"),
	}, {
		name:   "nested redirect",
		rounds: [][]map[string]string{{{"n": "redirect:a", "n/b": "redirect:c/d", "a": "wh", "c": "wh"}}},
		want:   baseLeaves(map[string]string{"n/x": "1", "n/b/e": "4", "n/empty": emptyDir}, "a", "c"),
	}, {
		name:   "the layer's entries below a redirect",
		rounds: [][]map[string]string{{{"a2": "redirect:a", "a": "wh", "a2/x": "file:new", "a2/b/y": "wh", "a2/z": "file:z"}}},
		want:   baseLeaves(map[string]string{"a2/x": "new", "a2/z": "z", "a2/b": emptyDir, "a2/empty": emptyDir}, "a"),
	}, {
		name: "origin from a deeper layer",
		rounds: [][]map[string]string{{
			{"p/q": "file:deep", "a/x": "file:one"},
			{"p2": "redirect:p", "p": "wh", "a/b2": "redirect:a/b", "a/b": "wh"},
		}},
		want: baseLeaves(map[string]string{"p2/q": "deep", "a/x": "one", "a/b2/y": "2"}, "a/b"),
	}, {
		name: "moves across snapshots",
		rounds: [][]map[string]string{
			{{"m": "redirect:a", "a": "wh", "m/b/w": "file:w"}},
			{{"m2": "redirect:m", "m": "wh", "m2/x": "file:edited"}},
		},
		want: baseLeaves(map[string]string{"m2/x": "edited", "m2/b/y": "2", "m2/b/w": "w", "m2/empty": emptyDir}, "a"),
	}, {
		name:   "directories swap",
		rounds: [][]map[string]string{{{"a": "redirect:c", "c": "redirect:a"}}},
		want:   baseLeaves(map[string]string{"a/d/e": "4", "c/x": "1", "c/b/y": "2", "c/empty": emptyDir}, "a", "c"),
	}, {
		name:   "redirect without a whiteout copies",
		rounds: [][]map[string]string{{{"a2": "redirect:a"}}},
		want:   baseLeaves(map[string]string{"a2/x": "1", "a2/b/y": "2", "a2/empty": emptyDir}),
	}, {
		name:   "a moved cache directory stays cache",
		skip:   []string{"out"},
		rounds: [][]map[string]string{{{"out": "redirect:build", "build": "wh"}}},
		want:   baseLeaves(nil),
	}, {
		name:   "a source directory moved to a cache name",
		rounds: [][]map[string]string{{{"a/build": "redirect:c", "c": "wh"}}},
		want:   baseLeaves(nil, "c"),
	}} {
		t.Run(c.name, func(t *testing.T) {
			hb := newHashBench(t)
			var got map[string]string
			for i, layers := range c.rounds {
				got = hb.foldRedirects(c.name+" round "+string(rune('0'+i)), c.skip, layers...)
			}
			if !maps.Equal(got, c.want) {
				t.Fatalf("leaves %v, want %v", got, c.want)
			}
		})
	}
}

// TestCacheOnlyRedirects checks attach's test for a checkout holding only
// cache changes: a moved source directory is a change, a moved cache directory
// is not.
func TestCacheOnlyRedirects(t *testing.T) {
	hb := newHashBench(t)
	for _, c := range []struct {
		name  string
		layer map[string]string
		clean bool
	}{
		{"nested cache files", map[string]string{"a/build/f": "file:x"}, true},
		{"moved cache directory", map[string]string{"out": "redirect:build", "build": "wh"}, true},
		{"moved source directory", map[string]string{"a2": "redirect:a", "a": "wh"}, false},
		{"redirected directory holding cache files", map[string]string{"a3": "redirect:a/b", "a3/build/f": "file:x"}, false},
	} {
		d := t.TempDir()
		specLayer(t, d, c.layer)
		l, err := overlay.IndexLayer(d)
		if err != nil {
			t.Fatal(err)
		}
		upper := t.TempDir()
		fs, err := overlay.New(upper, []*overlay.Layer{l}, hb.b.view.Bottom(nil), nil)
		if err != nil {
			t.Fatal(err)
		}
		clean, err := (&Registry{}).cacheOnlyLocked(&checkoutRecord{base: hb.b, upper: upper, fs: fs})
		if err != nil || clean != c.clean {
			t.Errorf("%s: cache only %v %v, want %v", c.name, clean, err, c.clean)
		}
	}
}
