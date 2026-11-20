package nfs

import (
	"os"
	"strconv"
	"sync"
	"testing"
	"time"
)

// TestDirCacheBudget checks that snapshots are kept by estimated bytes, least
// recently used out first, and that the newest stays even past the budget.
func TestDirCacheBudget(t *testing.T) {
	snapshot := func(dir uint64, n int) *listing {
		infos := make([]os.FileInfo, n)
		for i := range infos {
			infos[i] = fakeInfo("entry-" + strconv.Itoa(i))
		}
		return &listing{dir: dir, verf: verifier(dir, 0, firstCookie, infos), first: firstCookie, entries: infos}
	}
	var c dirCache
	perDir := dirCacheBytes / 4 / (entryBytes + 10)
	a, b := snapshot(1, perDir), snapshot(2, perDir)
	c.put(a, a.bytes())
	c.put(b, b.bytes())
	for dir := uint64(3); dir < 6; dir++ {
		if c.get(1, a.verf) != a { // keeps a recently used
			t.Fatal("recently used snapshot evicted")
		}
		l := snapshot(dir, perDir)
		c.put(l, l.bytes())
	}
	if c.get(2, b.verf) != nil {
		t.Fatal("least recently used snapshot kept past the budget")
	}
	if c.get(2, a.verf) != nil {
		t.Fatal("snapshot returned for another directory")
	}
	huge := snapshot(9, 2*dirCacheBytes/entryBytes)
	c.put(huge, huge.bytes())
	if c.get(9, huge.verf) != huge || c.get(1, a.verf) != nil || c.size != huge.bytes() {
		t.Fatalf("oversized snapshot: kept %v, size %d", c.get(9, huge.verf) == huge, c.size)
	}
}

// TestResumeCacheBounded checks that continuation points are kept up to
// resumeMax, least recently used out first.
func TestResumeCacheBounded(t *testing.T) {
	var c resumeCache
	key := func(i int) resumeKey { return resumeKey{dir: 1, verf: 2, cookie: uint64(i)} }
	for i := range 5000 {
		c.put(key(i), strconv.Itoa(i))
	}
	oldest := 5000 - resumeMax
	if last, ok := c.get(key(oldest)); !ok || last != strconv.Itoa(oldest) {
		t.Fatalf("point %d: %q %v", oldest, last, ok)
	}
	c.put(key(5000), "5000")
	if c.lru.Len() != resumeMax || len(c.byKey) != resumeMax {
		t.Fatalf("holds %d points, want %d", c.lru.Len(), resumeMax)
	}
	if _, ok := c.get(key(oldest - 1)); ok {
		t.Fatal("point kept past the limit")
	}
	if _, ok := c.get(key(oldest)); !ok {
		t.Fatal("recently used point dropped")
	}
	if _, ok := c.get(key(oldest + 1)); ok {
		t.Fatal("least recently used point kept")
	}
	if last, ok := c.get(key(5000)); !ok || last != "5000" {
		t.Fatalf("newest point: %q %v", last, ok)
	}
}

// blockedListingFS holds one fresh listing after ReadDir has obtained the
// old entries, and one cached continuation during page encoding.
type blockedListingFS struct {
	Backend
	read, child                sync.Once
	scanned, encoding, release chan struct{}
}

func (fs *blockedListingFS) ReadDir(p string) ([]os.FileInfo, error) {
	infos, err := fs.Backend.ReadDir(p)
	fs.read.Do(func() { close(fs.scanned); <-fs.release })
	return infos, err
}

func (fs *blockedListingFS) ChildID(p string, info os.FileInfo) uint64 {
	fs.child.Do(func() { close(fs.encoding); <-fs.release })
	return fs.Backend.ChildID(p, info)
}

