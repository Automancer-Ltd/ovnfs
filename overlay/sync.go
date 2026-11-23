package overlay

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"

	"github.com/go-git/go-billy/v5"
	"github.com/stencil-hq/ovnfs/internal/durable"
	"github.com/stencil-hq/ovnfs/internal/hostfile"
	"github.com/stencil-hq/ovnfs/internal/ident"
)

// SyncFile makes a regular file's data and namespace durable. name is the
// last known path of id, and opened may be a descriptor in this overlay.
// Unlike handle Sync, this always spills a memory file, without affecting
// other files in the overlay or copying up an unchanged lower file.
func (f *FS) SyncFile(name string, id ident.ID, opened billy.File) error {
	p := Clean(name)
	f.await(p)
	err := f.rlockPaths(p)
	var src source
	if err == nil {
		p, src, err = f.syncSource(p, id)
	}
	for err == nil {
		if f.files[p] != nil {
			f.mu.RUnlock()
			if p, err = f.spillForSync(p, id); err != nil {
				return err
			}
		} else if op := f.busy(p); op != nil {
			// The entry moved to a name a host operation is changing.
			f.mu.RUnlock()
			f.mu.Lock()
			err = f.waitOp(op)
			f.mu.Unlock()
			if err != nil {
				return err
			}
		} else {
			break
		}
		if err = f.rlockPaths(p); err == nil {
			p, src, err = f.syncSource(p, id)
		}
	}
	defer f.mu.RUnlock()
	if err != nil {
		return err
	}
	if src.upper {
		// The directories synced below carry the times the index shows.
		if err := f.settleAncestors(p); err != nil {
			return err
		}
		var h *file
		switch v := opened.(type) {
		case *file:
			h = v
		case *memHandle:
			h = v.disk
		}
		var fd *os.File
		if h != nil && h.fs == f && h.id == id {
			fd = h.File
		}
		return syncHostFile(f.upper, p, fd)
	}
	if src.layer != nil {
		return src.layer.Content(src.path, func(_, blob string) error {
			if blob != "" {
				// A completed snapshot may still live in an unsynced pack.
				return src.layer.blobs.SyncBlob(blob)
			}
			return syncHostFile(src.layer.dir, src.path, nil)
		})
	}
	return nil // The bottom received no writes.
}

// syncSource verifies the path hint or finds the current name of a moved
// entry. The caller holds f.mu at least shared.
func (f *FS) syncSource(p string, id ident.ID) (string, source, error) {
	src, info, err := f.lookupRead(p)
	if err != nil && !missing(err) {
		return "", source{}, err
	}
	if err != nil || handleID(info) != id {
		var ok bool
		p, ok = f.locate(id)
		if !ok {
			return "", source{}, syscall.ESTALE
		}
		src, info, err = f.lookupRead(p)
		if err != nil {
			return "", source{}, err
		}
	}
	if info.IsDir() {
		return "", source{}, syscall.EISDIR
	}
	if !info.Mode().IsRegular() {
		return "", source{}, syscall.EINVAL
	}
	return p, src, nil
}

// spillForSync takes the exclusive lock only for the spill, not for the
// host sync. Recheck the identity since the name may have moved meanwhile.
func (f *FS) spillForSync(p string, id ident.ID) (string, error) {
	err := f.lockPaths(p)
	defer f.mu.Unlock()
	if err != nil {
		return p, err
	}
	p, _, err = f.syncSource(p, id)
	if err == nil {
		if m := f.files[p]; m != nil {
			err = f.spillAs(m, p)
		}
	}
	return p, err
}

func syncHostFile(root, p string, fd *os.File) (err error) {
	if fd == nil {
		host := filepath.Join(root, filepath.FromSlash(p))
		fd, err = hostfile.OpenFile(host, os.O_WRONLY, 0)
		if errors.Is(err, os.ErrPermission) {
			fd, err = hostfile.Open(host)
		}
		if err != nil {
			return err
		}
		defer func() {
			if closeErr := fd.Close(); err == nil {
				err = closeErr
			}
		}()
	}
	// Where xattrs are unavailable, the handle's identity lives in a log,
	// not on the data inode. Directory fsync alone does not flush that log.
	table, err := hostfile.OpenFile(filepath.Join(root, tableName), os.O_RDWR, 0)
	if err == nil {
		err = errors.Join(durable.Sync(table), table.Close())
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// A spill can publish a new name below copied-up directories. Sync the
	// names before the file's final sync (F_FULLFSYNC on Darwin).
	for dir := parent(p); ; dir = parent(dir) {
		if err := durable.Dir(filepath.Join(root, filepath.FromSlash(dir))); err != nil {
			return err
		}
		if dir == "" {
			break
		}
	}
	if err := durable.Dir(filepath.Dir(root)); err != nil {
		return err
	}
	return fd.Sync()
}
