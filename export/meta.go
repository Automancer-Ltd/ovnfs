package export

import (
	"errors"
	"os"
	"path"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/go-git/go-billy/v5"
	"github.com/stencil-hq/ovnfs/internal/ident"
	"github.com/stencil-hq/ovnfs/overlay"
)

// nodeFS is the routed filesystem every export view serves: it hides
// AppleDouble sidecars from listings, types symlinks for Windows clients and
// recalls other protocols' cached copies of what it changes.
type nodeFS struct {
	billy.Filesystem
	billy.Change
	rt       *Router                      // resolves file ids
	onRecall atomic.Pointer[func(Recall)] // see Export.OnRecall; nil in protocol views
}

// ReadDir hides [overlay.AppleDouble] sidecars (fs::copy and cp copy their
// xattrs too). They must stay creatable and reachable by name, or xattr
// writes and therefore copies fail, but listing them would show tools like
// git and cargo a file per real file. Symlinks are typed as by [nodeFS.Lstat].
func (n *nodeFS) ReadDir(name string) ([]os.FileInfo, error) {
	infos, err := n.Filesystem.ReadDir(name)
	if err != nil {
		return nil, err
	}
	out := infos[:0]
	for _, info := range infos {
		if overlay.AppleDouble(info.Name()) {
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 {
			// Only a link needs its path, to resolve its target on demand.
			info = n.linked(path.Join(name, info.Name()), info)
		}
		out = append(out, info)
	}
	return out, nil
}

// Lstat describes name without following it; see [link] for symlinks.
func (n *nodeFS) Lstat(name string) (os.FileInfo, error) {
	info, err := n.Filesystem.Lstat(name)
	if err != nil {
		return nil, err
	}
	return n.linked(name, info), nil
}

// linked returns a symlink's info as a [link]; other infos pass through.
func (n *nodeFS) linked(p string, info os.FileInfo) os.FileInfo {
	if info.Mode()&os.ModeSymlink == 0 {
		return info
	}
	return &link{FileInfo: info, fs: n.Filesystem, path: p}
}

// link is a symlink's metadata that resolves its target only when asked
// whether it is a directory, which only the frontends serving Windows
// clients do: Windows enters a link only when it is marked as a directory.
type link struct {
	os.FileInfo
	fs   billy.Filesystem
	path string
	once sync.Once
	dir  bool
}

// LinksToDir implements [ntinfo.DirLink].
func (l *link) LinksToDir() bool {
	l.once.Do(func() {
		target, err := l.fs.Stat(l.path)
		l.dir = err == nil && target.IsDir()
	})
	return l.dir
}

// Identity forwards the link's own identity, which embedding hides.
func (l *link) Identity() ident.ID {
	id, _ := ident.Of(l.FileInfo)
	return id
}

// Link forwards hard links without coupling the protocol to the router type.
func (n *nodeFS) Link(oldname, newname string) error {
	n.recall(oldname) // its link count changes
	fs, ok := n.Filesystem.(interface{ Link(string, string) error })
	if !ok {
		return syscall.ENOTSUP
	}
	return fs.Link(oldname, newname)
}

// Mknod creates a socket or FIFO.
func (n *nodeFS) Mknod(name string, mode os.FileMode) error {
	fs, ok := n.Filesystem.(interface {
		Mknod(string, os.FileMode) error
	})
	if !ok {
		return syscall.ENOTSUP
	}
	return fs.Mknod(name, mode)
}

// Rename moves an entry, which keeps its file id, after recalling cached
// copies of it and of an entry it replaces. A destination directory left
// with only hidden sidecars counts as empty.
func (n *nodeFS) Rename(from, to string) error {
	n.recall(from, to)
	err := n.Filesystem.Rename(from, to)
	if errors.Is(err, syscall.ENOTEMPTY) {
		if info, statErr := n.Filesystem.Lstat(to); statErr == nil && info.IsDir() {
			err = n.retryWithoutSidecars(overlay.Clean(to), err, func() error {
				return n.Filesystem.Rename(from, to)
			})
		}
	}
	return err
}

// Remove removes an entry after recalling cached copies of it, and
// tolerates hidden sidecars in a directory that is otherwise empty.
func (n *nodeFS) Remove(name string) error {
	n.recall(name)
	err := n.Filesystem.Remove(name)
	if errors.Is(err, syscall.ENOTEMPTY) {
		err = n.retryWithoutSidecars(name, err, func() error {
			return n.Filesystem.Remove(name)
		})
	}
	return err
}

// retryWithoutSidecars handles notEmpty, the ENOTEMPTY of an operation on
// dir. It runs retry once after it drops the sidecars of dir. It keeps
// notEmpty when dir holds a visible entry, and it returns the cleanup error
// when a sidecar removal fails, because a partial cleanup is not success.
func (n *nodeFS) retryWithoutSidecars(dir string, notEmpty error, retry func() error) error {
	dropped, err := n.dropAppleDouble(dir)
	if err != nil {
		return err
	}
	if !dropped {
		return notEmpty
	}
	return retry()
}

// dropAppleDouble removes the "._*" files from dir if nothing else is in it.
// Clients cannot see them (ReadDir hides them), so to the client the
// directory is empty and rmdir must succeed. It returns false and no error
// when dir holds a visible entry, and the first listing or removal error.
func (n *nodeFS) dropAppleDouble(dir string) (bool, error) {
	infos, err := n.Filesystem.ReadDir(dir)
	if err != nil {
		return false, err
	}
	for _, info := range infos {
		if !overlay.AppleDouble(info.Name()) {
			return false, nil
		}
	}
	for _, info := range infos {
		name := path.Join(dir, info.Name())
		if err := n.Filesystem.Remove(name); err != nil {
			return false, err
		}
	}
	return true, nil
}
