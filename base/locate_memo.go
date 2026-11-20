//go:build darwin || linux || windows

package base

import "sync/atomic"

// memoBits sets how many identities a memo remembers, 1<<memoBits: a quarter
// of a megabyte of slots once used, and the paths behind them.
const memoBits = 15

// memo remembers the view paths of State identities a snapshot handed out,
// by inode. It is direct-mapped: each inode has one slot, a newer one there
// replaces an older one, and readers take no lock.
type memo struct {
	slots atomic.Pointer[[1 << memoBits]atomic.Pointer[located]]
}

// located is one remembered identity: the entry with inode ino was at path.
type located struct {
	ino  uint64
	path string
}

// slot returns where ino's entry goes; nil while nothing was ever put,
// unless make is set. Inodes of one directory are often consecutive, so a
// multiplicative hash spreads them.
func (m *memo) slot(ino uint64, make bool) *atomic.Pointer[located] {
	slots := m.slots.Load()
	if slots == nil {
		if !make {
			return nil
		}
		m.slots.CompareAndSwap(nil, new([1 << memoBits]atomic.Pointer[located]))
		slots = m.slots.Load()
	}
	return &slots[(ino*0x9e3779b97f4a7c15)>>(64-memoBits)]
}

// put remembers that the entry with inode ino is at view path p.
func (m *memo) put(ino uint64, p string) { m.slot(ino, true).Store(&located{ino: ino, path: p}) }

// get returns the view path the entry with inode ino was last put at.
func (m *memo) get(ino uint64) (string, bool) {
	if slot := m.slot(ino, false); slot != nil {
		if e := slot.Load(); e != nil && e.ino == ino {
			return e.path, true
		}
	}
	return "", false
}
