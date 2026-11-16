package export

import (
	"errors"
	"os"
	"path"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/go-git/go-billy/v5"
	"github.com/stencil-hq/ovnfs/overlay"
)

// nodeFS gives both protocols stable node ids and keeps their paths in step
// with renames and removals, independently of overlay copy-up.
type nodeFS struct {
	billy.Filesystem
	billy.Change
	nodes    *nodeTable
	onRecall atomic.Pointer[func(id uint64)] // see Export.OnRecall
}

// ReadDir hides [overlay.AppleDouble] sidecars (fs::copy and cp copy their
// xattrs too). They must stay creatable and reachable by name, or xattr
// writes and therefore copies fail, but listing them would show tools like
// git and cargo a file per real file. Symlinks are typed as by [nodeFS.Lstat].
//
// The complete listing also tells the node table which children may take
// derived ids.
func (n *nodeFS) ReadDir(name string) ([]os.FileInfo, error) {
	if err := n.IdentityError(); err != nil {
		return nil, err
	}
	if err := n.nodes.admitListing(overlay.Clean(name)); err != nil {
		return nil, err
	}
	l := n.nodes.startListing(overlay.Clean(name))
	if err := n.IdentityError(); err != nil {
		return nil, err
	}
	infos, err := n.Filesystem.ReadDir(name)
	if err != nil {
		return nil, err
	}
	n.nodes.observe(l, infos)
	if err := n.IdentityError(); err != nil {
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
	if err := n.IdentityError(); err != nil {
		return nil, err
	}
	info, err := n.Filesystem.Lstat(name)
	if err != nil {
		return nil, err
	}
	return n.linked(name, info), nil
}

// Stat preserves the index fault instead of presenting it as an absent name.
func (n *nodeFS) Stat(name string) (os.FileInfo, error) {
	if err := n.IdentityError(); err != nil {
		return nil, err
	}
	return n.Filesystem.Stat(name)
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

// Link forwards hard links without coupling the protocol to the router type.
func (n *nodeFS) Link(oldname, newname string) (err error) {
	release, err := n.nodes.guard(false, overlay.Clean(newname))
	if err != nil {
		return err
	}
	defer func() {
		release()
		if fault := n.IdentityError(); fault != nil {
			err = fault
		}
	}()

	n.recall(oldname) // its link count changes
	fs, ok := n.Filesystem.(interface{ Link(string, string) error })
	if !ok {
		return syscall.ENOTSUP
	}
	if err := fs.Link(oldname, newname); err != nil {
		return err
	}
	n.nodes.created(overlay.Clean(newname))
	return nil
}

// Mknod creates a special node and records its node id.
func (n *nodeFS) Mknod(name string, mode os.FileMode) (err error) {
	release, err := n.nodes.guard(false, overlay.Clean(name))
	if err != nil {
		return err
	}
	defer func() {
		release()
		if fault := n.IdentityError(); fault != nil {
			err = fault
		}
	}()

	fs, ok := n.Filesystem.(interface {
		Mknod(string, os.FileMode) error
	})
	if !ok {
		return syscall.ENOTSUP
	}
	if err := fs.Mknod(name, mode); err != nil {
		return err
	}
	n.nodes.created(overlay.Clean(name))
	return nil
}

// MkdirAll creates a directory and notes the new entry for node ids.
func (n *nodeFS) MkdirAll(name string, perm os.FileMode) (err error) {
	release, err := n.nodes.guard(false, overlay.Clean(name))
	if err != nil {
		return err
	}
	defer func() {
		release()
		if fault := n.IdentityError(); fault != nil {
			err = fault
		}
	}()

	if err := n.Filesystem.MkdirAll(name, perm); err != nil {
		return err
	}
	n.nodes.created(overlay.Clean(name))
	return nil
}

// Symlink creates a symbolic link and notes the new entry for node ids.
func (n *nodeFS) Symlink(target, link string) (err error) {
	release, err := n.nodes.guard(false, overlay.Clean(link))
	if err != nil {
		return err
	}
	defer func() {
		release()
		if fault := n.IdentityError(); fault != nil {
			err = fault
		}
	}()

	if err := n.Filesystem.Symlink(target, link); err != nil {
		return err
	}
	n.nodes.created(overlay.Clean(link))
	return nil
}

// TempFile creates a uniquely named file and notes the new entry for node ids.
func (n *nodeFS) TempFile(dir, prefix string) (f billy.File, err error) {
	release, err := n.nodes.guard(true, overlay.Clean(dir))
	if err != nil {
		return nil, err
	}
	defer func() {
		release()
		if fault := n.IdentityError(); fault != nil {
			if f != nil {
				f.Close()
				f = nil
			}
			err = fault
		}
	}()

	f, err = n.Filesystem.TempFile(dir, prefix)
	if err != nil {
		return nil, err
	}
	n.nodes.created(overlay.Clean(f.Name()))
	return n.bindFile(f.Name(), f)
}

// Rename moves a node without changing its id or those of its descendants.
func (n *nodeFS) Rename(from, to string) (err error) {
	// Do not log nonexistent destination ancestors for a rename which cannot
	// reach its backend commit. Preparation may pin real directory identities.
	if _, err := n.Filesystem.Lstat(from); err != nil {
		return err
	}
	dir, _ := split(overlay.Clean(to))
	parent, err := n.Filesystem.Stat(dir)
	if err != nil {
		return err
	}
	if !parent.IsDir() {
		return syscall.ENOTDIR
	}
	release, err := n.nodes.prepareRename(overlay.Clean(from), overlay.Clean(to))
	if err != nil {
		return err
	}
	defer func() {
		release()
		if fault := n.IdentityError(); fault != nil {
			err = fault
		}
	}()

	n.recall(from, to)
	err = n.Filesystem.Rename(from, to)
	if errors.Is(err, syscall.ENOTEMPTY) {
		if info, statErr := n.Filesystem.Lstat(to); statErr == nil && info.IsDir() {
			err = n.retryWithoutSidecars(overlay.Clean(to), err, func() error {
				return n.Filesystem.Rename(from, to)
			})
		}
	}
	if err != nil {
		return err
	}
	src, dst := overlay.Clean(from), overlay.Clean(to)
	if src == dst {
		return nil
	}
	n.nodes.rename(src, dst)
	return nil
}

// Remove retires a deleted node and tolerates hidden sidecars in empty dirs.
func (n *nodeFS) Remove(name string) (err error) {
	release, err := n.nodes.guard(false, overlay.Clean(name))
	if err != nil {
		return err
	}
	defer func() {
		release()
		if fault := n.IdentityError(); fault != nil {
			err = fault
		}
	}()

	n.recall(name)
	err = n.Filesystem.Remove(name)
	if errors.Is(err, syscall.ENOTEMPTY) {
		err = n.retryWithoutSidecars(name, err, func() error {
			return n.Filesystem.Remove(name)
		})
	}
	if err != nil {
		return err
	}
	n.nodes.remove(overlay.Clean(name))
	return nil
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
		n.nodes.remove(overlay.Clean(name))
	}
	return true, nil
}
