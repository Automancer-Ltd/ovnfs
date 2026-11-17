package export

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"time"

	"github.com/go-git/go-billy/v5"
)

// Recall is what clients must drop their cached copies of: the entries with
// file ids First through Last (First == Last for one entry; a checkout's
// whole slot for a tree), all at or below export path Path. A frontend that
// addresses files by id (NFS) uses the ids, a path-addressed one (SMB) Path.
type Recall struct {
	First, Last uint64
	Path        string
}

// OnRecall registers the frontend's cache invalidation hook; daemon changes
// and changes through other export views invoke fn, which must not block.
func (e *Export) OnRecall(fn func(Recall)) { e.fs.onRecall.Store(&fn) }

// recall tells the protocol layer that clients' cached copies of the named
// export paths are stale. Paths that do not exist have nothing cached.
func (n *nodeFS) recall(paths ...string) {
	fn := n.onRecall.Load()
	if fn == nil {
		return
	}
	for _, p := range paths {
		if id := n.rt.id(p); id != 0 {
			(*fn)(Recall{First: id, Last: id, Path: p})
		}
	}
}

// OpenFile recalls cached copies before any open that may change the file.
func (n *nodeFS) OpenFile(name string, flag int, perm os.FileMode) (billy.File, error) {
	if flag&routerWriteFlags != 0 {
		n.recall(name)
	}
	return n.Filesystem.OpenFile(name, flag, perm)
}

// Create truncates through OpenFile so the recall is not bypassed.
func (n *nodeFS) Create(name string) (billy.File, error) {
	return n.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o666)
}

// Chmod changes permissions after recalling cached attributes.
func (n *nodeFS) Chmod(name string, mode os.FileMode) error {
	n.recall(name)
	return n.Change.Chmod(name, mode)
}

// Lchown changes a link's ownership after recalling cached attributes.
func (n *nodeFS) Lchown(name string, uid, gid int) error {
	n.recall(name)
	return n.Change.Lchown(name, uid, gid)
}

// Chown changes ownership after recalling cached attributes.
func (n *nodeFS) Chown(name string, uid, gid int) error {
	n.recall(name)
	return n.Change.Chown(name, uid, gid)
}

// Chtimes changes times after recalling cached attributes.
func (n *nodeFS) Chtimes(name string, atime, mtime time.Time) error {
	n.recall(name)
	return n.Change.Chtimes(name, atime, mtime)
}

// Invalidate recalls clients' cached copies of checkout paths the daemon
// changed behind the file protocols (pull, reattach), and of everything at
// or below trees ("" covers the whole checkout).
//
// A changed path that still exists is recalled by its file id. One that is
// gone, like every tree, is recalled over the checkout's whole slot: its id
// can no longer be read, and an id never says which tree its entry is in.
// The handles of a gone path need no retiring, since an id that names no
// visible entry is stale; the paths ids resolved to are resolved again.
func (e *Export) Invalidate(checkout string, paths, trees []string) {
	// The daemon may have moved or removed entries behind the overlay.
	e.rt.paths.invalid.Add(1)
	fn := e.fs.onRecall.Load()
	if fn == nil {
		return
	}
	slot, _, ok := e.rt.reg.Slot(checkout)
	if !ok {
		return
	}
	first, last := slotIDs(slot)
	for _, t := range trees {
		(*fn)(Recall{First: first, Last: last, Path: path.Join(checkout, t)})
	}
	for _, p := range paths {
		full := path.Join(checkout, p)
		info, err := e.rt.Lstat(full)
		switch {
		case err == nil:
			if id := e.rt.entryID(checkout, info); id != 0 {
				(*fn)(Recall{First: id, Last: id, Path: full})
			}
		case errors.Is(err, fs.ErrNotExist):
			(*fn)(Recall{First: first, Last: last, Path: full})
		}
	}
}
