package base

import (
	"errors"
	"io"
	"io/fs"
	"math"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/stencil-hq/ovnfs/internal/clone"
	"github.com/stencil-hq/ovnfs/internal/hostfile"
	"github.com/stencil-hq/ovnfs/internal/ident"
	"github.com/stencil-hq/ovnfs/internal/search/fsys"
	"github.com/stencil-hq/ovnfs/overlay"
	"github.com/stencil-hq/ovnfs/rules"
	"github.com/stencil-hq/ovnfs/source"
	"github.com/stencil-hq/ovnfs/store"
)

// bottom serves a local base to the overlay without copying anything. A source
// file is the folder's file while its size, mtime and inode still match the
// capture, and the captured blob once they do not; a source directory lists
// the captured names, plus the cache and local roots c holds. Everything at or
// below those roots comes from c, the checkout's cache snapshot (see
// [Caches]); without one (hashing views) the roots do not exist. A checkout's
// writes copy up into its own upper.
type bottom struct {
	b *Base
	c *Caches
}

// entry is what serves a path of the view.
type entry struct {
	host     string // the folder's file serving a source path; "" for captured content and snapshot entries
	info     os.FileInfo
	n        uint32 // its index node, for a source path
	snapshot bool   // host is in the cache snapshot
}

// Lstat returns current metadata for unchanged source and snapshot paths,
// captured metadata otherwise.
func (v bottom) Lstat(p string) (os.FileInfo, error) {
	e, err := v.resolve(overlay.Clean(p))
	return e.info, err
}

// Await blocks until p can be served without waiting for the cache snapshot,
// so the overlay never waits for it while holding its lock (overlay.Awaiter).
func (v bottom) Await(p string) {
	if v.c != nil && !v.c.ready.Load() {
		v.c.await(v.b, overlay.Clean(p))
	}
}

// Locate returns the path of the entry whose identity is id: a captured entry
// by its index node, a snapshot file by its inode; false when the view does
// not show it.
func (v bottom) Locate(id ident.ID) (string, bool) {
	var p string
	var ok bool
	switch id.Space() {
	case ident.Capture:
		p, ok = v.b.nodePath(id.Value())
	case ident.State:
		if v.c != nil {
			p, ok = v.c.locate(id.Value())
			p = filepath.ToSlash(p)
		}
	}
	if !ok {
		return "", false
	}
	info, err := v.Lstat(p)
	if err != nil {
		return "", false
	}
	if got, known := ident.Of(info); !known || got != id {
		return "", false
	}
	return p, true
}

// ReadDir lists a directory: captured source names with the roots the cache
// snapshot holds, or a snapshot directory.
func (v bottom) ReadDir(p string) ([]os.FileInfo, error) {
	b := v.b
	p = overlay.Clean(p)
	e, err := v.resolve(p)
	if err != nil {
		return nil, err
	}
	if !e.info.IsDir() {
		return nil, &fs.PathError{Op: "readdir", Path: p, Err: syscall.ENOTDIR}
	}
	if e.snapshot {
		return v.c.list(p)
	}
	// Both lists are sorted. Native metadata serves every present child;
	// captured names missing from the host still retain their fork-time info.
	// Names the capture never saw are not in the view.
	x := b.index
	i, end := x.children(e.n)
	lower, listErr := fsys.ReadDir(b.lower(p))
	out := make([]os.FileInfo, 0, end-i)
	for j := 0; i < end; i++ {
		n := i
		child := x.nameOf(n)
		if p != "" {
			child = p + "/" + child
		}
		if x.kind(n) == kindNone {
			if class := x.class(n); snapshotted(class) && v.c != nil {
				if info, err := v.c.lstat(child, child, n); err == nil {
					out = append(out, info)
				}
			}
			continue
		}
		if child == ".git" || source.IsJJPath(child) {
			continue
		}
		for j < len(lower) && x.compareName(n, lower[j].Name()) > 0 {
			j++
		}
		var current os.FileInfo
		var err error
		if listErr == nil && j < len(lower) && x.compareName(n, lower[j].Name()) == 0 {
			if current, err = lower[j].Info(); err == nil {
				current = hostfile.Linked(current, b.meta.Dir, child)
			}
		}
		if listErr != nil || err != nil {
			var ce entry
			if ce, err = v.resolve(child); err != nil {
				if os.IsNotExist(err) || isNotDir(err) {
					continue
				}
				return nil, err
			}
			current = ce.info
		} else {
			current, _ = b.sourceInfo(child, n, current)
		}
		out = append(out, current)
	}
	return out, nil
}

// Open opens the folder's file while it still matches the capture, or the
// snapshot's file, else serves the captured blob (from memory when the store
// caches it).
func (v bottom) Open(p string) (overlay.Content, error) {
	b := v.b
	p = overlay.Clean(p)
	e, err := v.resolve(p)
	if err != nil {
		return nil, err
	}
	if !e.info.Mode().IsRegular() {
		return nil, &fs.PathError{Op: "open", Path: p, Err: syscall.EISDIR}
	}
	if e.snapshot {
		// The snapshot is the checkout's own and never changes.
		return v.c.files.openFile(p)
	}
	if e.host != "" {
		f, err := hostfile.Open(e.host)
		if err == nil {
			// Between lstat and open the folder may have changed; the open
			// file must be the inode that matched.
			if opened, statErr := f.Stat(); statErr == nil && os.SameFile(e.info.(hostInfo).FileInfo, opened) && opened.Size() == e.info.Size() && opened.ModTime().Equal(e.info.ModTime()) {
				return f, nil
			}
			f.Close()
		}
	}
	return b.blobFile(p, b.index.oid(e.n))
}

