package overlay

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"syscall"
	"time"

	"github.com/stencil-hq/ovnfs/internal/ident"
)

// Locate returns the current path of the visible entry whose identity is id,
// or false when the overlay shows none: the export resolves the file ids it
// hands out through it. Every answer is verified with a lookup of the path.
// It finds entries by identity, never by walking a tree: the upper's index
// maps the identity of each of its entries (memory files, copies that took
// over a lower entry's identity and moved since, renamed directories'
// contents), each frozen layer maps its own, and the bottom resolves its own.
// A cold upper index loads only when nothing else shows id.
func (f *FS) Locate(id ident.ID) (string, bool) {
	if id == 0 {
		return "", false
	}
	f.mu.RLock()
	p, ok := f.locate(id)
	cold := f.index == nil && f.upper != ""
	bottom, epoch := f.bottom, f.bottomEpoch
	f.mu.RUnlock()
	if ok {
		return p, true
	}
	// Outside the lock: the bottom may wait for background work.
	below, inBottom := "", false
	if bottom != nil {
		below, inBottom = bottom.Locate(id)
	}
	if inBottom {
		f.mu.RLock()
		p, ok = f.locateBottom(below, id)
		f.mu.RUnlock()
		if ok {
			return p, true
		}
	}
	if cold {
		f.mu.Lock()
		if f.loadIndex() != nil {
			f.mu.Unlock()
			return "", false
		}
		// The upper's redirects, known now, may show a lower entry elsewhere.
		p, ok = f.locate(id)
		if !ok && inBottom && f.bottomEpoch == epoch {
			p, ok = f.locateBottom(below, id)
		}
		f.mu.Unlock()
		if ok {
			return p, true
		}
	}
	if aliases, has := bottom.(AliasLocator); inBottom && has {
		ok = aliases.LocateAlias(id, func(q string) bool {
			if q == below {
				return false // the lookup above already rejected it
			}
			f.mu.RLock()
			defer f.mu.RUnlock()
			if f.bottomEpoch != epoch {
				return false
			}
			var visible bool
			p, visible = f.locateBottom(q, id)
			return visible
		})
		if ok {
			return p, true
		}
	}
	return "", false
}

// locateBottom finds where the overlay shows id, which the bottom holds at
// q. The caller holds f.mu.
func (f *FS) locateBottom(q string, id ident.ID) (string, bool) {
	p := f.above(len(f.layers), q)
	_, info, err := f.lookupRead(p)
	if err != nil {
		return "", false
	}
	got, ok := ident.Of(info)
	if !ok || got != id {
		return "", false
	}
	// Another name of this bottom inode may have taken the ID into the
	// upper. Load its index before accepting an unchanged hard link.
	if f.index == nil && f.upper != "" && !info.IsDir() && hardLinks(info) > 1 {
		return "", false
	}
	return p, true
}

// above translates path q of the i-th frozen layer (len(f.layers): the
// bottom) into the overlay's, through the redirects of every level above it,
// nearest first (see [Entry.Redirect]). A cold upper's redirects are unknown
// here; callers load the index before giving up. The caller holds f.mu.
func (f *FS) above(i int, q string) string {
	for j := i - 1; j >= 0; j-- {
		q = f.layers[j].Unredirected(q)
	}
	if f.index != nil {
		q = f.index.Unredirected(q)
	}
	return q
}

// locate finds id at the root, in the upper's loaded index or in a frozen
// layer. The caller holds f.mu.
func (f *FS) locate(id ident.ID) (string, bool) {
	if f.shows("", id) {
		return "", true
	}
	if p, ok := f.locateUpper(id); ok {
		return p, true
	}
	return f.locateLayers(id)
}

// locateLayers finds a visible holder of id in the frozen layers.
func (f *FS) locateLayers(id ident.ID) (string, bool) {
	for i, l := range f.layers {
		if q, ok := l.locate(id, func(q string) bool { return f.shows(f.above(i, q), id) }); ok {
			return f.above(i, q), true
		}
	}
	return "", false
}

// locateUpper finds id in the upper's index, if loaded. The caller holds f.mu.
func (f *FS) locateUpper(id ident.ID) (string, bool) {
	p, ok := f.paths[id]
	return p, ok && f.shows(p, id)
}

// shows reports whether the overlay shows an entry with identity id at p.
// The caller holds f.mu.
func (f *FS) shows(p string, id ident.ID) bool {
	_, info, err := f.lookupRead(p)
	if err != nil {
		return false
	}
	got, ok := ident.Of(info)
	return ok && got == id
}

// Vanished returns the overlay path and pre-fence metadata of an entry with
// identity id that left a fenced bottom cache root after the host changed or
// removed it, when the overlay itself has not replaced or hidden that path.
// It returns false when id is still live, was removed/shadowed in this
// overlay, or was never remembered.
func (f *FS) Vanished(id ident.ID) (string, os.FileInfo, bool) {
	if id == 0 {
		return "", nil, false
	}
	f.mu.RLock()
	bottom, epoch := f.bottom, f.bottomEpoch
	if f.upper != "" && f.index == nil {
		f.mu.RUnlock()
		f.mu.Lock()
		if f.loadIndex() != nil {
			f.mu.Unlock()
			return "", nil, false
		}
		bottom, epoch = f.bottom, f.bottomEpoch
		f.mu.Unlock()
		f.mu.RLock()
	}
	vl, ok := bottom.(VanishedLocator)
	if !ok {
		f.mu.RUnlock()
		return "", nil, false
	}
	if _, gone := f.removed[id]; gone {
		f.mu.RUnlock()
		return "", nil, false
	}
	// If id is held in the upper or a frozen layer, it has not vanished.
	if _, live := f.locate(id); live {
		f.mu.RUnlock()
		return "", nil, false
	}
	f.mu.RUnlock()

	var overlayPath string
	_, info, found := vl.Vanished(id, func(q string) bool {
		f.mu.RLock()
		defer f.mu.RUnlock()
		if f.bottomEpoch != epoch {
			return false
		}
		p := f.above(len(f.layers), q)
		if !f.reachesBottom(p, q) {
			return false
		}
		overlayPath = p
		return true
	})
	if !found {
		return "", nil, false
	}
	return overlayPath, info, true
}