// TestRetireDuringReaddir retires a range with both kinds of publication
// in flight: the listing itself, and a v3 resume point from a cached page.
func TestRetireDuringReaddir(t *testing.T) {
	fs := newTestFS(t)
	dirFixture(t, fs, 30)
	id := fs.FileID("d")
	s := &Server{FS: fs}
	pg := readdir3(t, direct(s), false, id, 0, 0, 0, 400)
	if pg.st != stOK || pg.eof || len(pg.entries) <= 2 {
		t.Fatalf("expected partial first page, got %+v", pg)
	}
	last := pg.entries[len(pg.entries)-1]
	if _, ok := s.resumes.get(resumeKey{id, pg.verf, last.cookie}); !ok {
		t.Fatal("first page has no resume point")
	}
	// Keep two versions in the range. An unrelated checkout also has a
	// live scan with a continuation point.
	fs.write(t, "d/another", "")
	old := listingDir{id: id, path: "d"}
	if newer, st := s.list(old, 0, 0); st != stOK || newer.verf == pg.verf {
		t.Fatal("second version not cached")
	}
	if err := fs.MkdirAll("other", 0o755); err != nil {
		t.Fatal(err)
	}
	fs.write(t, "other/first", "")
	fs.write(t, "other/later", "")
	other := listingDir{id: fs.FileID("other"), path: "other"}
	keep, st := s.list(other, 0, 0)
	if st != stOK || s.finishPage(other, keep.verf, firstCookie, "first") != stOK {
		t.Fatal("unrelated listing not cached")
	}

	gate := &blockedListingFS{Backend: fs, scanned: make(chan struct{}), encoding: make(chan struct{}), release: make(chan struct{})}
	s.FS = gate
	page := func(cookie, verf uint64, ready <-chan struct{}) <-chan status {
		t.Helper()
		done := make(chan status, 1)
		go func() {
			_, res := direct(s)(v3ReadDir, func(e *encoder) {
				e.fh(id, old.gen)
				e.u64(cookie)
				e.u64(verf)
				e.u32(400)
			})
			done <- status(res.u32())
		}()
		select {
		case <-ready:
		case st := <-done:
			t.Fatalf("page returned %v before retirement", st)
		}
		return done
	}
	// Also release a blocked call if the other call fails to reach its gate.
	release := sync.OnceFunc(func() { close(gate.release) })
	defer release()
	scan := page(0, 0, gate.scanned)
	hit := page(last.cookie, pg.verf, gate.encoding)
	// Like the registry: change ownership and retire caches under one lock.
	fs.mu.Lock()
	fs.gens[id]++
	s.RetireRange(id, id)
	fs.mu.Unlock()
	release()
	if st := <-scan; st != stStale {
		t.Errorf("old scan: %v, want STALE", st)
	}
	if st := <-hit; st != stStale {
		t.Errorf("old cached page: %v, want STALE", st)
	}
	if s.dirs.lru.Len() != 1 || len(s.dirs.byVerf) != 1 || s.dirs.size != keep.bytes() ||
		s.dirs.get(other.id, keep.verf) != keep {
		t.Fatal("retired listings remain or unrelated listing was evicted")
	}
	if name, ok := s.resumes.get(resumeKey{other.id, keep.verf, firstCookie}); !ok || name != "first" ||
		s.resumes.lru.Len() != 1 || len(s.resumes.byKey) != 1 {
		t.Fatal("retired resume points remain or unrelated resume point was lost")
	}
	if s.finishPage(other, keep.verf, firstCookie+1, "later") != stOK {
		t.Fatal("unrelated in-flight page could not record its continuation")
	}
}

// TestReaddirReusedSlot presents the same id and path, even the same names,
// with a new handle generation. The old handle is stale on both protocols;
// v4 cannot continue its verifier, and v3 recovers a new listing from the
// beginning rather than applying a resume point from the previous checkout.
func TestReaddirReusedSlot(t *testing.T) {
	fs := newTestFS(t)
	files := dirFixture(t, fs, 30)
	id := fs.FileID("d")
	s, addr := v4serve(t, fs, nil)
	c := v4dial(t, addr)
	v3 := wire(c.rc)
	first := readdir3(t, v3, false, id, 0, 0, 0, 400)
	if first.st != stOK || first.eof || len(first.entries) <= 2 {
		t.Fatalf("expected partial first page, got %+v", first)
	}
	cookie := first.entries[len(first.entries)-1].cookie
	fs.mu.Lock()
	fs.gens[id]++
	s.RetireRange(id, id)
	fs.mu.Unlock()

	if pg := readdir3(t, v3, false, id, cookie, first.verf, 0, 400); pg.st != stStale {
		t.Errorf("old v3 handle: %v, want STALE", pg.st)
	}
	if st := c.run(v4PutFH(id), v4ReadDir(cookie, first.verf, 400)).status; st != stStale {
		t.Errorf("old v4 handle: %v, want STALE", st)
	}
	if st := c.run(v4Root(), v4Look("d"), v4ReadDir(cookie, first.verf, 400)).status; st != stNotSame {
		t.Errorf("old verifier on a new v4 handle: %v, want NOT_SAME", st)
	}
	_, res := v3(v3ReadDir, func(e *encoder) {
		e.fh(id, fs.Generation(id))
		e.u64(cookie)
		e.u64(first.verf)
		e.u32(400)
	})
	if st := status(res.u32()); st != stOK {
		t.Fatalf("v3 recovery on a new handle: %v", st)
	}
	postOpOf(t, res)
	if res.u64() == first.verf || !res.bool() {
		t.Fatal("expected a fresh, nonempty v3 listing")
	}
	res.u64() // fileid
	if name := res.str(maxPath3); name != files[0] || res.u64() != cookie+1 {
		t.Fatalf("recovered page does not start with %q after cookie %d", files[0], cookie)
	}
}

// fakeInfo is a regular file's FileInfo with only a name.
type fakeInfo string

func (n fakeInfo) Name() string     { return string(n) }
func (fakeInfo) Size() int64        { return 0 }
func (fakeInfo) Mode() os.FileMode  { return 0o644 }
func (fakeInfo) ModTime() time.Time { return time.Time{} }
func (fakeInfo) IsDir() bool        { return false }
func (fakeInfo) Sys() any           { return nil }
