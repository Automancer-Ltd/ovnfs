package export

import (
	"os"

	"github.com/stencil-hq/ovnfs/checkout"
	"github.com/stencil-hq/ovnfs/internal/ident"
	"github.com/stencil-hq/ovnfs/smb"
)

// A file id, which NFS handles and fileids, SMB file ids and FUSE/WinFsp
// inode numbers carry, is the entry's backing-store identity (ident.ID)
// under its checkout's slot (checkout.Registry.Slot):
//
//	slot<<ident.Bits | identity
//
// An entry keeps its identity through copy-up, spill, freeze and rename, and
// its checkout keeps its persisted slot, so the id never changes for the
// entry's whole life, across daemon restarts too. Handing one out needs no
// listing and no record: the id is read off the entry's file information. An
// id resolves back to a path through the checkout's overlay
// ([overlay.FS.Locate]); one that names no visible entry is stale.
//
// Slot 0 is the export's virtual root: the root itself and the [neverIndex]
// marker. Twelve slot bits keep every id below 2^53, which JavaScript runtimes
// represent exactly.
const (
	rootID   uint64 = 1 // the export root
	markerID uint64 = 2 // the [neverIndex] file
)

// identityMask selects the identity of a file id.
const identityMask = 1<<ident.Bits - 1

// fileID packs checkout slot and an entry's identity into a file id.
func fileID(slot uint16, id ident.ID) uint64 { return uint64(slot)<<ident.Bits | uint64(id) }

// splitFileID returns the slot and identity a file id packs.
func splitFileID(fid uint64) (uint16, ident.ID) {
	return uint16(fid >> ident.Bits), ident.ID(fid & identityMask)
}

// slotIDs returns the first and last file id a checkout in slot can have.
func slotIDs(slot uint16) (first, last uint64) {
	first = uint64(slot) << ident.Bits
	return first, first | identityMask
}

// entryID returns the file id of the entry of checkout co that info
// describes; 0 when co is not live or info carries no identity.
func (r *Router) entryID(co string, info os.FileInfo) uint64 {
	slot, _, ok := r.reg.Slot(co)
	if !ok {
		return 0
	}
	id, ok := ident.Of(info)
	if !ok {
		return 0
	}
	return fileID(slot, id)
}

// idOf returns the file id of the entry at export path p that info
// describes, without a syscall.
func (r *Router) idOf(p string, info os.FileInfo) uint64 {
	co, rest := splitRouterPath(p)
	switch {
	case co == "":
		return rootID
	case marker(co, rest):
		return markerID
	}
	return r.entryID(co, info)
}

// childID returns the file id of the entry of directory dir that info, from
// a listing of dir, describes, without a syscall or joining their paths.
func (r *Router) childID(dir string, info os.FileInfo) uint64 {
	co, _ := splitRouterPath(dir)
	if co == "" {
		co = info.Name()
		if co == neverIndex {
			return markerID
		}
	}
	return r.entryID(co, info)
}

// id returns the file id of the entry at export path p; 0 when there is none.
func (r *Router) id(p string) uint64 {
	co, rest := splitRouterPath(p)
	switch {
	case co == "":
		return rootID
	case marker(co, rest):
		return markerID
	}
	info, err := r.Lstat(p)
	if err != nil {
		return 0
	}
	return r.entryID(co, info)
}

// locate returns the current export path of the entry with file id fid and
// the life of its checkout (checkout.Registry.Slot); false when no visible
// entry has that id. The virtual root's entries have life 0. An id resolved
// before, with nothing moved or removed since, costs no syscall ([pathCache]).
func (r *Router) locate(fid uint64) (p string, life uint32, ok bool) {
	switch fid {
	case rootID:
		return "", 0, true
	case markerID:
		return neverIndex, 0, true
	}
	slot, id := splitFileID(fid)
	if slot == 0 || id.Space() == ident.None {
		return "", 0, false
	}
	var s checkout.Session
	name, life, err := r.reg.EnterSlot(slot, &s)
	if err != nil {
		return "", 0, false
	}
	defer s.Done()
	// Read before resolving: a change from here on invalidates what is cached.
	names, invalid := s.FS.Names(), r.paths.invalid.Load()
	if p, ok := r.paths.get(fid, s.FS, life, names, invalid); ok {
		return p, life, true
	}
	rel, ok := s.FS.Locate(id)
	if !ok {
		return "", 0, false
	}
	p = name
	if rel != "" {
		p = name + "/" + rel
	}
	r.paths.put(fid, p, s.FS, life, names, invalid)
	return p, life, true
}

// Nodes is the export for frontends that address files by file id (FUSE
// and WinFsp inode numbers, remote-fs). Its filesystem and metadata
// calls recall other protocols' cached copies, as every export view does.
type Nodes struct {
	*nodeFS
	volume string
}

// Nodes exposes the routed filesystem with its file ids.
func (e *Export) Nodes() *Nodes { return &Nodes{nodeFS: e.fs, volume: e.volume} }

// ID returns the file id of the entry at export path p, the export root's
// being 1; 0 when p does not exist or carries no identity. It costs an
// Lstat: a frontend that has p's FileInfo uses [Nodes.IDOf].
func (n *Nodes) ID(p string) uint64 { return n.rt.id(p) }

// IDOf returns the file id of the entry at export path p that info, from
// this export's Lstat, Stat or open file Stat of p, describes. It makes no
// syscall; 0 when the id cannot be determined.
func (n *Nodes) IDOf(p string, info os.FileInfo) uint64 { return n.rt.idOf(p, info) }

// ChildID returns the file id of the entry info, from this export's ReadDir
// of export directory dir, describes; like [Nodes.IDOf] it makes no syscall.
func (n *Nodes) ChildID(dir string, info os.FileInfo) uint64 { return n.rt.childID(dir, info) }

// Path returns the current export-relative path of the entry with file id
// id; false once no visible entry has it (removed, or its checkout gone).
func (n *Nodes) Path(id uint64) (string, bool) {
	p, _, ok := n.rt.locate(id)
	return p, ok
}

// Statfs reports space on the upper volume, where all overlay writes land.
func (n *Nodes) Statfs() (smb.Statfs, error) { return volumeStatfs(n.volume) }
