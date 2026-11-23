package base

import (
	"os"
	"path"
	"sync/atomic"
	"time"

	"github.com/stencil-hq/ovnfs/internal/ident"
	"github.com/stencil-hq/ovnfs/internal/unixstat"
)

// seenBits sizes the per-snapshot table of pre-fence cache-root entries
// handed to clients (1<<seenBits slots): 256 KiB of slot pointers once used.
const seenBits = 15

// fencedMemo remembers the pre-fence path and metadata of non-directory cache
// entries a snapshot handed out, keyed by State identity value (inode).
// Direct-mapped and lock-free: a newer inode in the same slot replaces an
// older one, and multiple names of one hard-linked inode chain in the slot.
//
// When the folder later changes or removes a cache entry, the fence hides it
// from lookups and listings so builds never consume newer host output (#58),
// while frontends that still hold its handle can read its pre-fence metadata
// or replace it cleanly on a truncating write instead of failing ESTALE.
type fencedMemo struct {
	slots atomic.Pointer[[1 << seenBits]atomic.Pointer[seenEntry]]
}

// seenEntry is one observed non-directory entry below a cache root.
type seenEntry struct {
	ino  uint64
	path string
	info fencedInfo
	next *seenEntry
}

// fencedInfo is a self-contained snapshot of a fenced file's pre-fence
// metadata, carrying its State identity and POSIX stat fields.
type fencedInfo struct {
	name  string
	size  int64
	mode  os.FileMode
	mtime time.Time
	stat  unixstat.Info
	id    ident.ID
}

func (i fencedInfo) Name() string       { return i.name }
func (i fencedInfo) Size() int64        { return i.size }
func (i fencedInfo) Mode() os.FileMode  { return i.mode }
func (i fencedInfo) ModTime() time.Time { return i.mtime }
func (i fencedInfo) IsDir() bool        { return false }
func (i fencedInfo) Sys() any           { return i.stat }
func (i fencedInfo) Identity() ident.ID { return i.id }

// snapshotFenced extracts a self-contained pre-fence FileInfo for entry p.
func snapshotFenced(id ident.ID, p string, info os.FileInfo) fencedInfo {
	st, ok := unixstat.Of(info)
	if !ok {
		st = unixstat.Info{Nlink: 1}
	} else if st.Nlink == 0 {
		st.Nlink = 1
	}
	return fencedInfo{
		name:  path.Base(p),
		size:  info.Size(),
		mode:  info.Mode(),
		mtime: info.ModTime(),
		stat:  st,
		id:    id,
	}
}

// slot returns the bucket for ino; nil before the first put unless create is set.
func (m *fencedMemo) slot(ino uint64, create bool) *atomic.Pointer[seenEntry] {
	slots := m.slots.Load()
	if slots == nil {
		if !create {
			return nil
		}
		m.slots.CompareAndSwap(nil, new([1 << seenBits]atomic.Pointer[seenEntry]))
		slots = m.slots.Load()
	}
	return &slots[(ino*0x9e3779b97f4a7c15)>>(64-seenBits)]
}

// put remembers that id was served as non-directory cache entry p with info.
func (m *fencedMemo) put(id ident.ID, p string, info os.FileInfo) {
	if id.Space() != ident.State || info.IsDir() {
		return
	}
	ino := id.Value()
	slot := m.slot(ino, true)
	snap := snapshotFenced(id, p, info)
	for {
		old := slot.Load()
		var next *seenEntry
		if old != nil && old.ino == ino {
			for e := old; e != nil; e = e.next {
				if e.path == p {
					return
				}
			}
			next = old
		}
		if slot.CompareAndSwap(old, &seenEntry{ino: ino, path: p, info: snap, next: next}) {
			return
		}
	}
}

// get returns the first remembered name and pre-fence metadata of id accepted
// by match (or the head entry when match is nil).
func (m *fencedMemo) get(id ident.ID, match func(p string) bool) (string, os.FileInfo, bool) {
	if id.Space() != ident.State {
		return "", nil, false
	}
	ino := id.Value()
	slot := m.slot(ino, false)
	if slot == nil {
		return "", nil, false
	}
	for e := slot.Load(); e != nil && e.ino == ino; e = e.next {
		if match == nil || match(e.path) {
			return e.path, e.info, true
		}
	}
	return "", nil, false
}
