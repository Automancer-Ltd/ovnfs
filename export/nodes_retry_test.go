package export

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestLateColdReadFault rejects the id selected before a trim discovers that
// an existing authoritative index cannot be read for its replacement.
func TestLateColdReadFault(t *testing.T) {
	ns := newNamespace("co/file", "co/new", "pressure/entry")
	nt := newNodeTable(ns.list)
	nt.id("co/file")
	forceCold(t, nt, ns, "co")
	nt.cacheLimit = 1 << 20
	nt.mu.Lock()
	nt.addLeaf(nt.find("co"), "new")
	nt.find("co").dir.coldDirty = true
	nt.mu.Unlock()
	breakOwners(t, nt.pack.path, ownerHead(t, nt, nt.find("co").key))
	nt.cacheLimit = 0
	if id := nt.id("pressure/entry"); id != 0 || nt.cacheErr == nil {
		t.Fatalf("late fault returned %x, %v", id, nt.cacheErr)
	}
	// forceCold's cleanup expects success; the fault is asserted above.
	nt.cacheErr = nil
	nt.fault.Store(nil)
}

func TestSpillRetryPreservesResidentOwners(t *testing.T) {
	ns := newNamespace("co/file", "pressure/entry")
	nt := newNodeTable(ns.list)
	defer nt.close()
	old := nt.id("co/file")
	blocked := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(blocked, nil, 0600); err != nil {
		t.Fatal(err)
	}
	nt.coldRoot = blocked
	nt.cacheLimit = 0
	fresh := nt.id("pressure/entry")
	if nt.spillErr == nil || nt.cacheErr != nil || fresh == 0 {
		t.Fatalf("failed spill: %v %v %x", nt.spillErr, nt.cacheErr, fresh)
	}
	used, next := nt.cacheUsed, nt.next
	for i := range 1000 {
		p := fmt.Sprintf("new%d/file", i)
		ns.add(p)
		if nt.id(p) != 0 {
			t.Fatal("degraded cache admitted a new identity")
		}
	}
	if nt.cacheUsed != used || nt.next != next {
		t.Fatal("degraded admissions grew ownership")
	}
	if release, err := nt.guard(false, "new0/file"); err == nil {
		release()
		t.Fatal("degraded mutation admitted new ownership")
	}
	if err := nt.admitListing("new0"); err == nil {
		t.Fatal("degraded listing admitted a snapshot")
	}
	retry := nt.spillRetry
	for range 10 {
		if pathOf(t, nt, old) != "co/file" {
			t.Fatal("lost resident owner")
		}
	}
	if nt.spillRetry != retry || !retry.After(time.Now()) {
		t.Fatal("retry did not back off")
	}
	nt.coldRoot = ""
	nt.spillRetry = time.Time{}
	nt.id("pressure/entry")
	if nt.spillErr != nil || nt.cacheUsed > nt.cacheLimit+512 {
		t.Fatalf("retry: %v, usage %d", nt.spillErr, nt.cacheUsed)
	}
	if pathOf(t, nt, old) != "co/file" {
		t.Fatal("retry lost owner")
	}
}

