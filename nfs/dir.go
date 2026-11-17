package nfs

import (
	"container/list"
	"os"
	"slices"
	"strings"
	"sync"
)

// firstCookie is the READDIR cookie of a listing's first entry. NFSv4
// reserves cookies 1 and 2 (RFC 7530 section 16.24); NFSv3 READDIR gives
// them to "." and "..".
const firstCookie = 3

// listing is a directory's entries as one READDIR saw them, sorted by name;
// entries[i] has cookie first+i. Continuations page through the same
// listing, so a directory changing between pages neither skips nor repeats
// entries.
type listing struct {
	dir     uint64
	verf    uint64
	first   uint64
	entries []os.FileInfo
	// changes is Server.changes from before the directory was read: while
	// it is unchanged, the entries' attributes are as current as when read.
	changes uint64
}

// after returns the index of the first entry after cookie; false for a
// cookie this listing cannot have issued. Cookies below first start at the
// first entry.
func (l *listing) after(cookie uint64) (int, bool) {
	if cookie < l.first {
		return 0, true
	}
	i := cookie - l.first + 1
	if i > uint64(len(l.entries)) {
		return 0, false
	}
	return int(i), true
}

// list returns the listing a READDIR of directory id, at path p, pages
// through: the snapshot verf names when continuing (cookie and verf not 0),
// else a fresh listing. A continuation whose snapshot was evicted lists the
// directory again and fails with stBadCookie unless its entries are still
// the same, which is the verifier no longer matching.
func (s *Server) list(id uint64, p string, cookie, verf uint64) (*listing, status) {
	resume := cookie != 0 && verf != 0
	if resume {
		if l := s.dirs.get(id, verf); l != nil {
			return l, stOK
		}
	}
	l, st := s.read(id, p, firstCookie)
	if st != stOK {
		return nil, st
	}
	if resume && l.verf != verf {
		return nil, stBadCookie
	}
	s.dirs.put(l)
	return l, stOK
}

// relist lists directory id again for a continuation after cookie, from the
// listing verf names, whose snapshot is gone and whose directory changed.
// When s.resumes knows the last name that page returned, it keeps only the
// entries after it. Its entries are numbered from cookie+1: the macOS client
// drops entries whose cookie is not past the one it asked for, and ends the
// listing at BAD_COOKIE.
func (s *Server) relist(id uint64, p string, cookie, verf uint64) (*listing, status) {
	changes := s.changes.Load()
	infos, err := s.FS.ReadDir(p)
	if err != nil {
		return nil, statusOf(err)
	}
	if last, ok := s.resumes.get(resumeKey{id, verf, cookie}); ok {
		i, found := slices.BinarySearchFunc(infos, last, func(info os.FileInfo, name string) int {
			return strings.Compare(info.Name(), name)
		})
		if found {
			i++
		}
		infos = infos[i:]
	}
	first := cookie + 1
	l := &listing{dir: id, verf: verifier(id, first, infos), first: first, entries: infos, changes: changes}
	s.dirs.put(l)
	return l, stOK
}

// read lists directory id, at path p, sorted by name and numbered from first.
func (s *Server) read(id uint64, p string, first uint64) (*listing, status) {
	changes := s.changes.Load()
	infos, err := s.FS.ReadDir(p)
	if err != nil {
		return nil, statusOf(err)
	}
	return &listing{dir: id, verf: verifier(id, first, infos), first: first, entries: infos, changes: changes}, stOK
}

// verifier is a cookie verifier for a directory's entry names and their
// numbering, never 0 (which clients send before they have one): FNV-1a of
// the directory's id, its first cookie and its names, each preceded by a NUL.
func verifier(dir, first uint64, infos []os.FileInfo) uint64 {
	const prime = 1099511628211
	h := uint64(14695981039346656037)
	for _, v := range []uint64{dir, first} {
		for i := range 8 {
			h = (h ^ v>>(8*i)&0xff) * prime
		}
	}
	for _, info := range infos {
		h *= prime // the NUL separator
		for _, c := range []byte(info.Name()) {
			h = (h ^ uint64(c)) * prime
		}
	}
	return max(h, 1)
}