// Readlink returns a symlink's target from the folder, the snapshot or the
// captured blob. A snapshot link whose absolute target lies inside the folder
// becomes relative to the link, so in a checkout it names the checkout's copy
// instead of the folder: Bun on Windows, for one, links workspace packages by
// absolute path.
func (v bottom) Readlink(p string) (string, error) {
	b := v.b
	p = overlay.Clean(p)
	e, err := v.resolve(p)
	if err != nil {
		return "", err
	}
	if e.info.Mode()&os.ModeSymlink == 0 {
		return "", &fs.PathError{Op: "readlink", Path: p, Err: syscall.EINVAL}
	}
	if e.snapshot || e.host != "" {
		var target string
		if e.snapshot {
			target, err = v.c.files.readlink(p)
			target = b.relocate(b.lower(p), target)
		} else {
			target, err = os.Readlink(e.host)
		}
		if err != nil {
			return "", err
		}
		// Windows stores and reports targets with backslashes; views use slashes.
		return filepath.ToSlash(target), nil
	}
	data, err := b.Blob(p, b.index.oid(e.n))
	return string(data), err
}

// relocate returns target relative to the folder's link at host when it is
// an absolute path inside the folder, else unchanged.
func (b *Base) relocate(host, target string) string {
	if !filepath.IsAbs(target) {
		return target
	}
	if in, err := filepath.Rel(b.meta.Dir, target); err != nil || in == ".." || strings.HasPrefix(in, ".."+string(filepath.Separator)) {
		return target
	}
	if rel, err := filepath.Rel(filepath.Dir(host), target); err == nil {
		return rel
	}
	return target
}

// CopyTo materializes p (a whole directory tree, for renames) at the new path dst for copy-up.
func (v bottom) CopyTo(p, dst string) error {
	p = overlay.Clean(p)
	e, err := v.resolve(p)
	if err != nil {
		return err
	}
	info := e.info
	switch {
	case info.IsDir():
		if err := os.Mkdir(dst, info.Mode().Perm()|0o700); err != nil {
			return err
		}
		children, err := v.ReadDir(p)
		if err != nil {
			return err
		}
		for _, child := range children {
			if err := v.CopyTo(path.Join(p, child.Name()), filepath.Join(dst, child.Name())); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		if err := os.Chmod(dst, info.Mode().Perm()); err != nil {
			return err
		}
	case info.Mode()&os.ModeSymlink != 0:
		target, err := v.Readlink(p)
		if err != nil {
			return err
		}
		if err := os.Symlink(target, dst); err != nil {
			return err
		}
	case info.Mode().IsRegular():
		c, err := v.Open(p)
		if err != nil {
			return err
		}
		err = copyContent(c, dst, info.Mode().Perm())
		c.Close()
		if err != nil {
			return err
		}
	default:
		// Sockets, FIFOs and devices in snapshots have no content to copy.
		return nil
	}
	return setTimes(dst, info.ModTime(), info.Mode()&os.ModeSymlink != 0)
}

// resolve finds what serves p: the folder's entry of an unchanged source path
// with its current metadata, the captured blob with captured metadata, or the
// cache snapshot's entry below a cache or local root.
func (v bottom) resolve(p string) (entry, error) {
	b := v.b
	if p == ".git" || strings.HasPrefix(p, ".git/") || source.IsJJPath(p) {
		return entry{}, absent(p)
	}
	n, root, class := b.find(p)
	if root == "" {
		host := b.lower(p)
		current, _ := hostfile.Lstat(host)
		info, unchanged := b.sourceInfo(p, n, current)
		if !unchanged {
			host = ""
		}
		return entry{host: host, info: info, n: n}, nil
	}
	if snapshotted(class) && v.c != nil {
		info, err := v.c.lstat(p, root, n)
		return entry{info: info, snapshot: true}, err
	}
	return entry{}, absent(p)
}

// sourceInfo selects current or captured metadata of source path p, index
// node n, after a stat or bulk listing, and reports whether the folder's
// entry serves p. Either carries p's Capture identity.
func (b *Base) sourceInfo(p string, n uint32, current os.FileInfo) (os.FileInfo, bool) {
	mode := b.index.fileMode(n)
	if mode.IsDir() {
		if current != nil && current.IsDir() {
			return hostInfo{FileInfo: current, id: captureID(n)}, true
		}
		name := path.Base(p)
		if p == "" {
			name = "/"
		}
		return capturedInfo{name: name, mode: mode, mtime: b.meta.ForkTime, id: captureID(n)}, false
	}
	captured, _ := b.index.info(n)
	if current != nil && b.unchanged(p, current, mode, captured) {
		return hostInfo{FileInfo: current, id: captureID(n)}, true
	}
	return capturedInfo{name: path.Base(p), mode: mode, size: captured.Size, mtime: presentedMtime(captured, b.meta.ForkTime), id: captureID(n)}, false
}

// presentedMtime is the mtime of a source file served from its capture: the
// captured one, or the fork time for content the folder never held.
func presentedMtime(captured source.Info, fork time.Time) time.Time {
	if captured.Mtime.IsZero() {
		return fork
	}
	return captured.Mtime
}

// unchanged reports a folder entry that still is the captured file: same type,
// size, mtime and inode. The inode check keeps a swapped-in lookalike (an
// ancestor directory replaced by a copy) from being served as captured content.
func (b *Base) unchanged(p string, current os.FileInfo, mode os.FileMode, captured source.Info) bool {
	if current.Mode().Type() != mode.Type() || current.Size() != captured.Size || !current.ModTime().Equal(captured.Mtime) {
		return false
	}
	if captured.Ino == 0 {
		return false
	}
	_, ino, err := fileIdentity(current, b.meta.Dir, p)
	return err == nil && ino == captured.Ino
}

// blobFile serves a source file's captured content: cached bytes, the stored
// object, or a repository object written into the store first.
func (b *Base) blobFile(p string, id store.OID) (overlay.Content, error) {
	st := b.config.Store
	if data, ok := st.Cached(id); ok {
		return overlay.ByteContent(data), nil
	}
	if !st.Has(id) {
		data, err := b.Blob(p, id)
		if err != nil {
			return nil, err
		}
		if _, err := st.PutBytes(data); err != nil {
			return nil, err
		}
		// PutBytes made a small blob resident; serve it without reopening.
		if data, ok := st.Cached(id); ok {
			return overlay.ByteContent(data), nil
		}
	}
	return st.Open(id)
}

// copyContent materializes c at the new path dst with mode: host files are
// cloned, in-memory content is written out.
func copyContent(c overlay.Content, dst string, mode os.FileMode) error {
	if f, ok := c.(*os.File); ok {
		return clone.FD(f, dst, mode)
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, io.NewSectionReader(c, 0, math.MaxInt64))
	if err == nil {
		// The umask may have narrowed mode at creation.
		err = out.Chmod(mode)
	}
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(dst)
	}
	return err
}

