package export

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"sync"
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
	// Retired distinguishes the end of a checkout's life from an ordinary
	// invalidation, which may also recall its entire slot.
	Retired bool
}

// OnRecall registers the frontend's cache invalidation hook; daemon changes
// and changes through other export views invoke fn, which must not block.
func (e *Export) OnRecall(fn func(Recall)) { e.fs.onRecall.Store(&fn) }

// OnInvalidate registers fn to run whenever the daemon has changed checkout
// name behind the file protocols ([Export.Invalidate]) or retired it
// ([Export.Retire]), and returns a function that unregisters it. Unlike
// [Export.OnRecall] it takes any number of observers, and changes made
// through export views never reach them, so a frontend can tell the
// daemon's changes from its own. fn runs after the change, possibly under
// the registry's lock, so it must not block or call into the registry.
func (e *Export) OnInvalidate(name string, fn func()) (cancel func()) {
	return e.watchers.add(name, fn)
}

// invalidations holds the OnInvalidate observers by checkout name.
type invalidations struct {
	mu   sync.Mutex
	next uint64
	fns  map[string]map[uint64]func()
}

// add registers fn for checkout name and returns its removal.
func (w *invalidations) add(name string, fn func()) func() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.fns == nil {
		w.fns = make(map[string]map[uint64]func())
	}
	if w.fns[name] == nil {
		w.fns[name] = make(map[uint64]func())
	}
	w.next++
	key := w.next
	w.fns[name][key] = fn
	return func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		delete(w.fns[name], key)
		if len(w.fns[name]) == 0 {
			delete(w.fns, name)
		}
	}
}

// notify runs the observers of checkout name.
func (w *invalidations) notify(name string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, fn := range w.fns[name] {
		fn()
	}
}

// Quiet is the [Nodes] view without the recall hook, for a frontend that
// recalls other protocols' cached copies itself ([Export.Recall]) by the
// file ids it already holds: the hook finds each changed path's id with a
// stat of its own, which a request that just stated the path repeats.
func (e *Export) Quiet() *Nodes {
	return &Nodes{nodeFS: &nodeFS{Filesystem: e.fs.Filesystem, Change: e.fs.Change, rt: e.rt}, volume: e.volume}
}

// Recall tells the other protocols that their cached copies of file id, at
// export path p, are stale, as the Nodes view does before each change it
// makes. Id 0 names nothing and recalls nothing.
func (e *Export) Recall(id uint64, p string) {
	if fn := e.fs.onRecall.Load(); fn != nil && id != 0 {
		(*fn)(Recall{First: id, Last: id, Path: p})
	}
}

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
	e.watchers.notify(checkout)
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
