package export

import (
	"sync/atomic"

	"github.com/stencil-hq/ovnfs/overlay"
)

// pathCacheBits sets the number of file ids whose paths [pathCache] keeps,
// 1<<pathCacheBits: with their paths, a few MiB at most.
const pathCacheBits = 14

// pathCache remembers the export paths file ids last resolved to, so that a
// protocol request naming a file it named before (the directory a listing's
// lookups all start from, a file being read or written) finds its path
// without the syscalls [overlay.FS.Locate] verifies it with.
//
// A cached path is used only while nothing could have moved or removed the
// entry since it was resolved: the checkout's overlay is the same, with the
// same namespace generation ([overlay.FS.Names]), and the daemon has not
// changed checkouts behind the overlay ([Export.Invalidate]). Otherwise
// the id is resolved again, so a removed entry's id is stale and a moved
// one's names its new path, as without the cache.
//
// The cache is direct-mapped: each id has one place, a newer id there
// replaces an older one, and readers take no lock.
type pathCache struct {
	entries [1 << pathCacheBits]atomic.Pointer[pathEntry]
	invalid atomic.Uint64 // advanced by Export.Invalidate
}

// pathEntry is one resolution: file id fid named path in the checkout of
// life life, whose overlay fs had namespace generation names, while the
// cache's invalid count was invalid. Entries are never changed once stored.
type pathEntry struct {
	fid     uint64
	path    string
	fs      *overlay.FS
	names   uint64
	invalid uint64
	life    uint32
}

// place returns where fid's entry goes. Ids of one checkout differ in
// their low bits, so a multiplicative hash spreads them over the cache.
func (c *pathCache) place(fid uint64) *atomic.Pointer[pathEntry] {
	return &c.entries[(fid*0x9e3779b97f4a7c15)>>(64-pathCacheBits)]
}

// get returns the path fid resolved to in checkout life on fs while it
// was at namespace generation names, if that is still cached.
func (c *pathCache) get(fid uint64, fs *overlay.FS, life uint32, names, invalid uint64) (string, bool) {
	e := c.place(fid).Load()
	if e == nil || e.fid != fid || e.fs != fs || e.life != life || e.names != names || e.invalid != invalid {
		return "", false
	}
	return e.path, true
}

// put caches the resolution of fid to path, made on fs after reading its
// namespace generation names and the cache's invalid count.
func (c *pathCache) put(fid uint64, path string, fs *overlay.FS, life uint32, names, invalid uint64) {
	c.place(fid).Store(&pathEntry{fid: fid, path: path, fs: fs, names: names, invalid: invalid, life: life})
}