// dirCacheBytes bounds the estimated bytes of the listings dirCache keeps:
// room for tens of thousands of entries, enough for every directory clients
// are paging through at once.
const dirCacheBytes = 64 << 20

// Estimated memory of a listing beyond its names: the struct and its slice,
// and per entry the interface word pair plus the FileInfo behind it (a host
// os.FileInfo with its stat data measured about 250 bytes on macOS).
const (
	listingBytes = 128
	entryBytes   = 256
)

// bytes estimates the memory l holds.
func (l *listing) bytes() int {
	n := listingBytes + len(l.entries)*entryBytes
	for _, info := range l.entries {
		n += len(info.Name())
	}
	return n
}

// dirCache keeps the most recently used listings within dirCacheBytes,
// evicting the least recently used. The newest listing always stays, so a
// directory too big for the budget still pages through its own snapshot.
// The zero value is empty and ready.
type dirCache struct {
	mu     sync.Mutex
	byVerf map[uint64]*list.Element // of *cachedListing
	lru    list.List                // most recently used first
	size   int
}

// cachedListing is a dirCache entry with its estimated size.
type cachedListing struct {
	*listing
	size int
}

// get returns the listing verf names if it belongs to directory dir.
func (c *dirCache) get(dir, verf uint64) *listing {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := c.byVerf[verf]; e != nil {
		if l := e.Value.(*cachedListing); l.dir == dir {
			c.lru.MoveToFront(e)
			return l.listing
		}
	}
	return nil
}

// put records l, replacing any listing with the same verifier.
func (c *dirCache) put(l *listing) {
	n := l.bytes()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byVerf == nil {
		c.byVerf = make(map[uint64]*list.Element)
	}
	if e := c.byVerf[l.verf]; e != nil {
		c.remove(e)
	}
	c.byVerf[l.verf] = c.lru.PushFront(&cachedListing{listing: l, size: n})
	c.size += n
	for c.size > dirCacheBytes && c.lru.Len() > 1 {
		c.remove(c.lru.Back())
	}
}

// remove forgets entry e. The caller holds c.mu.
func (c *dirCache) remove(e *list.Element) {
	l := c.lru.Remove(e).(*cachedListing)
	delete(c.byVerf, l.verf)
	c.size -= l.size
}

// resumeMax bounds the continuation points resumeCache keeps.
const resumeMax = 4096

// resumeKey names a continuation point: the cookie of the last entry a
// READDIR page returned from the listing of directory dir with verifier verf.
type resumeKey struct{ dir, verf, cookie uint64 }

// resumeCache keeps the name of the entry each of the resumeMax most
// recently used continuation points follows, evicting the least recently
// used, so a continuation whose snapshot dirCache evicted resumes after it.
// The zero value is empty and ready.
type resumeCache struct {
	mu    sync.Mutex
	byKey map[resumeKey]*list.Element // of *resumePoint
	lru   list.List                   // most recently used first
}

// resumePoint is a resumeCache entry.
type resumePoint struct {
	key  resumeKey
	last string
}

// put records that continuation point k follows the entry named last.
func (c *resumeCache) put(k resumeKey, last string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byKey == nil {
		c.byKey = make(map[resumeKey]*list.Element)
	}
	if e := c.byKey[k]; e != nil {
		e.Value.(*resumePoint).last = last
		c.lru.MoveToFront(e)
		return
	}
	c.byKey[k] = c.lru.PushFront(&resumePoint{key: k, last: last})
	if c.lru.Len() > resumeMax {
		delete(c.byKey, c.lru.Remove(c.lru.Back()).(*resumePoint).key)
	}
}

// get returns the name of the entry continuation point k follows.
func (c *resumeCache) get(k resumeKey) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.byKey[k]
	if e == nil {
		return "", false
	}
	c.lru.MoveToFront(e)
	return e.Value.(*resumePoint).last, true
}
