package export

import (
	"errors"
	"maps"
	"os"
	"path/filepath"
	"testing"
)

// TestMovedDerivedIDOutlivesItsOrigin moves an entry holding a derived id
// out of the directory its id derives from, removes that directory, then
// pages the moved entry out. Its handle must still resolve, as when a
// package manager carries a package's nested module directory into a
// freshly staged copy, renames the copy over the old one and deletes the
// old one.
func TestMovedDerivedIDOutlivesItsOrigin(t *testing.T) {
	for _, tc := range []struct {
		name       string
		file       bool   // move a file (unlogged until it moves) instead of a logged directory
		keep       bool   // keep the origin directory
		coldOrigin bool   // the origin is paged out when it is removed
		evict      bool   // page the moved entry out before using its handle
		restart    string // "", "clean" or "crash"
	}{
		{name: "resident"},
		{name: "origin-kept", keep: true, evict: true},
		{name: "origin-removed", evict: true},
		{name: "cold-origin-removed", coldOrigin: true, evict: true},
		{name: "file-origin-removed", file: true, evict: true},
		{name: "origin-removed-clean-restart", evict: true, restart: "clean"},
		{name: "origin-removed-crash-restart", evict: true, restart: "crash"},
		{name: "file-origin-removed-crash-restart", file: true, evict: true, restart: "crash"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name := filepath.Join(t.TempDir(), "nodes.log")
			ns := newNamespace("pkg/target/package.json", "pkg/target/sub/.bin/tool", "pkg/stage/package.json")
			nt := openLogged(t, name, ns)
			nt.id("pkg/target/package.json")
			sub := nt.id("pkg/target/sub")
			var bin uint64
			if !tc.file {
				bin = nt.id("pkg/target/sub/.bin") // sub gains a child: logged under its derived id
			}
			if !derived(sub) {
				t.Fatalf("sub took counter id %x", sub)
			}
			nt.id("pkg/stage/package.json")

			// Carry sub into the staged copy, delete the original, swap the copy in.
			ns.move("pkg/target/sub", "pkg/stage/sub")
			nt.rename("pkg/target/sub", "pkg/stage/sub")
			final := "pkg/stage/sub"
			if !tc.keep {
				if tc.coldOrigin {
					evictAll(t, nt, ns)
					if nt.find("pkg/target") != nil {
						t.Fatal("origin stayed resident")
					}
				}
				ns.del("pkg/target")
				nt.remove("pkg/target")
				ns.move("pkg/stage", "pkg/target")
				nt.rename("pkg/stage", "pkg/target")
				final = "pkg/target/sub"
			}
			if got := pathOf(t, nt, sub); got != final {
				t.Fatalf("resident: %x → %q, want %q", sub, got, final)
			}
			if tc.evict {
				evictAll(t, nt, ns)
				if nt.find(final) != nil {
					t.Fatalf("%s stayed resident", final)
				}
			}
			switch tc.restart {
			case "clean":
				closeLogged(t, nt)
				nt = openLogged(t, name, ns)
			case "crash":
				nt = openLogged(t, crashCopy(t, name), ns)
			}
			// A client holding the handle uses it first (GETATTR, READDIR,
			// LOOKUP inside), without walking the path that would page it in.
			if got := pathOf(t, nt, sub); got != final {
				t.Fatalf("handle %x of %s → %q", sub, final, got)
			}
			if bin != 0 {
				if got := pathOf(t, nt, bin); got != final+"/.bin" {
					t.Fatalf("child handle %x → %q", bin, got)
				}
			}
			// LOOKUP by name hands out the same handle.
			if got := nt.id(final); got != sub {
				t.Fatalf("id(%s) = %x, want %x", final, got, sub)
			}
		})
	}
}