func TestColdStoreLifecycle(t *testing.T) {
	root := t.TempDir()
	name := filepath.Join(root, "nodes.log")
	abandoned := filepath.Join(name+".cache", "instance-abandoned")
	if err := os.MkdirAll(abandoned, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(abandoned, "old"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	ns := newNamespace("co/file", "pressure/entry")
	nt := openLogged(t, name, ns)
	if _, err := os.Stat(abandoned); !os.IsNotExist(err) {
		t.Fatalf("abandoned store survived: %v", err)
	}
	nt.id("co/file")
	nt.cacheLimit = 0
	nt.id("pressure/entry")
	if filepath.Dir(nt.coldRoot) != name+".cache" {
		t.Fatal(nt.coldRoot)
	}
	if nt.pack == nil || nt.pack.end == 0 || nt.pack.end != nt.coldBytes {
		t.Fatalf("index size %v != %d", nt.pack, nt.coldBytes)
	}
	other := openLogged(t, filepath.Join(t.TempDir(), "nodes.log"), ns)
	defer closeLogged(t, other)
	if _, err := os.Stat(nt.coldRoot); err != nil {
		t.Fatal("independent export removed a live store", err)
	}
	cold := nt.coldRoot
	closeLogged(t, nt)
	if _, err := os.Stat(cold); err != nil {
		t.Fatal("planned close removed store", err)
	}
	nt = openLogged(t, name, ns)
	if nt.coldRoot != cold {
		t.Fatalf("reopen after a clean close used %q, not the completed store", nt.coldRoot)
	}
	closeLogged(t, nt)
	// The store belongs to the log it was completed for.
	if err := os.WriteFile(name, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	nt = openLogged(t, name, ns)
	defer closeLogged(t, nt)
	if _, err := os.Stat(cold); !os.IsNotExist(err) {
		t.Fatal("reopen with another log retained the store", err)
	}
}

// busyRestart issues an id for co/<leaf>, crashes, and reopens the log over
// a directory co where a create lands during every read of co. The extra
// paths appear after the crash.
func busyRestart(t *testing.T, leaf string, extra ...string) (*nodeTable, uint64) {
	t.Helper()
	name := filepath.Join(t.TempDir(), "nodes.log")
	ns := newNamespace("co/"+leaf, "co/other")
	nt := openLogged(t, name, ns)
	id := nt.id("co/" + leaf)
	if !derived(id) {
		t.Fatalf("co/%s got a counter id %x", leaf, id)
	}
	ns.add(extra...)
	nt2 := openLogged(t, crashCopy(t, name), ns)
	creates, busy := 0, false
	nt2.list = func(dir string) ([]os.FileInfo, error) {
		if dir == "co" && !busy {
			busy = true
			creates++
			p := fmt.Sprintf("co/new%d", creates)
			ns.add(p)
			nt2.created(p)
			busy = false
		}
		return ns.list(dir)
	}
	return nt2, id
}

// coState returns the listing and leaf count of co.
func coState(nt *nodeTable) (*dirList, int) {
	nt.mu.Lock()
	defer nt.mu.Unlock()
	d := nt.find("co")
	if d == nil || d.dir == nil {
		return nil, 0
	}
	return d.dir.list, len(d.dir.leaves)
}

// TestResolveDecidesFromReadListing resolves a pre-crash handle from the
// listing it read, though a create during each read makes install refuse it.
func TestResolveDecidesFromReadListing(t *testing.T) {
	nt, id := busyRestart(t, "leaf")
	before, _ := coState(nt)
	if got := pathOf(t, nt, id); got != "co/leaf" {
		t.Fatalf("handle resolved to %s", got)
	}
	if after, _ := coState(nt); after != before {
		t.Fatal("a refused listing was stored")
	}
	if got := pathOf(t, nt, id); got != "co/leaf" {
		t.Fatalf("second resolution: %s", got)
	}
}

// TestResolveKeepsDegradedBehavior grows no leaf from a read listing while
// a failed spill stops admissions.
func TestResolveKeepsDegradedBehavior(t *testing.T) {
	nt, id := busyRestart(t, "leaf")
	nt.spillErr = errors.New("spill")
	nt.spillRetry = time.Now().Add(time.Hour)
	nt.cacheLimit = 0
	_, leaves := coState(nt)
	if p, ok := nt.path(id); ok {
		t.Fatalf("degraded resolution returned %s", p)
	}
	if nt.spillErr == nil {
		t.Fatal("spill failure cleared during the test")
	}
	if _, after := coState(nt); after != leaves {
		t.Fatalf("leaves grew from %d to %d", leaves, after)
	}
}

// TestResolveAmbiguousReadListing resolves nothing when the read listing
// holds two unowned entries of the handle's hash.
func TestResolveAmbiguousReadListing(t *testing.T) {
	nt, id := busyRestart(t, collideA, "co/"+collideB)
	if p, ok := nt.path(id); ok {
		t.Fatalf("ambiguous handle resolved to %s", p)
	}
}

// TestResolveRechecksReadListing resolves nothing from a listing read while
// its directory was removed and replaced: the handle died with the old key.
func TestResolveRechecksReadListing(t *testing.T) {
	nt, id := busyRestart(t, "leaf")
	ns, read := newNamespace("co/leaf", "co/other"), false
	nt.list = func(dir string) ([]os.FileInfo, error) {
		infos, err := ns.list(dir)
		if dir == "co" && !read {
			read = true
			nt.remove("co")
			ns.del("co")
			ns.add("co/leaf")
			nt.created("co")
			nt.created("co/leaf")
		}
		return infos, err
	}
	if p, ok := nt.path(id); ok {
		t.Fatalf("handle of a removed directory resolved to %s", p)
	}
}
