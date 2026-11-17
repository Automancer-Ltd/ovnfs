//go:build !windows

package export_test

import (
	"context"
	"encoding/binary"
	"os"
	"slices"
	"sync"
	"testing"

	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/checkout"
	exportfs "github.com/stencil-hq/ovnfs/export"
	"github.com/stencil-hq/ovnfs/internal/nfstest"
)

// recorder collects the recalls the export asks the frontend for.
type recorder struct {
	mu      sync.Mutex
	recalls []exportfs.Recall
}

// add records one recall.
func (r *recorder) add(rc exportfs.Recall) {
	r.mu.Lock()
	r.recalls = append(r.recalls, rc)
	r.mu.Unlock()
}

// has reports whether a recall covered the file NFS handle fh names, then
// clears the recorder.
func (r *recorder) has(fh []byte) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	var id uint64
	if len(fh) >= 8 {
		id = binary.BigEndian.Uint64(fh)
	}
	found := slices.ContainsFunc(r.recalls, func(rc exportfs.Recall) bool { return rc.First <= id && id <= rc.Last })
	r.recalls = nil
	return found
}

// paths returns the paths recalled since the last check, then clears the recorder.
func (r *recorder) paths() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, rc := range r.recalls {
		out = append(out, rc.Path)
	}
	r.recalls = nil
	return out
}

// TestInvalidateTreeRecallsItsCheckout recalls a changed tree over its
// checkout's ids, naming the tree for path-addressed frontends, and leaves
// other checkouts alone.
func TestInvalidateTreeRecallsItsCheckout(t *testing.T) {
	f := newStack(t, map[string]string{"t/d/x": "1", "s": "2"})
	f.create(t, "app")
	f.create(t, "other")
	var rec recorder
	f.export.OnRecall(rec.add)
	c := nfstest.Start(t, f.export.NFS())
	x := lookup(t, c, "app/t/d/x")
	o := lookup(t, c, "other/s")
	rec.has(nil)

	f.export.Invalidate("app", nil, []string{"t"})
	if !rec.has(x) {
		t.Fatal("a tree invalidation did not recall a handle below it")
	}
	f.export.Invalidate("app", nil, []string{"t"})
	if got := rec.paths(); !slices.Equal(got, []string{"app/t"}) {
		t.Fatalf("tree invalidation named %q, want app/t", got)
	}
	f.export.Invalidate("app", nil, []string{""})
	if rec.has(o) {
		t.Fatal("invalidating one checkout recalled another's handle")
	}
}

// TestRecallsChangesBehindNFSClients covers every way a file changes without
// an NFS request: another protocol, the daemon (Invalidate), and checkout
// removal, while NFS requests leave recalls to the NFS server. The handle of
// a vanished path is stale.
func TestRecallsChangesBehindNFSClients(t *testing.T) {
	f := newStack(t, map[string]string{"src": "base"})
	f.create(t, "app")
	var rec recorder
	f.export.OnRecall(rec.add)
	c := nfstest.Start(t, f.export.NFS())
	src := lookup(t, c, "app/src")
	gone := nfsCreate(t, c, lookup(t, c, "app"), "gone", "doomed")
	rec.has(nil) // the client's own requests are not the subject here

	nfsWrite(t, c, src, "nfs")
	if rec.has(src) {
		t.Fatal("an NFS write went through the export's recall hook; the NFS server recalls conflicting delegations itself")
	}

	other, err := f.export.Nodes().OpenFile("app/src", os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		t.Fatal(err)
	}
	other.Close()
	if !rec.has(src) {
		t.Fatal("a write through another protocol did not recall the NFS handle")
	}

	var s checkout.Session
	if err := f.reg.Enter("app", &s); err != nil {
		t.Fatal(err)
	}
	err = s.FS.Remove("gone")
	s.Done()
	if err != nil {
		t.Fatal(err)
	}
	f.export.Invalidate("app", []string{"gone", "never-handed-out"}, nil)
	if !rec.has(gone) {
		t.Fatal("Invalidate did not recall the changed path")
	}
	_, status, err := c.GetAttr(gone)
	nfsStatus(t, status, nfstest.Stale, err)

	if err := f.reg.Rm(context.Background(), api.RmArgs{Checkout: "app"}); err != nil {
		t.Fatal(err)
	}
	if !rec.has(src) {
		t.Fatal("removing the checkout did not recall its handles")
	}
}
