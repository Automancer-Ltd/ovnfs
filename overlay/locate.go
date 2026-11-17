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
	if bottom != nil {
		// Outside the lock: the bottom may wait for background work.
		if p, ok := bottom.Locate(id); ok {
			f.mu.RLock()
			ok = f.shows(p, id)
			f.mu.RUnlock()
			if ok {
				return p, true
			}
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
	return f.locateUpper(id)
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
	for _, l := range f.layers {
		if p, ok := l.locate(id, func(p string) bool { return f.shows(p, id) }); ok {
			return p, true
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