// reachesBottom reports whether overlay path p falls through the upper and
// every frozen layer unobstructed to bottom path q (no entry, whiteout, or
// opaque/non-dir barrier above). The caller holds f.mu with the index loaded
// when upper != "".
func (f *FS) reachesBottom(p, q string) bool {
	if p == "" {
		return false
	}
	cur := p
	if f.index != nil {
		if _, ok := f.index.entries[cur]; ok {
			return false
		}
		if f.index.barrier(cur) {
			return false
		}
		cur = f.index.redirected(cur)
	}
	for _, l := range f.layers {
		v := l.view()
		if _, ok := v.entries[cur]; ok {
			return false
		}
		if v.barrier(cur) {
			return false
		}
		cur = v.redirected(cur)
	}
	return cur == q
}

// TruncateVanished materializes a vanished fenced regular file id as an empty
// file in the writable upper at its remembered path, keeping id as its
// identity so a client's truncating write (SETATTR size=0) succeeds without
// exposing post-fence host bytes. Returns ESTALE when id is no longer
// vanished, is not a regular file, or its parent directory is gone.
func (f *FS) TruncateVanished(id ident.ID) (string, os.FileInfo, error) {
	if id == 0 {
		return "", nil, syscall.ESTALE
	}
	p, vinfo, ok := f.Vanished(id)
	if !ok || !vinfo.Mode().IsRegular() {
		return "", nil, syscall.ESTALE
	}
	f.await(p)
	f.staging.RLock()
	defer f.staging.RUnlock()
	f.lockSync(p)
	defer f.mu.Unlock()
retry:
	f.syncOps(p)
	if err := f.loadIndex(); err != nil {
		return "", nil, err
	}
	if err := f.mutable("open", p); err != nil {
		return "", nil, err
	}
	if p == "" || reserved(p) {
		return "", nil, syscall.ESTALE
	}
	if op := f.queuedOn(p); op != nil {
		if err := f.waitOp(op); err != nil {
			return "", nil, err
		}
		goto retry
	}
	// Another caller may have materialized or created p meanwhile.
	if _, info, err := f.lookup(p); err == nil {
		if got, known := ident.Of(info); known && got == id && info.Mode().IsRegular() {
			return p, info, nil
		}
		return "", nil, syscall.ESTALE
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", nil, err
	}
	// Reject if id was removed in this checkout, is held elsewhere, or p is hidden.
	if _, gone := f.removed[id]; gone {
		return "", nil, syscall.ESTALE
	}
	if _, live := f.locate(id); live {
		return "", nil, syscall.ESTALE
	}
	if _, hasUpper := f.index.entries[p]; hasUpper || f.index.barrier(p) {
		return "", nil, syscall.ESTALE
	}
	if err := ownerAccess(vinfo.Mode(), p, os.O_WRONLY|os.O_TRUNC); err != nil {
		return "", nil, err
	}
	if err := f.claimName("open", p, ""); err != nil {
		return "", nil, err
	}
	if op := f.queuedOn(parent(p)); op != nil && f.index.entries[parent(p)].Kind != KindDir {
		if err := f.waitOp(op); err != nil {
			return "", nil, err
		}
		goto retry
	}
	if err := f.ensureDir(parent(p)); err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			return "", nil, syscall.ESTALE
		}
		return "", nil, err
	}
	if f.mem != nil {
		if m := f.addMem(p, vinfo.Mode().Perm(), time.Now(), nil, id); m != nil {
			return p, m.info(path.Base(p)), nil
		}
	}
	err := f.preserveDirTime(parent(p), func() error {
		fh, err := os.OpenFile(f.up(p), os.O_CREATE|os.O_EXCL|os.O_WRONLY, vinfo.Mode().Perm())
		if err != nil {
			return err
		}
		if err := fh.Chmod(vinfo.Mode().Perm()); err != nil {
			fh.Close()
			return err
		}
		if err := fh.Close(); err != nil {
			return err
		}
		if err := f.ids.persist(f.up(p), id); err != nil {
			return err
		}
		return keepMode(f.up(p), vinfo.Mode())
	})
	if err != nil {
		return "", nil, err
	}
	if err := f.refresh(p); err != nil {
		return "", nil, err
	}
	_, info, err := f.lookup(p)
	return p, info, err
}

// markRemoved remembers that State identity id was removed or replaced in
// this overlay so a vanished fenced entry never revives it. The caller holds f.mu.
func (f *FS) markRemoved(id ident.ID) {
	if f.removed == nil {
		f.removed = make(map[ident.ID]struct{})
	}
	f.removed[id] = struct{}{}
}

// await lets an [Awaiter] bottom finish background work for path name before
// the caller takes f.mu.
func (f *FS) await(name string) {
	f.mu.RLock()
	bottom := f.bottom
	f.mu.RUnlock()
	if w, ok := bottom.(Awaiter); ok {
		w.Await(Clean(name))
	}
}
