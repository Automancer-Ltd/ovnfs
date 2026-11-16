package export

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// openLogged opens the identity log name over namespace ns.
func openLogged(t *testing.T, name string, ns *namespace) *nodeTable {
	t.Helper()
	nt, err := openNodeTable(name, ns.list)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { nt.close() })
	return nt
}

// closeLogged closes nt and reports a log failure.
func closeLogged(t *testing.T, nt *nodeTable) {
	t.Helper()
	if err := nt.close(); err != nil {
		t.Fatal(err)
	}
}

// crashCopy snapshots a live log as a crash would leave it.
func crashCopy(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	crashed := name + ".crashed"
	if err := os.WriteFile(crashed, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return crashed
}

// logSize returns the log's current length.
func logSize(t *testing.T, name string) int64 {
	t.Helper()
	fi, err := os.Stat(name)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Size()
}

// TestNodeLogRestart preserves moved descendants and keeps retired ids stale.
func TestNodeLogRestart(t *testing.T) {
	name := filepath.Join(t.TempDir(), "nodes.log")
	ns := newNamespace("co/a/file", "co/b", "removed/deep/file")
	nt := openLogged(t, name, ns)
	dir, child := nt.id("co/a"), nt.id("co/a/file")
	victim := nt.id("co/b")
	retired := nt.id("removed/deep/file")
	ns.move("co/a", "co/new/b")
	nt.rename("co/a", "co/new/b")
	ns.del("removed")
	nt.remove("removed")
	ns.del("co/b")
	nt.remove("co/b")
	closeLogged(t, nt)
	issued := map[uint64]bool{1: true, dir: true, child: true, victim: true, retired: true}
	for range 2 {
		nt = openLogged(t, name, ns)
		if got := pathOf(t, nt, dir); got != "co/new/b" {
			t.Fatalf("directory: %s", got)
		}
		if got := pathOf(t, nt, child); got != "co/new/b/file" {
			t.Fatalf("child: %s", got)
		}
		for _, id := range []uint64{victim, retired} {
			if _, ok := nt.path(id); ok {
				t.Fatalf("retired %x restored", id)
			}
		}
		if nt.id("co/new/b/file") != child {
			t.Fatal("identity changed")
		}
		ns.add("fresh/dir/file")
		for _, p := range []string{"fresh", "fresh/dir", "fresh/dir/file"} {
			id := nt.id(p)
			if issued[id] {
				t.Fatalf("%s reused %x", p, id)
			}
			issued[id] = true
		}
		ns.del("fresh")
		nt.remove("fresh")
		closeLogged(t, nt)
	}
}

// TestDerivedIDsResolveAfterRestart keeps every handed-out id on its file
// across clean and crashed restarts, though only directories were logged.
func TestDerivedIDsResolveAfterRestart(t *testing.T) {
	files := []string{"co/README", "co/src/a.go", "co/src/b.go", "co/src/deep/x", "co/empty"}
	for _, crash := range []bool{false, true} {
		name := filepath.Join(t.TempDir(), "nodes.log")
		ns := newNamespace(files...)
		nt := openLogged(t, name, ns)
		ids := map[string]uint64{}
		for _, p := range append([]string{"co", "co/src", "co/src/deep"}, files...) {
			ids[p] = nt.id(p)
		}
		for _, p := range files {
			if !derived(ids[p]) {
				t.Fatalf("%s: id %x not derived", p, ids[p])
			}
		}
		reopen := name
		if crash {
			reopen = crashCopy(t, name)
		} else {
			closeLogged(t, nt)
		}
		nt = openLogged(t, reopen, ns)
		for p, id := range ids {
			if got := pathOf(t, nt, id); got != p {
				t.Fatalf("crash=%v: %x → %q, want %q", crash, id, got, p)
			}
		}
		// Looking a path up first must find the same id as resolution. Close
		// first, as a restart would: Windows cannot replace an open log.
		closeLogged(t, nt)
		nt = openLogged(t, reopen, ns)
		for p, id := range ids {
			if got := nt.id(p); got != id {
				t.Fatalf("crash=%v: %s → %x, want %x", crash, p, got, id)
			}
		}
	}
}

// TestFreshReadLogsDirectoriesOnly bounds what reading a fresh tree costs
// the log: one record per directory, nothing per file.
func TestFreshReadLogsDirectoriesOnly(t *testing.T) {
	name := filepath.Join(t.TempDir(), "nodes.log")
	var paths []string
	for _, dir := range []string{"co/one", "co/two"} {
		for i := range 500 {
			paths = append(paths, path.Join(dir, "file"+string(rune('a'+i%26))+string(rune('a'+i/26))))
		}
	}
	ns := newNamespace(paths...)
	nt := openLogged(t, name, ns)
	before := logSize(t, name)
	for _, p := range paths {
		nt.id(p)
	}
	if grown := logSize(t, name) - before; grown > 64 {
		t.Fatalf("1000 files grew the log by %d bytes", grown)
	}
}

// TestRenamePinSurvivesRestart keeps moved derived ids on the moved node,
// including the unlogged children of a moved directory.
func TestRenamePinSurvivesRestart(t *testing.T) {
	name := filepath.Join(t.TempDir(), "nodes.log")
	ns := newNamespace("co/a/f", "co/d/g", "co/b/keep")
	nt := openLogged(t, name, ns)
	f, d, g := nt.id("co/a/f"), nt.id("co/d"), nt.id("co/d/g")
	ns.move("co/a/f", "co/b/f")
	nt.rename("co/a/f", "co/b/f")
	ns.move("co/d", "co/b/e")
	nt.rename("co/d", "co/b/e")
	closeLogged(t, nt)

	ns.add("co/a/f", "co/d/g")
	nt = openLogged(t, name, ns)
	want := map[uint64]string{f: "co/b/f", d: "co/b/e", g: "co/b/e/g"}
	for id, p := range want {
		if got := pathOf(t, nt, id); got != p {
			t.Fatalf("%x → %q, want %q", id, got, p)
		}
	}
	for _, p := range []string{"co/a/f", "co/d", "co/d/g"} {
		if id := nt.id(p); want[id] != "" {
			t.Fatalf("new %s took moved id %x", p, id)
		}
	}
}

// TestCollisionFallbackSurvivesRestart keeps both colliding names on their
// ids: the counted one through the log, the derived one by elimination.
func TestCollisionFallbackSurvivesRestart(t *testing.T) {
	name := filepath.Join(t.TempDir(), "nodes.log")
	a, b := "co/"+collideA, "co/"+collideB
	ns := newNamespace(a, b)
	nt := openLogged(t, name, ns)
	idA, idB := nt.id(a), nt.id(b)
	closeLogged(t, nt)
	nt = openLogged(t, name, ns)
	if got := pathOf(t, nt, idB); got != b {
		t.Fatalf("derived %x → %q", idB, got)
	}
	if got := pathOf(t, nt, idA); got != a {
		t.Fatalf("counted %x → %q", idA, got)
	}
}

// TestCreatedCollisionIsLogged gives a name created next to a derived id of
// the same hash a logged id at once, so that id still resolves after a
// restart even though the newcomer was never handed out.
func TestCreatedCollisionIsLogged(t *testing.T) {
	name := filepath.Join(t.TempDir(), "nodes.log")
	a, b := "co/"+collideA, "co/"+collideB
	ns := newNamespace(a)
	nt := openLogged(t, name, ns)
	idA := nt.id(a)
	ns.add(b)
	nt.created(b)
	closeLogged(t, nt)
	nt = openLogged(t, name, ns)
	if got := pathOf(t, nt, idA); got != a {
		t.Fatalf("%x → %q", idA, got)
	}
	if nt.id(b) == idA {
		t.Fatal("newcomer took the derived id")
	}
}

// TestOwnersSurviveCleanRestart keeps a derived id on the name it was issued
// for across clean restarts, resident or evicted, after that name was replaced
// behind the export or removed through it: a newcomer with the same hash must
// not inherit the handle.
func TestOwnersSurviveCleanRestart(t *testing.T) {
	a, b := "co/"+collideA, "co/"+collideB
	for _, evicted := range []bool{false, true} {
		for _, removed := range []bool{false, true} {
			t.Run(fmt.Sprintf("evicted=%v/removed=%v", evicted, removed), func(t *testing.T) {
				name := filepath.Join(t.TempDir(), "nodes.log")
				ns := newNamespace(a, "pressure/entry", "d0/f", "d1/f", "d2/f")
				nt := openLogged(t, name, ns)
				idA := nt.id(a)
				if evicted {
					forceCold(t, nt, ns, "co")
				}
				want := a
				if removed {
					ns.del(a)
					nt.remove(a)
					want = "<stale>"
				}
				closeLogged(t, nt)
				ns.del(a)
				ns.add(b)
				for restart := range 3 {
					nt = openLogged(t, name, ns)
					if got := pathOf(t, nt, idA); got != want {
						t.Fatalf("restart %d: %x → %q, want %q", restart, idA, got, want)
					}
					if nt.id(b) == idA {
						t.Fatalf("restart %d: newcomer took the issued id", restart)
					}
					if packs, _ := filepath.Glob(filepath.Join(nt.coldRoot, "pack-*")); len(packs) > 1 {
						t.Fatalf("restart %d: earlier packs kept: %v", restart, packs)
					}
					// A new owner elsewhere rewrites the pack around co's.
					nt.id(fmt.Sprintf("d%d/f", restart))
					closeLogged(t, nt)
				}
			})
		}
	}
}

// TestUnreadableAdoptedIndexResolvesByListing treats a damaged index from
// before a restart as missing, as a crash would leave it, instead of failing
// every identity operation.
func TestUnreadableAdoptedIndexResolvesByListing(t *testing.T) {
	name := filepath.Join(t.TempDir(), "nodes.log")
	ns := newNamespace("co/file")
	nt := openLogged(t, name, ns)
	id := nt.id("co/file")
	closeLogged(t, nt)
	breakOwners(t, nt.pack.path, nt.find("co").dir.base)
	nt = openLogged(t, name, ns)
	if got := pathOf(t, nt, id); got != "co/file" || nt.cacheErr != nil {
		t.Fatalf("%x → %q, %v", id, got, nt.cacheErr)
	}
}

// TestUnknownDerivedIDsStayStale rejects ids no directory or name backs.
func TestUnknownDerivedIDsStayStale(t *testing.T) {
	ns := newNamespace("co/file")
	nt := newNodeTable(ns.list)
	nt.id("co") // derived, then pinned with a separate key
	nt.id("co/file")
	co := nt.find("co").key
	for _, id := range []uint64{
		derivedID(co, nameHash("missing")),
		derivedID(co+1000, nameHash("file")),
		co, // a key, not an id
	} {
		if got := pathOf(t, nt, id); got != "<stale>" {
			t.Fatalf("%x → %q", id, got)
		}
	}
}

// TestNodeLogTornTail recovers complete operations before an interrupted
// write, whether cut short or followed by zeros.
func TestNodeLogTornTail(t *testing.T) {
	for _, tail := range [][]byte{{opCounter, 0x80}, {opPin, 1, 9, 'x'}, make([]byte, 16)} {
		name := filepath.Join(t.TempDir(), "nodes.log")
		ns := newNamespace("co/dir/file")
		nt := openLogged(t, name, ns)
		id := nt.id("co/dir/file")
		closeLogged(t, nt)
		f, err := os.OpenFile(name, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write(tail); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		nt = openLogged(t, name, ns)
		if got := pathOf(t, nt, id); got != "co/dir/file" {
			t.Fatalf("tail %v: path = %q", tail, got)
		}
	}
}

// TestNodeLogRejectsCorruption refuses a log whose records contradict
// each other instead of guessing identities.
func TestNodeLogRejectsCorruption(t *testing.T) {
	name := filepath.Join(t.TempDir(), "nodes.log")
	b := append([]byte(nil), nodeLogMagic...)
	b = append(b, opCounter, 5, 77, 1, 'x') // parent key 77 was never placed
	if err := os.WriteFile(name, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := openNodeTable(name, nil); err == nil {
		t.Fatal("inconsistent log accepted")
	}
}

// TestLegacyLogIDsGoStale drops identities of the JSON format but never
// issues one of its ids again.
func TestLegacyLogIDsGoStale(t *testing.T) {
	name := filepath.Join(t.TempDir(), "nodes.log")
	legacy := `{"op":"alloc","id":2,"path":"co"}
{"op":"alloc","id":4000,"path":"co/x"}
{"op":"next","id":4100}
{"op":"alloc","id":9`
	if err := os.WriteFile(name, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	ns := newNamespace("co/x", "new/dir/file")
	nt := openLogged(t, name, ns)
	for _, id := range []uint64{2, 4000} {
		if got := pathOf(t, nt, id); got != "<stale>" {
			t.Fatalf("legacy %d → %q", id, got)
		}
	}
	for _, p := range []string{"co", "co/x", "new/dir/file", "new", "new/dir"} {
		if id := nt.id(p); !derived(id) && id < 4100 {
			t.Fatalf("%s reissued legacy-range id %d", p, id)
		}
	}
	if id, _ := nt.lookup("new"); derived(id) {
		t.Fatal("expected a counter id for a directory created as an ancestor")
	}
}

// captureLog collects nt's log lines. Read them with nt.mu held.
func captureLog(nt *nodeTable) *[]string {
	var lines []string
	nt.mu.Lock()
	nt.logf = func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }
	nt.mu.Unlock()
	return &lines
}

// swapLog replaces nt's log handle with f, under the node mutex. The
// original handle is closed at cleanup.
func swapLog(t *testing.T, nt *nodeTable, f *os.File) {
	t.Helper()
	nt.mu.Lock()
	orig := nt.log
	nt.log = f
	nt.mu.Unlock()
	t.Cleanup(func() { orig.Close() })
}

// failedTable opens a logged table over co/a and co/b/f, issues co/a, then
// makes every log write fail with EBADF.
func failedTable(t *testing.T) (nt *nodeTable, name string, ns *namespace, lines *[]string, issued uint64) {
	t.Helper()
	name = filepath.Join(t.TempDir(), "nodes.log")
	ns = newNamespace("co/a", "co/b/f")
	nt = openLogged(t, name, ns)
	lines = captureLog(nt)
	if issued = nt.id("co/a"); issued == 0 {
		t.Fatal("co/a: no id before the failure")
	}
	ro, err := os.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	swapLog(t, nt, ro)
	return nt, name, ns, lines, issued
}

// TestNodeLogWriteFailureLatchesFault refuses new identities after the first
// failed log append, so no id that a restart cannot resolve reaches a client.
func TestNodeLogWriteFailureLatchesFault(t *testing.T) {
	nt, name, ns, lines, issued := failedTable(t)
	for i := range 3 {
		if id := nt.id("co/b/f"); id != 0 {
			t.Fatalf("call %d: issued %x after the log failed", i, id)
		}
	}
	if nt.fault.Load() == nil {
		t.Fatal("no fault latched")
	}
	if err := (&nodeFS{nodes: nt}).IdentityError(); !errors.Is(err, syscall.EIO) {
		t.Fatalf("identity error: %v", err)
	}
	nt.mu.Lock()
	got := len(*lines)
	nt.mu.Unlock()
	if got != 1 {
		t.Fatalf("log lines: %d, want 1: %q", got, *lines)
	}

	// A restart from the log as it stands resolves every issued id.
	reopened := openLogged(t, crashCopy(t, name), ns)
	if got := pathOf(t, reopened, issued); got != "co/a" {
		t.Fatalf("issued id resolves to %s", got)
	}
	fresh := reopened.id("co/b/f")
	for _, id := range []uint64{1, reopened.id("co"), issued} {
		if fresh == 0 || fresh == id {
			t.Fatalf("fresh id %x reuses %x", fresh, id)
		}
	}
}

// TestNodeLogFailureShowsInStatus reports a failed log write as the node
// cache error that status shows.
func TestNodeLogFailureShowsInStatus(t *testing.T) {
	nt, _, _, _, _ := failedTable(t)
	nt.id("co/b/f")
	got := (&Export{fs: &nodeFS{nodes: nt}}).NodeCacheError()
	if !strings.HasPrefix(got, "node log:") || !strings.Contains(got, "input/output error") {
		t.Fatalf("node cache error: %q", got)
	}
}

// TestNodeLogSyncFailureLatchesFault latches the same fault when the flush
// worker cannot make the log durable.
func TestNodeLogSyncFailureLatchesFault(t *testing.T) {
	name := filepath.Join(t.TempDir(), "nodes.log")
	nt := openLogged(t, name, newNamespace("co/a"))
	lines := captureLog(nt)
	closed, err := os.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	closed.Close()
	swapLog(t, nt, closed)
	nt.mu.Lock()
	nt.dirty = true
	nt.mu.Unlock()
	deadline := time.Now().Add(2 * time.Second)
	for nt.fault.Load() == nil {
		if time.Now().After(deadline) {
			t.Fatal("no fault latched within 2s")
		}
		time.Sleep(10 * time.Millisecond)
	}
	nt.mu.Lock()
	got := len(*lines)
	nt.mu.Unlock()
	if got != 1 {
		t.Fatalf("log lines: %d, want 1: %q", got, *lines)
	}
}
