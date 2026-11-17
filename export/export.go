package export

import "sync"

// Export serves the checkouts of a Router to the file protocols, with file
// ids that follow each entry for its whole life (see [Nodes]).
type Export struct {
	fs      *nodeFS
	volume  string
	rt      *Router
	handles *handleGens
}

// New exports rt's checkouts; upperVolume is where overlay writes land,
// whose capacity Statfs reports.
func New(rt *Router, upperVolume string) *Export {
	return &Export{fs: &nodeFS{Filesystem: rt, Change: rt, rt: rt}, volume: upperVolume, rt: rt, handles: new(handleGens)}
}

// Retire recalls clients' cached copies of checkout name, which held export
// slot, and closes its retained files before the name can be reused. The
// registry calls it (checkout.Config.Retire) under its lock, so it must not
// call back into the registry.
func (e *Export) Retire(name string, slot uint16) {
	if fn := e.fs.onRecall.Load(); fn != nil {
		first, last := slotIDs(slot)
		(*fn)(Recall{First: first, Last: last, Path: name})
	}
	e.rt.retire(name)
}

// handleGens counts how often each file id's NFS handles were retired
// (nfs.Backend.RetireHandle). Retirement is rare, so the map stays small; it
// is not persisted, so a restart makes the handles of a retired id stale
// once more, which clients recover from by looking the name up again. Every
// handle the server encodes reads it, so reads share the lock.
type handleGens struct {
	mu   sync.RWMutex
	gens map[uint64]uint32
}

// get returns id's retirement count.
func (h *handleGens) get(id uint64) uint32 {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.gens[id]
}

// bump retires id's current handles.
func (h *handleGens) bump(id uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.gens == nil {
		h.gens = make(map[uint64]uint32)
	}
	h.gens[id]++
}
