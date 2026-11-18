package base

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/stencil-hq/ovnfs/git"
	"github.com/stencil-hq/ovnfs/internal/hostfile"
	"github.com/stencil-hq/ovnfs/overlay"
	"github.com/stencil-hq/ovnfs/store"
)

// Files from git (--rev, --take from the index or a revision) are blobs as git
// stores them, but for a file the lfs filter tracks git's checkout writes the
// object its pointer blob names instead. The capture keeps the pointer as the
// leaf, so trees, diffs and the revision index see what git stores, and records
// the object's size and lfsMtime. The first open reads the object from the
// repository's LFS storage, else from lfsDir, where `git lfs smudge` puts one
// the storage lacks.

// lfsDir, in a base's state directory, holds the LFS objects git lfs smudge
// produced for it.
const lfsDir = "lfs"

// lfsTimeout bounds how long an open waits for git lfs smudge, which may
// download the object.
const lfsTimeout = 10 * time.Minute

// lfsMtime is the mtime LFS objects present: the whole second before the
// fork's. The revision index is written after the fork, so git never finds
// them racily clean: the first status trusts their seeded stat data instead of
// reading, and so producing, every object.
func lfsMtime(fork time.Time) time.Time { return fork.Truncate(time.Second).Add(-time.Second) }

// lfsFinder recognizes, among the files git checks out, the pointers its
// checkout replaces by their objects. A nil finder, for a repository whose
// checkout leaves pointers in place, recognizes none.
type lfsFinder struct {
	attrs   *git.Attributes
	objects *git.Objects
}

// newLFSFinder reads repo's lfs filter configuration and, with opts, the
// attributes of the files' source.
func newLFSFinder(repo *git.Repo, opts git.AttributeOptions) (*lfsFinder, error) {
	if on, err := repo.LFSCheckout(); err != nil || !on {
		return nil, err
	}
	attrs, err := repo.Attributes([]string{"filter"}, opts)
	if err != nil {
		return nil, err
	}
	objects, err := repo.Objects()
	if err != nil {
		return nil, err
	}
	return &lfsFinder{attrs: attrs, objects: objects}, nil
}

// object returns the size of the object git's checkout writes for e; false
// when it writes e's blob. A blob missing locally (a partial clone) counts as
// content: it is read, and fails, only when opened.
func (l *lfsFinder) object(e git.TreeEntry) (int64, bool, error) {
	if l == nil || e.Type != "blob" || e.Mode == store.ModeSymlink || e.Size >= git.LFSPointerLimit {
		return 0, false, nil
	}
	values, err := l.attrs.Check(e.Path)
	if err != nil || values[0] != "lfs" {
		return 0, false, err
	}
	data, err := l.objects.Blob(e.OID)
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	pointer, ok := git.ParseLFSPointer(data)
	return pointer.Size, ok, nil
}

// Close releases the object reader.
func (l *lfsFinder) Close() {
	if l != nil {
		l.objects.Close()
	}
}

// LFS reports whether the view serves the leaf at p, a Git LFS pointer, as the
// object it names; Info then has the object's size, not the pointer's.
func (b *Base) LFS(p string) bool {
	n, ok := b.index.lookup(overlay.Clean(p))
	return ok && b.index.lfs(n)
}

// lfsFile serves the object the pointer blob id at p names: from the root
// repository's LFS storage, else from lfsDir, which git lfs smudge fills on
// first need. An object git lfs cannot produce is reported as a fault and
// reads as EIO.
func (b *Base) lfsFile(p string, id store.OID) (overlay.Content, error) {
	data, err := b.Blob(p, id)
	if err != nil {
		return nil, err
	}
	pointer, ok := git.ParseLFSPointer(data)
	repo := b.domains.Repo("")
	if !ok || repo == nil {
		return nil, fmt.Errorf("the LFS object of %s (%s) is no longer available", p, id)
	}
	if name, err := repo.LFSObject(pointer.OID); err == nil {
		if f, ok := openSized(name, pointer.Size); ok {
			return f, nil
		}
	}
	name := filepath.Join(b.config.StateDir, lfsDir, pointer.OID)
	if f, ok := openSized(name, pointer.Size); ok {
		return f, nil
	}
	err = smudge(repo, p, data, pointer.Size, name)
	// A concurrent open may have produced the object even if this one failed.
	if f, ok := openSized(name, pointer.Size); ok {
		return f, nil
	}
	if err == nil {
		err = errors.New("the produced object vanished")
	}
	if b.config.OnFault != nil {
		b.config.OnFault(fmt.Sprintf("LFS object of %s: %v", p, err))
	}
	return nil, &fs.PathError{Op: "smudge", Path: p, Err: syscall.EIO}
}

// smudge has git lfs smudge write the object pointer names for p to name,
// which appears only complete and with the size the pointer promises.
func smudge(repo *git.Repo, p string, pointer []byte, size int64, name string) error {
	if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(name), ".smudge-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	ctx, cancel := context.WithTimeout(context.Background(), lfsTimeout)
	defer cancel()
	err = repo.SmudgeLFS(ctx, p, pointer, tmp)
	if err == nil {
		var info os.FileInfo
		if info, err = tmp.Stat(); err == nil && info.Size() != size {
			err = fmt.Errorf("git lfs smudge wrote %d bytes, not the %d its pointer names", info.Size(), size)
		}
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), name)
}

// openSized opens the regular host file name when it holds size bytes.
func openSized(name string, size int64) (*os.File, bool) {
	f, err := hostfile.Open(name)
	if err != nil {
		return nil, false
	}
	if info, err := f.Stat(); err == nil && info.Mode().IsRegular() && info.Size() == size {
		return f, true
	}
	f.Close()
	return nil, false
}
