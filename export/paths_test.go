package export

import (
	"io"
	"os"
	"testing"

	"github.com/stencil-hq/ovnfs/checkout"
)

// TestInvalidateDropsCachedPaths: a change the daemon makes behind the
// overlay reaches the export only as Invalidate, after which no id
// resolves to the path cached before it.
func TestInvalidateDropsCachedPaths(t *testing.T) {
	e := newTestExport(t)
	nodes := e.Nodes()
	id := nodes.ID("co/source")
	if p, ok := nodes.Path(id); !ok || p != "co/source" {
		t.Fatalf("id resolves to %q (%v)", p, ok)
	}
	// Stand in for a resolution that a daemon-side change made wrong: the
	// overlay's namespace generation cannot see such a change.
	var s checkout.Session
	_, life, err := e.rt.reg.EnterSlot(slotOf(t, e, "co"), &s)
	if err != nil {
		t.Fatal(err)
	}
	e.rt.paths.put(id, "co/elsewhere", s.FS, life, s.FS.Names(), e.rt.paths.invalid.Load())
	s.Done()
	if p, _ := nodes.Path(id); p != "co/elsewhere" {
		t.Fatalf("id resolves to %q, not the cached path", p)
	}
	e.Invalidate("co", []string{"source"}, nil)
	if p, ok := nodes.Path(id); !ok || p != "co/source" {
		t.Fatalf("after Invalidate, id resolves to %q (%v), want co/source", p, ok)
	}
}

// TestNodesFollowTheirEntry caches a resolution of an id to a path another
// file holds now, as one made just before a rename can be. The id-addressed
// frontends describe and open entries through Describe and OpenID, which
// must reach the id's own file, never the one at the cached path.
func TestNodesFollowTheirEntry(t *testing.T) {
	e := newTestExport(t)
	nodes := e.Nodes()
	f, err := nodes.OpenFile("co/other", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("other!")); err != nil {
		t.Fatal(err)
	}
	f.Close()
	id := nodes.ID("co/source")
	var s checkout.Session
	_, life, err := e.rt.reg.EnterSlot(slotOf(t, e, "co"), &s)
	if err != nil {
		t.Fatal(err)
	}
	e.rt.paths.put(id, "co/other", s.FS, life, s.FS.Names(), e.rt.paths.invalid.Load())
	s.Done()

	if p, info, err := nodes.Describe(id); err != nil || p != "co/source" || info.Size() != int64(len("base")) {
		t.Fatalf("Describe: %q (%v), want co/source", p, err)
	}
	f, p, _, err := nodes.OpenID(id, os.O_RDONLY)
	if err != nil || p != "co/source" {
		t.Fatalf("OpenID: %q (%v), want co/source", p, err)
	}
	defer f.Close()
	if data, err := io.ReadAll(f); err != nil || string(data) != "base" {
		t.Fatalf("OpenID read %q (%v), want the id's own bytes", data, err)
	}
}

// slotOf returns checkout name's export slot.
func slotOf(t testing.TB, e *Export, name string) uint16 {
	t.Helper()
	slot, _, ok := e.rt.reg.Slot(name)
	if !ok {
		t.Fatalf("no checkout %s", name)
	}
	return slot
}

// TestRequestIDsDoNotAllocate keeps the ids every protocol request derives
// from file information it has, and repeated resolutions, off the heap.
func TestRequestIDsDoNotAllocate(t *testing.T) {
	e := newTestExport(t)
	nodes := e.Nodes()
	info, err := nodes.Lstat("co/source")
	if err != nil {
		t.Fatal(err)
	}
	id := nodes.IDOf("co/source", info)
	nodes.Path(id)
	for name, fn := range map[string]func(){
		"IDOf":    func() { nodes.IDOf("co/source", info) },
		"ChildID": func() { nodes.ChildID("co", info) },
		"Path":    func() { nodes.Path(id) },
	} {
		if n := testing.AllocsPerRun(100, fn); n != 0 {
			t.Errorf("%s allocates %v times", name, n)
		}
	}
}

// BenchmarkPath resolves the id of a base file, as every NFS request does
// for the file or directory it names: "hit" again and again,
// as a listing's lookups resolve their directory, "miss" after a change
// each time, which is what every resolution cost without the cache.
func BenchmarkPath(b *testing.B) {
	e := newTestExport(b)
	nodes := e.Nodes()
	id := nodes.ID("co/source")
	b.Run("hit", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, ok := nodes.Path(id); !ok {
				b.Fatal("stale")
			}
		}
	})
	b.Run("miss", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			e.rt.paths.invalid.Add(1)
			if _, ok := nodes.Path(id); !ok {
				b.Fatal("stale")
			}
		}
	})
}
