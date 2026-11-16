package nfs

import (
	"os"
	"strconv"
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
		return &listing{dir: dir, verf: verifier(dir, firstCookie, infos), first: firstCookie, entries: infos}
	}
	var c dirCache
	perDir := dirCacheBytes / 4 / (entryBytes + 10)
	a, b := snapshot(1, perDir), snapshot(2, perDir)
	c.put(a)
	c.put(b)
	for dir := uint64(3); dir < 6; dir++ {
		if c.get(1, a.verf) != a { // keeps a recently used
			t.Fatal("recently used snapshot evicted")
		}
		c.put(snapshot(dir, perDir))
	}
	if c.get(2, b.verf) != nil {
		t.Fatal("least recently used snapshot kept past the budget")
	}
	if c.get(2, a.verf) != nil {
		t.Fatal("snapshot returned for another directory")
	}
	huge := snapshot(9, 2*dirCacheBytes/entryBytes)
	c.put(huge)
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

// fakeInfo is a regular file's FileInfo with only a name.
type fakeInfo string

func (n fakeInfo) Name() string     { return string(n) }
func (fakeInfo) Size() int64        { return 0 }
func (fakeInfo) Mode() os.FileMode  { return 0o644 }
func (fakeInfo) ModTime() time.Time { return time.Time{} }
func (fakeInfo) IsDir() bool        { return false }
func (fakeInfo) Sys() any           { return nil }