// Affects reports whether an edit at folder path p can make the folder differ
// from the capture: false only below a cache, local or hidden root, which the
// capture never descended, and in the root's .jj, which captures leave out. A
// name the capture never saw counts, since it may be source. Remote bases have
// no folder to edit.
func (b *Base) Affects(p string) bool {
	p = overlay.Clean(p)
	if b.remote != nil || source.IsJJPath(p) {
		return false
	}
	_, root, class := b.find(p)
	return root == "" || class == ""
}

// nextPrefixEnd returns the end of the root-relative clean path p's component
// that starts after offset from (the next slash or len(p)), so p[:end] is the
// next prefix without building it.
func nextPrefixEnd(p string, from int) int {
	if from > 0 {
		from++ // skip the slash ending the previous prefix
	}
	if i := strings.IndexByte(p[from:], '/'); i >= 0 {
		return from + i
	}
	return len(p)
}

// find locates p in the captured tree. A captured source path yields its
// index node and root "". Otherwise root is the first prefix of p absent from
// the source paths, with the class the capture recorded for it and its index
// node (0 when the index lacks it): a cache or local root is served from the
// cache snapshot; any other absent prefix (a hidden root, or a name that
// appeared after the capture) does not exist in the view.
func (b *Base) find(p string) (n uint32, root string, class rules.Class) {
	x := b.index
	// Mostly p is captured, or its parent is: two exact lookups settle it.
	n, ok := x.lookup(p)
	if ok && x.kind(n) != kindNone {
		return n, "", ""
	}
	if ok && x.kind(x.parent(n)) != kindNone {
		return n, p, x.class(n)
	}
	if !ok {
		if dir, found := x.lookup(parentOf(p)); found && x.kind(dir) != kindNone {
			return 0, p, ""
		}
	}
	// Deeper below the first absent prefix: walk down to it.
	n = 0
	for end := 0; end < len(p); {
		start := end
		if start > 0 {
			start++
		}
		end = nextPrefixEnd(p, end)
		child, ok := x.child(n, p[start:end])
		if !ok {
			return 0, p[:end], ""
		}
		if x.kind(child) == kindNone {
			return child, p[:end], x.class(child)
		}
		n = child
	}
	return n, "", ""
}

// lower names an entry in the source folder.
func (b *Base) lower(p string) string { return filepath.Join(b.meta.Dir, filepath.FromSlash(p)) }

// isNotDir reports a lookup through a non-directory, which listings skip like a missing name.
func isNotDir(err error) bool {
	var pe *fs.PathError
	return errors.As(err, &pe) && pe.Err == syscall.ENOTDIR
}

// absent returns the filesystem-standard missing-entry error.
func absent(p string) error { return &fs.PathError{Op: "lstat", Path: p, Err: fs.ErrNotExist} }
