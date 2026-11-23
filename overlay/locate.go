package overlay

import (
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
	bottom := f.bottom
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
	if !cold {
		return "", false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.loadIndex() != nil {
		return "", false
	}
	// The upper's redirects, known now, may show a lower entry elsewhere.
	if p, ok := f.locate(id); ok {
		return p, true
	}
	if inBottom && f.bottom == bottom {
		return f.locateBottom(below, id)
	}
	return "", false
}

// locateBottom finds where the overlay shows id, which the bottom holds at
// q. The caller holds f.mu.
func (f *FS) locateBottom(q string, id ident.ID) (string, bool) {
	p := f.above(len(f.layers), q)
	return p, f.shows(p, id)
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