// moveOutOfRemovedOrigin logs pkg/target/sub under a derived id with a logged
// child, carries sub into pkg/stage, removes pkg/target and renames pkg/stage
// over it, then pages everything out. It returns sub's id.
func moveOutOfRemovedOrigin(t *testing.T, nt *nodeTable, ns *namespace) uint64 {
	t.Helper()
	sub := nt.id("pkg/target/sub")
	nt.id("pkg/target/sub/.bin")
	if !derived(sub) {
		t.Fatalf("sub took counter id %x", sub)
	}
	nt.id("pkg/stage/package.json")
	ns.move("pkg/target/sub", "pkg/stage/sub")
	nt.rename("pkg/target/sub", "pkg/stage/sub")
	ns.del("pkg/target")
	nt.remove("pkg/target")
	ns.move("pkg/stage", "pkg/target")
	nt.rename("pkg/stage", "pkg/target")
	evictAll(t, nt, ns)
	return sub
}

// TestIndexWithoutStraysReplays opens an index a clean close left before the
// table kept strays: the open must replay the log rather than adopt it, which
// recovers the handles of entries moved out of removed directories.
func TestIndexWithoutStraysReplays(t *testing.T) {
	name := filepath.Join(t.TempDir(), "nodes.log")
	ns := newNamespace("pkg/target/sub/.bin/tool", "pkg/stage/package.json")
	nt := openLogged(t, name, ns)
	sub := moveOutOfRemovedOrigin(t, nt, ns)
	closeLogged(t, nt)
	files, err := filepath.Glob(filepath.Join(name+".cache", "nodes-*", strayFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if err := os.Remove(f); err != nil {
			t.Fatal(err)
		}
	}
	nt = openLogged(t, name, ns)
	if nt.opened.adopted {
		t.Fatalf("adopted an index without strays: %v", nt.opened)
	}
	if got := pathOf(t, nt, sub); got != "pkg/target/sub" {
		t.Fatalf("after replay: %x → %q", sub, got)
	}
	evictAll(t, nt, ns)
	closeLogged(t, nt)
	nt = openLogged(t, name, ns)
	if !nt.opened.adopted {
		t.Fatalf("replayed the rebuilt index: %v", nt.opened)
	}
	if got := pathOf(t, nt, sub); got != "pkg/target/sub" {
		t.Fatalf("after adoption: %x → %q", sub, got)
	}
}

// TestStrayOfRemovedHolderGoesStale removes the entry holding a stray id, or
// leaves it inside the origin removed after it, and checks the handle goes
// stale rather than naming another path, then and after restarts.
func TestStrayOfRemovedHolderGoesStale(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cold    bool   // the holder is paged out when it is removed
		restart string // "", "clean" or "crash"
	}{
		{name: "resident"},
		{name: "cold", cold: true},
		{name: "clean-restart", restart: "clean"},
		{name: "crash-restart", restart: "crash"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name := filepath.Join(t.TempDir(), "nodes.log")
			ns := newNamespace("pkg/target/sub/.bin/tool", "pkg/stage/package.json")
			nt := openLogged(t, name, ns)
			sub := moveOutOfRemovedOrigin(t, nt, ns)
			if !tc.cold {
				nt.id("pkg/target/sub")
			}
			ns.del("pkg/target/sub")
			nt.remove("pkg/target/sub")
			evictAll(t, nt, ns)
			switch tc.restart {
			case "clean":
				closeLogged(t, nt)
				nt = openLogged(t, name, ns)
			case "crash":
				nt = openLogged(t, crashCopy(t, name), ns)
			}
			if key, ok := nt.strays[sub]; ok {
				t.Fatalf("stray %x → key %d outlived its holder", sub, key)
			}
			if got := pathOf(t, nt, sub); got != "<stale>" {
				t.Fatalf("removed holder's id %x → %q", sub, got)
			}
			if key, ok := nt.strays[sub]; ok {
				t.Fatalf("stray %x → key %d kept", sub, key)
			}
		})
	}
}

