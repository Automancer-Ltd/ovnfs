package export

import (
	"io/fs"
	"os"
	"syscall"
	"time"

	"github.com/stencil-hq/ovnfs/checkout"
	"github.com/stencil-hq/ovnfs/internal/ident"
	"github.com/stencil-hq/ovnfs/overlay"
)

// File ids a client chooses: the kernel client's guest creates files and
// directories ahead of the daemon with ids it reserved (AllocIDs), and the
// entries it later asks for take them (CreateWithID, MkdirWithID).

// AllocIDs reserves up to n consecutive file ids for entries of checkout
// co a client will create, and returns the first and how many there are.
// They are the checkout overlay's Synthetic identities under its slot,
// which no other entry gets, after a daemon restart too.
func (n *Nodes) AllocIDs(co string, count uint64) (first, got uint64, err error) {
	var s checkout.Session
	if err := n.rt.enter(co, "allocids", co, &s); err != nil {
		return 0, 0, err
	}
	defer s.Done()
	slot, _, ok := n.rt.reg.Slot(co)
	if !ok {
		return 0, 0, &fs.PathError{Op: "allocids", Path: co, Err: fs.ErrNotExist}
	}
	id, got, err := s.FS.AllocIDs(count)
	if err != nil {
		return 0, 0, err
	}
	return fileID(slot, id), got, nil
}

// identityFor returns the overlay identity file id fid names in checkout co;
// EINVAL when it belongs to another checkout or slot.
func (n *Nodes) identityFor(op, co string, fid uint64) (ident.ID, error) {
	slot, _, ok := n.rt.reg.Slot(co)
	fslot, id := splitFileID(fid)
	if !ok || fslot != slot {
		return 0, &fs.PathError{Op: op, Path: co, Err: syscall.EINVAL}
	}
	return id, nil
}

// CreateWithID creates regular file name, which must not exist, with file id
// fid from AllocIDs, exactly mode perm, modification time mtime and
// contents data (see [overlay.FS.CreateFile]).
func (n *Nodes) CreateWithID(name string, perm os.FileMode, fid uint64, mtime time.Time, data []byte) (os.FileInfo, error) {
	var info os.FileInfo
	err := n.rt.mutate("create", name, func(f *overlay.FS, p string) error {
		co, _ := splitRouterPath(name)
		id, err := n.identityFor("create", co, fid)
		if err != nil {
			return err
		}
		info, err = f.CreateFile(p, perm, id, mtime, data)
		return err
	})
	return info, err
}

// MkdirWithID creates directory name, which must not exist, with file id fid
// from AllocIDs, exactly mode perm and modification time mtime (see
// [overlay.FS.MkdirID]).
func (n *Nodes) MkdirWithID(name string, perm os.FileMode, fid uint64, mtime time.Time) (os.FileInfo, error) {
	var info os.FileInfo
	err := n.rt.mutate("mkdir", name, func(f *overlay.FS, p string) error {
		co, _ := splitRouterPath(name)
		id, err := n.identityFor("mkdir", co, fid)
		if err != nil {
			return err
		}
		info, err = f.MkdirID(p, perm, id, mtime)
		return err
	})
	return info, err
}
