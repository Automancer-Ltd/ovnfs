package base

import (
	"os"

	"github.com/stencil-hq/ovnfs/internal/ident"
)

// Every entry a base serves carries its identity (see package ident): a
// captured entry, source or root, its Capture identity, the number of its
// capture index node, whether the folder's file or the captured blob serves
// it; an entry below a root, the State identity of the file serving it, which
// is the cache snapshot's copy below a local root and the folder's own file
// below a cache root.

// captureID is the identity of capture index node n.
func captureID(n uint32) ident.ID {
	id, _ := ident.New(ident.Capture, uint64(n))
	return id
}

// hostInfo is the folder's own metadata of a captured entry it still holds
// unchanged, source or root, carrying the entry's Capture identity instead
// of the folder inode's.
type hostInfo struct {
	os.FileInfo
	id ident.ID
}

// Identity returns the entry's Capture identity.
func (i hostInfo) Identity() ident.ID { return i.id }

// nodePath returns the path of capture index node v, when it is one.
func (b *Base) nodePath(v uint64) (string, bool) {
	if v >= uint64(b.index.nodes) {
		return "", false
	}
	return b.index.pathOf(uint32(v)), true
}
