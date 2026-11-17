// Package dirbottom provides a live host-directory bottom for overlay tests.
package dirbottom

import (
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/stencil-hq/ovnfs/internal/clone"
	"github.com/stencil-hq/ovnfs/internal/hostfile"
	"github.com/stencil-hq/ovnfs/internal/ident"
	"github.com/stencil-hq/ovnfs/overlay"
)

// bottom exposes ordinary host entries without interpreting overlay whiteouts.
// Like a base's capture, it numbers its entries by path: each path gets the
// next [ident.Capture] identity the first time the bottom reports it, which
// it keeps while the bottom lives.
type bottom struct {
	dir   string
	mu    sync.Mutex
	ids   map[string]ident.ID // path → identity
	paths map[ident.ID]string // identity → path
}

// New returns a read-only overlay view of dir; mutations occur only through CopyTo's destination.
func New(dir string) overlay.Bottom {
	return &bottom{dir: dir, ids: make(map[string]ident.ID), paths: make(map[ident.ID]string)}
}

// info is host metadata with the identity of the entry's path.
type info struct {
	os.FileInfo
	id ident.ID
}

// Identity returns the path's capture identity.
func (i info) Identity() ident.ID { return i.id }

// identify gives host metadata of slash path p its path's identity.
func (b *bottom) identify(p string, fi os.FileInfo) os.FileInfo {
	b.mu.Lock()
	defer b.mu.Unlock()
	id, ok := b.ids[p]
	if !ok {
		id, _ = ident.New(ident.Capture, uint64(len(b.ids))+1)
		b.ids[p], b.paths[id] = id, p
	}
	return info{FileInfo: fi, id: id}
}

// Lstat returns metadata without following the final symlink.
func (b *bottom) Lstat(p string) (os.FileInfo, error) {
	fi, err := os.Lstat(filepath.Join(b.dir, filepath.FromSlash(p)))
	if err != nil {
		return nil, err
	}
	return b.identify(p, fi), nil
}

// ReadDir returns children in name order, retaining symlink metadata.
func (b *bottom) ReadDir(p string) ([]os.FileInfo, error) {
	fi, err := b.Lstat(p)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, &fs.PathError{Op: "readdir", Path: p, Err: syscall.ENOTDIR}
	}
	entries, err := os.ReadDir(filepath.Join(b.dir, filepath.FromSlash(p)))
	if err != nil {
		return nil, err
	}
	infos := make([]os.FileInfo, 0, len(entries))
	for _, entry := range entries {
		fi, err := entry.Info()
		if err != nil {
			return nil, err
		}
		infos = append(infos, b.identify(path.Join(p, entry.Name()), fi))
	}
	return infos, nil
}

// Locate returns the path the bottom numbered id for, when it still exists.
func (b *bottom) Locate(id ident.ID) (string, bool) {
	b.mu.Lock()
	p, ok := b.paths[id]
	b.mu.Unlock()
	if !ok {
		return "", false
	}
	_, err := os.Lstat(filepath.Join(b.dir, filepath.FromSlash(p)))
	return p, err == nil
}

// Open opens a regular file read-only; symlink resolution belongs to the overlay.
func (b *bottom) Open(p string) (overlay.Content, error) {
	name := filepath.Join(b.dir, filepath.FromSlash(p))
	fi, err := os.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, &os.PathError{Op: "open", Path: name, Err: os.ErrInvalid}
	}
	f, err := hostfile.Open(name)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// Readlink returns the stored target without resolving it against the host
// directory, slash-separated as Windows hosts do not store it.
func (b *bottom) Readlink(p string) (string, error) {
	target, err := os.Readlink(filepath.Join(b.dir, filepath.FromSlash(p)))
	return filepath.ToSlash(target), err
}

// CopyTo materializes an entry at a new host path with [clone.Tree].
func (b *bottom) CopyTo(p, dst string) error {
	return clone.Tree(filepath.Join(b.dir, filepath.FromSlash(p)), dst)
}
