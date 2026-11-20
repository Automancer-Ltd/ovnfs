//go:build darwin || linux

package base

import (
	"os"
	"path/filepath"
	"sync"

	"github.com/stencil-hq/ovnfs/internal/search/fsys"
	"golang.org/x/sys/unix"
)

// tree is a cache snapshot's tree directory, held open so that lookups below
// it resolve relative to it (fstatat, openat, readlinkat): the snapshot's
// absolute path is a dozen components deep, and walking it for every lookup
// cost a third of each one. Relative lookups follow symlinks in intermediate
// components, so callers prove those directories first (see [Caches.dirsBelow]).
type tree struct {
	dir    string
	mu     sync.RWMutex // held shared by lookups using fd, exclusively to close it
	fd     int          // -1 until opened, and once closed
	closed bool
}

// newTree describes the tree directory dir, not yet opened.
func newTree(dir string) *tree { return &tree{dir: dir, fd: -1} }

// open opens the tree directory, which must exist; a closed tree stays closed.
func (t *tree) open() error {
	fd, err := unix.Open(t.dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return &os.PathError{Op: "open", Path: t.dir, Err: err}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		unix.Close(fd)
		return nil
	}
	t.fd = fd
	return nil
}

// close releases the directory; later lookups find nothing.
func (t *tree) close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.fd >= 0 {
		unix.Close(t.fd)
	}
	t.fd, t.closed = -1, true
}

// lstat describes slash path rel below the tree without following a final
// symlink; its Sys is the *syscall.Stat_t whose inode is the State identity.
func (t *tree) lstat(rel string, l *locator) (os.FileInfo, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.fd < 0 {
		return nil, absent(rel)
	}
	info, err := fsys.Lstatat(t.fd, rel)
	if err == nil && l != nil {
		l.seen(info, rel)
	}
	return info, err
}

// openFile opens regular file rel below the tree for reading.
func (t *tree) openFile(rel string) (*os.File, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.fd < 0 {
		return nil, absent(rel)
	}
	fd, err := unix.Openat(t.fd, rel, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: rel, Err: err}
	}
	return os.NewFile(uintptr(fd), filepath.Join(t.dir, rel)), nil
}

// readlink returns the target of symlink rel below the tree.
func (t *tree) readlink(rel string) (string, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.fd < 0 {
		return "", absent(rel)
	}
	for size := 256; ; size *= 2 {
		buf := make([]byte, size)
		n, err := unix.Readlinkat(t.fd, rel, buf)
		if err != nil {
			return "", &os.PathError{Op: "readlink", Path: rel, Err: err}
		}
		if n < size {
			return string(buf[:n]), nil
		}
	}
}
