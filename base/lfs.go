package base

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
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
// the storage lacks. Comparisons of work-tree content with such a leaf go
// through LFSClean.

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
	filter  *git.LFSFilter
	attrs   *git.Attributes
	objects *git.Objects
}

// newLFSFinder reads repo's lfs filter configuration and, with opts, the
// attributes of the files' source.
func newLFSFinder(repo *git.Repo, opts git.AttributeOptions) (*lfsFinder, error) {
	filter, err := repo.LFSFilter()
	if err != nil || filter == nil {
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
	return &lfsFinder{filter: filter, attrs: attrs, objects: objects}, nil
}

// object returns the object git's checkout writes for e, and whether e's blob
// is that pointer's canonical encoding, which git's clean filter gives back;
// false when checkout writes e's blob. A blob missing locally (a partial
// clone) counts as content: it is read, and fails, only when opened.
func (l *lfsFinder) object(e git.TreeEntry) (pointer git.LFSPointer, canonical, ok bool, err error) {
	if l == nil || e.Type != "blob" || e.Mode == store.ModeSymlink || e.Size >= git.LFSPointerLimit || !l.filter.Smudges(e.Path) {
		return pointer, false, false, nil
	}
	values, err := l.attrs.Check(e.Path)
	if err != nil || values[0] != "lfs" {
		return pointer, false, false, err
	}
	data, err := l.objects.Blob(e.OID)
	if errors.Is(err, os.ErrNotExist) {
		return pointer, false, false, nil
	}
	if err != nil {
		return pointer, false, false, err
	}
	pointer, ok = git.ParseLFSPointer(data)
	return pointer, ok && string(data) == pointer.String(), ok, nil
}

// Close releases the object reader.
func (l *lfsFinder) Close() {
	if l != nil {
		l.objects.Close()
	}
}

// LFS reports whether the view serves the leaf at p, a Git LFS pointer, as the
// object it names.
func (b *Base) LFS(p string) bool {
	n, ok := b.index.lookup(overlay.Clean(p))
	return ok && b.index.lfs(n)
}

// LFSPointer returns the pointer blob of the leaf at p when it is a Git LFS
// pointer the view serves as the object it names (whose size Info reports);
// false for any other path.
func (b *Base) LFSPointer(p string) ([]byte, bool, error) {
	p = overlay.Clean(p)
	n, ok := b.index.lookup(p)
	if !ok || !b.index.lfs(n) {
		return nil, false, nil
	}
	data, err := b.Blob(p, b.index.oid(n))
	return data, err == nil, err
}

// LFSLeaf reports whether id is the LFS pointer the view serves at p as the
// object it names: content whose work-tree bytes Content, not the store or
// Blob, supplies.
func (b *Base) LFSLeaf(p string, id store.OID) bool {
	n, ok := b.index.lookup(overlay.Clean(p))
	return ok && b.index.lfs(n) && b.index.oid(n) == id
}

// Content returns the work-tree bytes of the leaf id at p: the object the
// view serves when id is the LFS pointer it holds there (LFSLeaf), else the
// blob (Blob), which is the stored form a patch, an attach or a merge base
// record needs instead.
func (b *Base) Content(p string, id store.OID) ([]byte, error) {
	p = overlay.Clean(p)
	if !b.LFSLeaf(p, id) {
		return b.Blob(p, id)
	}
	c, err := b.lfsFile(p, id)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	return io.ReadAll(c)
}

// LFSClean returns the pointer blob id of the LFS leaf at p when content of
// size bytes, which open reads, is the object it names: content git's clean
// filter stores as that very pointer, and so unchanged. False for any other
// content or path.
func (b *Base) LFSClean(p string, size int64, open func() (io.ReadCloser, error)) (store.OID, bool, error) {
	p = overlay.Clean(p)
	n, ok := b.index.lookup(p)
	if !ok || !b.index.lfs(n) {
		return "", false, nil
	}
	id := b.index.oid(n)
	data, err := b.Blob(p, id)
	if err != nil {
		return "", false, err
	}
	pointer, ok := git.ParseLFSPointer(data)
	if !ok || pointer.Size != size {
		return "", false, nil
	}
	r, err := open()
	if err != nil {
		return "", false, err
	}
	defer r.Close()
	same, err := pointer.Names(r)
	return id, same, err
}

// lfsFile serves the object the pointer blob id at p names: from the root
// repository's LFS storage, else from lfsDir, which git lfs smudge fills on
// first need, one object at a time. An object git lfs cannot produce is
// reported as a fault and reads as EIO.
func (b *Base) lfsFile(p string, id store.OID) (overlay.Content, error) {
	data, err := b.Blob(p, id)
	if err != nil {
		return nil, err
	}
	pointer, ok := git.ParseLFSPointer(data)
	if !ok {
		return nil, fmt.Errorf("%s: blob %s is no Git LFS pointer", p, id)
	}
	repo := b.domains.Repo("")
	cached := filepath.Join(b.config.StateDir, lfsDir, pointer.OID)
	open := func() (*os.File, bool) {
		if f, ok := lfsStored(repo, pointer); ok {
			return f, true
		}
		return openSized(cached, pointer.Size)
	}
	if f, ok := open(); ok {
		return f, nil
	}
	if repo == nil {
		return nil, fmt.Errorf("the LFS object of %s (%s) is no longer available", p, id)
	}
	stripe, _ := strconv.ParseUint(pointer.OID[:2], 16, 8)
	mu := &b.lfsLocks[stripe%uint64(len(b.lfsLocks))]
	mu.Lock()
	defer mu.Unlock()
	if f, ok := open(); ok {
		return f, nil // another open produced it meanwhile
	}
	err = smudge(repo, p, data, pointer, cached)
	if f, ok := open(); ok {
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

// lfsStored opens the object pointer names in repo's LFS storage, if it holds
// it whole.
func lfsStored(repo *git.Repo, pointer git.LFSPointer) (*os.File, bool) {
	if repo == nil {
		return nil, false
	}
	name, err := repo.LFSObject(pointer.OID)
	if err != nil {
		return nil, false
	}
	return openSized(name, pointer.Size)
}

// smudge has git lfs smudge produce the object pointer (blob data) names for
// p. git-lfs keeps a downloaded object in the repository's storage; only where
// that did not happen is the verified output kept, at name.
func smudge(repo *git.Repo, p string, data []byte, pointer git.LFSPointer, name string) error {
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
	err = repo.SmudgeLFS(ctx, p, data, tmp)
	if err == nil {
		_, err = tmp.Seek(0, io.SeekStart)
	}
	if err == nil {
		// git-lfs may write the pointer back instead (skipped or failed
		// downloads), which can even have the object's size.
		var same bool
		if same, err = pointer.Names(tmp); err == nil && !same {
			err = errors.New("git lfs smudge wrote other content than the object its pointer names")
		}
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if f, ok := lfsStored(repo, pointer); ok {
		return f.Close()
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
