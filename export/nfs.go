package export

import (
	"os"
	"syscall"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/stencil-hq/ovnfs/checkout"
	"github.com/stencil-hq/ovnfs/nfs"
	"github.com/stencil-hq/ovnfs/overlay"
)

// nfsFS is the export as the NFS server sees it.
type nfsFS struct {
	protocolFS
	handles *handleGens
}

// NFS exposes the routed filesystem to the NFS server. Its requests skip the
// export's recall hook: the server recalls the delegations an NFS request
// conflicts with itself, and recalling the requesting client's own delegation
// races with the reply that granted it.
func (e *Export) NFS() nfs.Backend { return &nfsFS{protocolFS: e.protocolFS(), handles: e.handles} }

// ChildID identifies an entry of a listing of dir without a syscall.
func (n *nfsFS) ChildID(dir string, info os.FileInfo) uint64 { return n.rt.childID(dir, info) }

// Generation returns what id's current handles carry beside it: the life of
// its checkout, so a handle of a removed checkout never reaches a later one
// in the same slot, and how often RetireHandle retired id's handles.
func (n *nfsFS) Generation(id uint64) uint64 {
	var life uint32
	if slot, _ := splitFileID(id); slot != 0 {
		_, life, _ = n.rt.reg.SlotOwner(slot)
	}
	return uint64(life)<<32 | uint64(n.handles.get(id))
}

// Path resolves an NFS handle, file id id with generation gen, to the
// current path of its entry; false when the entry is gone or the handle is
// not id's current one.
func (n *nfsFS) Path(id, gen uint64) (string, bool) {
	p, life, ok := n.rt.locate(id)
	if !ok || gen != uint64(life)<<32|uint64(n.handles.get(id)) {
		return "", false
	}
	return p, true
}

// SyncFile commits a handle in its current checkout generation. A writer
// kept across a freeze may be closed; sync the current backing without
// reopening that writer, which would copy it into the new upper.
func (n *nfsFS) SyncFile(p string, id, gen uint64, writer billy.File) error {
	slot, entry := splitFileID(id)
	if slot == 0 {
		switch id {
		case rootID:
			return syscall.EISDIR
		case markerID:
			return nil
		default:
			return syscall.ESTALE
		}
	}
	var s checkout.Session
	rf, _ := writer.(*routerFile)
	if rf != nil {
		// Match routerFile.current's lock order.
		rf.mu.Lock()
		defer rf.mu.Unlock()
		if rf.closed || rf.life.retired.Load() || rf.identity != entry {
			return syscall.ESTALE
		}
	}
	co, life, err := n.rt.reg.EnterSlot(slot, &s)
	if err != nil {
		return syscall.ESTALE
	}
	defer s.Done()
	name, rel := splitRouterPath(p)
	if co != name || gen != uint64(life)<<32|uint64(n.handles.get(id)) {
		return syscall.ESTALE
	}
	var opened billy.File
	if rf != nil && rf.gen == s.Gen {
		opened = rf.file
	}
	return s.FS.SyncFile(rel, entry, opened)
}

// RetireHandle makes id's existing NFS handles stale while its path keeps
// the file id; a fresh lookup gets a handle of the next generation.
func (n *nfsFS) RetireHandle(id uint64) bool {
	if id == rootID {
		return false
	}
	if _, _, ok := n.rt.locate(id); !ok {
		return false
	}
	n.handles.bump(id)
	return true
}

// Rename advances a regular file's mtime to stand in for the ctime change
// rename(2) implies: the NFS server reports ctime as mtime. macOS NFS needs
// that change to invalidate cached linker output, even when executable bits
// are restored only after renaming the temporary file.
//
// Other protocols must preserve mtime on rename; SMB clients may still have
// dirty output pages cached when they issue it.
func (n *nfsFS) Rename(from, to string) error {
	if err := n.nodeFS.Rename(from, to); err != nil {
		return err
	}
	if overlay.Clean(from) == overlay.Clean(to) {
		return nil
	}
	info, err := n.Filesystem.Lstat(to)
	if err != nil || !info.Mode().IsRegular() {
		return err
	}
	mtime := info.ModTime().Add(time.Nanosecond)
	return n.Change.Chtimes(to, mtime, mtime)
}

// The adapter is a complete NFS backend.
var _ nfs.Backend = (*nfsFS)(nil)
