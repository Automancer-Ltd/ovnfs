package base

import (
	"os"
	"path/filepath"

	"github.com/stencil-hq/ovnfs/internal/hostfile"
)

// tree is a cache snapshot's tree directory. Windows has no lookups relative
// to an open directory in its file API, so it resolves full paths below dir.
type tree struct{ dir string }

// newTree describes the tree directory dir.
func newTree(dir string) *tree { return &tree{dir: dir} }

// open does nothing: lookups take full paths.
func (t *tree) open() error { return nil }

// close does nothing: no handle is held.
func (t *tree) close() {}

// lstat describes slash path rel below the tree without following a final
// symlink or junction, with its State identity (see [stateInfo]).
func (t *tree) lstat(rel string) (os.FileInfo, error) {
	host := t.path(rel)
	info, err := hostfile.Lstat(host)
	if err != nil {
		return nil, err
	}
	return stateInfo(info, host), nil
}

// openFile opens regular file rel below the tree for reading.
func (t *tree) openFile(rel string) (*os.File, error) { return hostfile.Open(t.path(rel)) }

// readlink returns the target of symlink rel below the tree.
func (t *tree) readlink(rel string) (string, error) { return os.Readlink(t.path(rel)) }

// path names rel below the tree.
func (t *tree) path(rel string) string { return filepath.Join(t.dir, filepath.FromSlash(rel)) }
