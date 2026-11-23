package overlay

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"slices"
	"syscall"
	"time"

	"github.com/stencil-hq/ovnfs/internal/ident"
)

// Entries whose identity a client chose. The kernel client's guest creates
// files and directories ahead of the daemon and sends the requests later
// (kmod/ovnfs_wire.h, ALLOC_IDS), so an entry must take the identity the
// guest already shows for it: one this upper's allocator handed out
// ([FS.AllocIDs]) and no entry holds. The overlay keeps no record of which
// identities went to which client: one handed out for anything else (a
// memory file since removed) passes the same check.

// AllocIDs reserves up to n consecutive new Synthetic identities and
// returns the first and how many there are. They are never handed out
// again: the allocator's persisted mark passes them before they are
// returned, and a restart or a freeze continues above it.
func (f *FS) AllocIDs(n uint64) (ident.ID, uint64, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if f.upper == "" {
		return 0, 0, &fs.PathError{Op: "allocids", Path: "", Err: syscall.EROFS}
	}
	return f.ids.syn.allocateRun(n)
}

// claimable reports whether an entry may take identity id: a Synthetic one
// this upper's allocator handed out, which no visible entry holds. The
// caller holds f.mu exclusively with the index loaded.
func (f *FS) claimable(id ident.ID) bool {
	if !f.ids.syn.issued(id) {
		return false
	}
	_, held := f.locate(id)
	return !held
}

// errNotClaimable refuses an identity an entry may not take.
func errNotClaimable(op, p string) error {
	return &fs.PathError{Op: op, Path: p, Err: syscall.EINVAL}
}

// CreateFile creates regular file name, which must not exist, with identity
// id ([FS.claimable]), exactly mode perm, modification time mtime and
// contents data, in one step: in the memory tier, as one index entry, or on
// disk, staged and renamed into place, when the tier is off or refuses the
// file. The result describes the new file.
func (f *FS) CreateFile(name string, perm os.FileMode, id ident.ID, mtime time.Time, data []byte) (os.FileInfo, error) {
	f.await(name)
	p := Clean(name)
	f.lockSync(p)
	defer f.mu.Unlock()
retry:
	f.syncOps(p)
	if err := f.mutable("create", p); err != nil {
		return nil, err
	}
	if p == "" || reserved(p) {
		return nil, &fs.PathError{Op: "create", Path: name, Err: syscall.EPERM}
	}
	if _, _, err := f.lookup(p); err == nil {
		return nil, &fs.PathError{Op: "create", Path: name, Err: syscall.EEXIST}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if err := f.claimName("create", p, ""); err != nil {
		return nil, err
	}
	if !f.claimable(id) {
		return nil, errNotClaimable("create", name)
	}
	if op := f.queuedOn(parent(p)); op != nil && f.index.entries[parent(p)].Kind != KindDir {
		if err := f.waitOp(op); err != nil {
			return nil, err
		}
		goto retry
	}
	if err := f.ensureDir(parent(p)); err != nil {
		return nil, err
	}
	data = slices.Clone(data)
	if f.mem != nil && int64(len(data)) <= f.mem.fileMax {
		if m := f.addMem(p, perm, mtime, data, id); m != nil {
			if err := f.touchDir(parent(p), false); err != nil {
				return nil, err
			}
			return m.info(path.Base(p)), nil
		}
	}
	// On disk: after the queued changes on the name, as any create there.
	if op := f.queuedOn(p); op != nil {
		if err := f.waitOp(op); err != nil {
			return nil, err
		}
		goto retry
	}
	m := &memFile{data: data, mode: perm & memModeBits, atime: mtime, mtime: mtime, created: time.Now(), id: id}
	m.uid, m.gid = newOwner(f.index.entries[parent(p)].Info)
	marked := f.mayHoldMarker(p)
	tmp := f.stagingName()
	if _, err := stage(tmp, m, f.ids); err != nil {
		f.ids.discard(tmp)
		return nil, err
	}
	if err := renameFile(tmp, f.up(p)); err != nil {
		f.ids.discard(tmp)
		return nil, err
	}
	if err := f.clearWhiteout(p, marked); err != nil {
		return nil, err
	}
	if err := f.refresh(p); err != nil {
		return nil, err
	}
	if err := f.touchDir(parent(p), true); err != nil {
		return nil, err
	}
	return f.index.entries[p].Info, nil
}

// MkdirID creates directory name, which must not exist, under an existing
// directory, with identity id ([FS.claimable]), exactly mode perm and
// modification time mtime. Like [FS.MkdirAll] it queues the host mkdir
// where it can (queue.go). The result describes the new directory.
func (f *FS) MkdirID(name string, perm os.FileMode, id ident.ID, mtime time.Time) (os.FileInfo, error) {
	f.await(name)
	p := Clean(name)
	f.lockSync(p)
	defer f.mu.Unlock()
retry:
	f.syncOps(p)
	if err := f.mutable("mkdir", p); err != nil {
		return nil, err
	}
	if p == "" || reserved(p) {
		return nil, &fs.PathError{Op: "mkdir", Path: name, Err: syscall.EPERM}
	}
	if _, _, err := f.lookup(p); err == nil {
		return nil, &fs.PathError{Op: "mkdir", Path: name, Err: syscall.EEXIST}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if err := f.claimName("mkdir", p, ""); err != nil {
		return nil, err
	}
	if !f.claimable(id) {
		return nil, errNotClaimable("mkdir", name)
	}
	_, pinfo, err := f.lookup(parent(p))
	if err != nil {
		return nil, err
	}
	if !pinfo.IsDir() {
		return nil, &fs.PathError{Op: "mkdir", Path: name, Err: syscall.ENOTDIR}
	}
	lower, err := f.lowerHas(p)
	if err != nil {
		return nil, err
	}
	marked := f.mayHoldMarker(p)
	if f.queueable() && !lower && !marked && f.index.entries[parent(p)].Kind == KindDir {
		if err := f.queueMkdirAs(p, perm, id, mtime); err == nil {
			return f.index.entries[p].Info, nil
		} else if !errors.Is(err, errNotQueued) {
			return nil, err
		}
	}
	if op := f.queuedOn(p); op != nil {
		if err := f.waitOp(op); err != nil {
			return nil, err
		}
		goto retry
	}
	if err := f.ensureDir(parent(p)); err != nil {
		return nil, err
	}
	real := f.up(p)
	if err := os.Mkdir(real, perm.Perm()); err != nil {
		return nil, err
	}
	if err := os.Chmod(real, perm); err != nil {
		return nil, err
	}
	if err := keepMode(real, perm); err != nil {
		return nil, err
	}
	if err := f.ids.persist(real, id); err != nil {
		return nil, err
	}
	if err := os.Chtimes(real, mtime, mtime); err != nil {
		return nil, err
	}
	if err := f.refresh(p); err != nil {
		return nil, err
	}
	if err := f.placeDir(p, false, marked); err != nil {
		return nil, err
	}
	if err := f.refreshDir(parent(p)); err != nil {
		return nil, err
	}
	return f.index.entries[p].Info, nil
}
