package base

import (
	"os"
	"path/filepath"
	"sync/atomic"

	"github.com/stencil-hq/ovnfs/internal/hostfile"
)

// tree is a cache snapshot's tree directory. Windows has no lookups relative
// to an open directory in its file API, so it resolves full paths below dir.
type tree struct {
	dir    string
	closed atomic.Bool
}

// newTree describes the tree directory dir.
func newTree(dir string) *tree { return &tree{dir: dir} }

// open does nothing: lookups take full paths.
func (t *tree) open() error { return nil }

// close holds no handle to release; later lookups find nothing, as where one
// is held.
func (t *tree) close() { t.closed.Store(true) }

// lstat describes slash path rel below the tree without following a final
// symlink or junction. Entries exposed to clients use l for their State identity.
func (t *tree) lstat(rel string, l *locator) (os.FileInfo, error) {
	if t.closed.Load() {
		return nil, absent(rel)
	}
	host := t.path(rel)
	info, err := hostfile.Lstat(host)
	if err == nil && l != nil {
		info = l.lookupInfo(info, host, rel)
	}
	return info, err
}

// openFile opens regular file rel below the tree for reading.
func (t *tree) openFile(rel string) (*os.File, error) {
	if t.closed.Load() {
		return nil, absent(rel)
	}
	return hostfile.Open(t.path(rel))
}

// readlink returns the target of symlink rel below the tree.
func (t *tree) readlink(rel string) (string, error) {
	if t.closed.Load() {
		return "", absent(rel)
	}
	return os.Readlink(t.path(rel))
}

// path names rel below the tree.
func (t *tree) path(rel string) string { return filepath.Join(t.dir, filepath.FromSlash(rel)) }