// TestStrayInsideRemovedOriginGoesStale moves an entry holding a derived id
// deeper into its own directory, then removes that directory: the id must go
// stale with it.
func TestStrayInsideRemovedOriginGoesStale(t *testing.T) {
	name := filepath.Join(t.TempDir(), "nodes.log")
	ns := newNamespace("pkg/target/sub/.bin/tool", "pkg/target/deep/package.json", "pkg/other/package.json")
	nt := openLogged(t, name, ns)
	sub := nt.id("pkg/target/sub")
	nt.id("pkg/target/sub/.bin")
	nt.id("pkg/target/deep/package.json")
	nt.id("pkg/other/package.json")
	ns.move("pkg/target/sub", "pkg/target/deep/sub")
	nt.rename("pkg/target/sub", "pkg/target/deep/sub")
	ns.del("pkg/target")
	nt.remove("pkg/target")
	if len(nt.strays) != 0 {
		t.Fatalf("strays of a removed subtree kept: %v", nt.strays)
	}
	evictAll(t, nt, ns)
	if got := pathOf(t, nt, sub); got != "<stale>" {
		t.Fatalf("id %x of a removed subtree → %q", sub, got)
	}
	closeLogged(t, nt)
	nt = openLogged(t, name, ns)
	if got := pathOf(t, nt, sub); got != "<stale>" {
		t.Fatalf("after restart: %x → %q", sub, got)
	}
}

// TestStrayNeverNamesAnotherHolder gives a stray id the key of a live node
// that holds another id: the handle must go stale, not name that node.
func TestStrayNeverNamesAnotherHolder(t *testing.T) {
	name := filepath.Join(t.TempDir(), "nodes.log")
	ns := newNamespace("pkg/other/sub/.bin/tool")
	nt := openLogged(t, name, ns)
	other := nt.id("pkg/other/sub")
	nt.id("pkg/other/sub/.bin")
	nt.mu.Lock()
	holder := nt.byID[other].key
	id := derivedID(nt.next+1, 7) // its directory was never issued
	nt.addStray(id, holder)
	nt.mu.Unlock()
	if got := pathOf(t, nt, id); got != "<stale>" {
		t.Fatalf("stray %x → %q, the path of %x", id, got, other)
	}
	if key, ok := nt.strays[id]; ok {
		t.Fatalf("stray %x → key %d kept", id, key)
	}
}

// TestReadStraysRejectsCorruption checks that a stray file the counter or
// the id layout contradicts makes the index unusable.
func TestReadStraysRejectsCorruption(t *testing.T) {
	const next = 100
	id := uint64(derivedBit | 5<<hashBits | 7)
	for _, tc := range []struct {
		name    string
		strays  map[uint64]uint64
		corrupt func([]byte) []byte
	}{
		{name: "counter id", strays: map[uint64]uint64{9: 10}},
		{name: "id past the limit", strays: map[uint64]uint64{idLimit | derivedBit: 10}},
		{name: "root key", strays: map[uint64]uint64{id: 1}},
		{name: "key not issued", strays: map[uint64]uint64{id: next}},
		{name: "truncated", strays: map[uint64]uint64{id: 10}, corrupt: func(b []byte) []byte { return b[:len(b)-1] }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := writeStrays(dir, tc.strays); err != nil {
				t.Fatal(err)
			}
			if tc.corrupt != nil {
				f := filepath.Join(dir, strayFile)
				b, err := os.ReadFile(f)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(f, tc.corrupt(b), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if got, err := readStrays(dir, next); !errors.Is(err, errNodeIndex) {
				t.Fatalf("read %v, %v", got, err)
			}
		})
	}
	dir := t.TempDir()
	want := map[uint64]uint64{id: 10, id + 1: 99}
	if err := writeStrays(dir, want); err != nil {
		t.Fatal(err)
	}
	if got, err := readStrays(dir, next); err != nil || !maps.Equal(got, want) {
		t.Fatalf("round trip: %v, %v", got, err)
	}
}
