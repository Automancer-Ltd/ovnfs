package checkout

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/base"
	"github.com/stencil-hq/ovnfs/internal/clone"
	"github.com/stencil-hq/ovnfs/watch"
)

// sharesBlocks reports whether the test's folders and state can share blocks:
// where they cannot, cache roots of snapshots start empty.
func sharesBlocks(t *testing.T) bool {
	t.Helper()
	dir := t.TempDir()
	write(t, filepath.Join(dir, "probe"), "probe")
	in, err := os.Open(filepath.Join(dir, "probe"))
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	err = clone.Reflink(in, filepath.Join(dir, "clone"), 0o644)
	if errors.Is(err, errors.ErrUnsupported) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	return true
}

// cached is want where cache roots hold the folder's content, else "<absent>".
func cached(t *testing.T, want string) string {
	if sharesBlocks(t) {
		return want
	}
	return "<absent>"
}

// checkoutCaches returns the cache snapshot checkout name reads, its base's
// folder and the snapshots on the base's disk.
func (f *fixture) checkoutCaches(name string) (id, dir string, onDisk []string) {
	f.t.Helper()
	f.reg.mu.RLock()
	h := f.reg.checkouts[name]
	f.reg.mu.RUnlock()
	if h == nil {
		f.t.Fatalf("no checkout %s", name)
	}
	ids, err := h.base.view.CachesIDs()
	if err != nil {
		f.t.Fatal(err)
	}
	return h.meta.Caches, h.base.view.Meta().Dir, ids
}

// TestCacheSnapshotsIsolateCheckouts keeps a checkout's cache roots as its
// snapshot holds them however the folder changes; a checkout opened after the
// folder changed there gets a newer snapshot while older checkouts keep
// theirs, those opened without further changes share one, and a snapshot no
// checkout uses is removed. A folder change never invalidates a checkout.
func TestCacheSnapshotsIsolateCheckouts(t *testing.T) {
	f := newFixture(t)
	if !sharesBlocks(t) {
		t.Skip("the volume cannot clone: cache roots start empty")
	}
	dir := folder(t, map[string]string{"src/main": "one", "target/out": "one", "target/gone": "gone"})
	f.newCheckout(dir, "a", "cache dir & target")
	first, root, _ := f.checkoutCaches("a")
	invalidated := len(f.invalidations())

	write(t, filepath.Join(dir, "target/out"), "two")
	write(t, filepath.Join(dir, "target/new"), "new")
	if err := os.Remove(filepath.Join(dir, "target/gone")); err != nil {
		t.Fatal(err)
	}
	f.reg.folderChanged([]watch.Change{{Path: filepath.Join(root, "target", "out")}})
	for p, want := range map[string]string{"target/out": "one", "target/new": "<absent>", "target/gone": "gone", "src/main": "one"} {
		if got := f.read("a", p); got != want {
			t.Errorf("a: %s = %q after the folder changed, want %q", p, got, want)
		}
	}
	if got := f.invalidations()[invalidated:]; len(got) != 0 {
		t.Errorf("folder changes invalidated checkouts: %+v", got)
	}

	f.newCheckout(dir, "b", "cache dir & target")
	second, _, _ := f.checkoutCaches("b")
	if second == first {
		t.Fatal("a checkout opened after the folder changed its cache root shares the older snapshot")
	}
	for p, want := range map[string]string{"target/out": "two", "target/new": "new", "target/gone": "<absent>"} {
		if got := f.read("b", p); got != want {
			t.Errorf("b: %s = %q, want %q", p, got, want)
		}
	}
	if got := f.read("a", "target/out"); got != "one" {
		t.Errorf("a: target/out = %q after b opened, want its own one", got)
	}

	// Checkouts opened while the folder does not change share one snapshot.
	if f.reg.watcher != nil && f.reg.watcher.Covers(root) && f.reg.watcher.Sync() == nil {
		f.reg.mu.RLock()
		b := f.reg.checkouts["b"].base
		f.reg.mu.RUnlock()
		x, errX := f.reg.useCaches(b)
		y, errY := f.reg.useCaches(b)
		if errX != nil || errY != nil || x != y {
			t.Errorf("two checkouts opened on an unchanged folder took snapshots %v and %v (%v, %v)", x, y, errX, errY)
		}
		for _, c := range []*base.Caches{x, y} {
			f.reg.dropCaches(&checkoutRecord{base: b, caches: c})
		}
	}

	if err := f.reg.Rm(t.Context(), api.RmArgs{Checkout: "a"}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the snapshot no checkout uses was not removed", func() bool {
		_, _, onDisk := f.checkoutCaches("b")
		return !slices.Contains(onDisk, first) && slices.Contains(onDisk, second)
	})
}

// TestCacheSnapshotsSurviveRestart reopens a checkout on its own snapshot, not
// the folder as it is then, and removes snapshots on disk no checkout names.
func TestCacheSnapshotsSurviveRestart(t *testing.T) {
	f := newFixture(t)
	dir := folder(t, map[string]string{"src/main": "one", "target/out": "one", ".env": "SECRET=1"})
	f.newCheckout(dir, "a", "cache dir & target")
	id, _, _ := f.checkoutCaches("a")
	if id == "" {
		t.Fatal("a checkout on a local base has no cache snapshot")
	}
	write(t, filepath.Join(dir, "target/out"), "two")
	write(t, filepath.Join(dir, ".env"), "SECRET=2")
	f.reg.mu.RLock()
	baseID := f.reg.checkouts["a"].base.view.Meta().ID
	f.reg.mu.RUnlock()
	orphan := filepath.Join(f.state, "bases", baseID, "caches", "c_000000000000")
	if err := os.MkdirAll(orphan, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := f.reg.Close(); err != nil {
		t.Fatal(err)
	}
	f.open()
	if got, _, _ := f.checkoutCaches("a"); got != id {
		t.Fatalf("reopened checkout reads snapshot %q, want %q", got, id)
	}
	for p, want := range map[string]string{"target/out": cached(t, "one"), ".env": "SECRET=1"} {
		if got := f.read("a", p); got != want {
			t.Errorf("%s = %q after a restart, want the snapshot's %q", p, got, want)
		}
	}
	eventually(t, "a snapshot no checkout names survived the restart", func() bool {
		_, err := os.Stat(orphan)
		return os.IsNotExist(err)
	})
}

// TestCacheRootsOfPreparedCheckouts gives a checkout taken from a folder's
// prepared pool the folder's cache roots as of its create, not as of its
// preparation.
func TestCacheRootsOfPreparedCheckouts(t *testing.T) {
	f := newFixture(t)
	dir := folder(t, map[string]string{"src/main": "one", ".env": "SECRET=1"})
	f.newCheckout(dir, "a")
	_, root, _ := f.checkoutCaches("a")
	write(t, filepath.Join(dir, ".env"), "SECRET=2")
	f.reg.folderChanged([]watch.Change{{Path: filepath.Join(root, ".env")}})
	f.newCheckout(dir, "b")
	if got := f.read("b", ".env"); got != "SECRET=2" {
		t.Errorf("b: .env = %q, want the folder's at its create", got)
	}
	if got := f.read("a", ".env"); got != "SECRET=1" {
		t.Errorf("a: .env = %q, want its snapshot's", got)
	}
}
