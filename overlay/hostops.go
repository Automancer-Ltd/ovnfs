package overlay

import (
	"errors"
	"os"
	"path"
	"slices"
	"strings"

	"github.com/go-git/go-billy/v5"
	"github.com/stencil-hq/ovnfs/internal/hostfile"
)

// Host operations outside the lock. With the memory tier, a few host changes
// of upper names run without f.mu, so the checkout's other calls go on
// during their syscalls: spill installs ([FS.writeBack]), and opening,
// removing and renaming upper regular files that need no deletion marker
// ([FS.openOutside], [FS.removeOutside], [FS.renameOutside]), and linking
// them ([FS.linkOutside]); their callers still wait for them. While one runs, the
// index still shows its names as before; it publishes its result under f.mu
// when the host change is done. Every call that changes the upper, its index
// entries or its host entries first waits until no such operation touches
// the names it addresses, their ancestors or their descendants
// ([FS.lockPaths]); calls that only read the index go on.

// hostOp is one host change of upper names in flight outside f.mu: run by
// its caller, which waits for it, or queued for the overlay's applier (see
// queue.go), whose caller does not.
type hostOp struct {
	// paths are the names the operation reads or changes on the host, which
	// orders queued operations; changes, when not nil, the ones it changes,
	// which calls needing their host state wait for (a link reads its
	// source and changes only its new name).
	paths   []string
	changes []string
	done    chan struct{}
	// queued marks an operation the applier runs: apply makes the host
	// change outside f.mu, then applied, if not nil, finishes it under f.mu.
	queued  bool
	apply   func() error
	applied func()
	making  bool // a worker is making the queued change now
	ended   bool // the operation is done or was dropped
	// For a queued change, before are the older queued changes on related
	// names, deps how many of them are not made yet, and after the newer
	// ones waiting for this one (see [FS.next]).
	before []*hostOp
	deps   int
	after  []*hostOp
}

// related reports whether overlay paths a and b are equal or one lies below the other.
func related(a, b string) bool {
	return a == b || a == "" || b == "" || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

// busy returns a host operation in flight on p, an ancestor or a descendant
// of p; nil when none is. The caller holds f.mu in either mode.
func (f *FS) busy(p string) *hostOp {
	for _, op := range f.ops {
		for _, q := range op.paths {
			if related(p, q) {
				return op
			}
		}
	}
	return nil
}

// busyAny is [FS.busy] for any of paths.
func (f *FS) busyAny(paths []string) *hostOp {
	return f.find(paths, func(*hostOp) bool { return true })
}

// find returns an operation in flight on any of paths, their ancestors or
// descendants that match accepts; nil when none is. The caller holds f.mu
// in either mode.
func (f *FS) find(paths []string, match func(*hostOp) bool) *hostOp {
	return f.byPath.first(paths, match)
}

// changing returns an operation in flight or queued that changes the host
// entry of p, an ancestor or a descendant of p; nil when none does. A call
// that only reads p's host entry (opens it, stats it) waits for this one;
// one that changes it waits for every operation reading it too
// ([FS.busy]).
func (f *FS) changing(p string) *hostOp {
	return f.byChange.first([]string{p}, func(*hostOp) bool { return true })
}

// barrier returns the names whose host state op changes.
func (op *hostOp) barrier() []string {
	if op.changes != nil {
		return op.changes
	}
	return op.paths
}

// queuedOn returns a queued operation on any of paths, their ancestors or
// descendants that the applier has not made yet; nil when none is. A call
// that changes the host itself waits for it, the host trailing the index.
func (f *FS) queuedOn(paths ...string) *hostOp {
	return f.find(paths, func(op *hostOp) bool { return op.queued })
}

// lockSync takes f.mu exclusively once no operation its caller runs is in
// flight on the clean overlay paths, their ancestors or descendants;
// queued ones may be. Calls that may change the index only and queue their
// host change take it this way, and before any host change of their own
// wait for the queue ([FS.queuedOn]).
func (f *FS) lockSync(paths ...string) {
	f.mu.Lock()
	for {
		op := f.find(paths, func(op *hostOp) bool { return !op.queued })
		if op == nil {
			return
		}
		f.waitOp(op) // an operation in flight never parks
	}
}

// begin records a host operation on paths (never "", which would hold up
// every call), which the caller found idle. The
// caller holds f.mu exclusively and releases it for the host change.
func (f *FS) begin(paths ...string) *hostOp {
	op := &hostOp{paths: paths, done: make(chan struct{})}
	f.trackOp(op)
	return op
}

// trackOp records op as in flight or queued. The caller holds f.mu exclusively.
func (f *FS) trackOp(op *hostOp) {
	f.ops = append(f.ops, op)
	f.byPath.add(op, op.paths)
	f.byChange.add(op, op.barrier())
}

// end retires op, waking the calls that wait for it. The caller holds f.mu exclusively.
func (f *FS) end(op *hostOp) {
	f.ops = slices.DeleteFunc(f.ops, func(o *hostOp) bool { return o == op })
	f.byPath.remove(op, op.paths)
	f.byChange.remove(op, op.barrier())
	op.ended = true
	close(op.done)
	for _, a := range op.after {
		if a.deps--; a.deps == 0 && !a.ended {
			f.ready = append(f.ready, a)
		}
	}
}

// waitOp releases f.mu, held exclusively, until op ends, then takes it
// again. A queued op is made next, before unrelated older ones
// ([FS.next]), so a call waits for the changes on its names, not for the
// whole queue. While op cannot end because the queue is parked on it or on
// a change it follows, waitOp returns the failure instead of waiting for
// the host to accept the change ([FS.stuck]); f.mu is held either way.
func (f *FS) waitOp(op *hostOp) error {
	if op.queued {
		f.urgent[op]++
		defer func() {
			if f.urgent[op]--; f.urgent[op] <= 0 {
				delete(f.urgent, op)
			}
		}()
	}
	for {
		if err := f.stuck(op); err != nil {
			return err
		}
		signal := f.parkSignal
		f.mu.Unlock()
		select {
		case <-op.done:
			f.mu.Lock()
			return nil
		case <-signal:
			f.mu.Lock()
		}
	}
}

// stuck returns the parked queue's failure when op cannot end before the
// parked change succeeds: op is that change or follows it on some name.
// The caller holds f.mu.
func (f *FS) stuck(op *hostOp) error {
	if f.parkedErr == nil || op.ended || !op.queued {
		return nil
	}
	seen := make(map[*hostOp]bool)
	var follows func(o *hostOp) bool
	follows = func(o *hostOp) bool {
		if o == f.parkedOp {
			return true
		}
		seen[o] = true
		for _, b := range o.before {
			if !b.ended && !seen[b] && follows(b) {
				return true
			}
		}
		return false
	}
	if follows(op) {
		return f.parkedErr
	}
	return nil
}

// lockPaths takes f.mu exclusively once no host operation is in flight on
// the clean overlay paths, their ancestors or descendants ("" waits for
// every operation). Calls that change the upper take it this way before
// they read anything their change depends on. It returns, with f.mu held,
// the parked queue's failure that holds up a change on paths ([FS.waitOp]).
func (f *FS) lockPaths(paths ...string) error {
	f.mu.Lock()
	for {
		op := f.busyAny(paths)
		if op == nil {
			return nil
		}
		if err := f.waitOp(op); err != nil {
			return err
		}
	}
}

// rlockPaths is [FS.lockPaths] taking f.mu shared.
func (f *FS) rlockPaths(paths ...string) error {
	f.mu.RLock()
	for {
		op := f.busyAny(paths)
		if op == nil {
			return nil
		}
		f.mu.RUnlock()
		f.mu.Lock()
		err := f.waitOp(op)
		f.mu.Unlock()
		f.mu.RLock()
		if err != nil {
			return err
		}
	}
}

// lockNames takes f.mu exclusively for a call on open descriptor h, once no
// host operation is in flight on the upper name the call reads h's inode
// metadata through. It returns, with f.mu held, the parked queue's failure
// that holds up that name ([FS.waitOp]).
func (f *FS) lockNames(h *file) error {
	f.mu.Lock()
	for {
		name := ""
		if h.tracked {
			name = f.aliases[h.inode].first
		}
		op := (*hostOp)(nil)
		if name != "" {
			op = f.busy(name)
		}
		if op == nil {
			return nil
		}
		if err := f.waitOp(op); err != nil {
			return err
		}
	}
}

// hostFile reports whether the index holds p as an upper regular file on
// disk, the kind of entry the host operations below change outside f.mu.
func (f *FS) hostFile(p string) bool {
	e, ok := f.index.entries[p]
	return ok && e.Kind == KindFile && f.files[p] == nil && e.Info != nil && e.Info.Mode().IsRegular()
}

// openOutside opens upper regular file p, for name and with flag (which
// neither creates nor truncates it), without holding f.mu across the open.
// The caller holds f.mu exclusively with no host operation on p in flight.
func (f *FS) openOutside(p, name string, flag int) (billy.File, error) {
	op := f.begin(p)
	real := f.up(p)
	f.mu.Unlock()
	fh, err := hostfile.OpenFile(real, flag&^(os.O_CREATE|os.O_EXCL), 0)
	f.mu.Lock()
	f.end(op)
	if err != nil {
		return nil, err
	}
	h := f.adopt(&file{File: fh, name: name, writable: writable(flag)})
	f.bind(h, p)
	return h, nil
}

// removeOutside removes upper regular file p, described by info, which has
// no lower entry to mask, without holding f.mu across the unlink. Its
// identity is released first, as the inode may be reused once the last name
// is gone; alias, another name of the file if not "", is stated afterwards
// for its link count. The caller holds f.mu exclusively with no host
// operation on p or alias in flight.
func (f *FS) removeOutside(p string, info os.FileInfo, alias string) error {
	if err := f.ids.release(info); err != nil {
		return err
	}
	paths := []string{p}
	if alias != "" {
		paths = append(paths, alias)
	}
	op := f.begin(paths...)
	real, other, ids := f.up(p), f.up(alias), f.ids
	f.mu.Unlock()
	// Unlike os.Remove on Windows, keeps a read-only hard link's other names read-only.
	err := hostfile.Remove(real)
	var aliasInfo os.FileInfo
	var aliasErr error
	if err == nil && alias != "" {
		aliasInfo, aliasErr = statHost(other, ids)
	}
	f.mu.Lock()
	f.end(op)
	if err != nil {
		return err
	}
	f.dropIndex(p)
	if aliasErr != nil {
		return aliasErr
	}
	if aliasInfo != nil {
		f.cacheInfo(alias, aliasInfo)
	}
	return f.touchDir(parent(p), true)
}

// renameOutside moves upper regular file src, which has no lower entry to
// mask, to dst, absent or an upper regular file with no other name that it
// replaces (dinfo describes it then), without holding f.mu across the
// rename. The file keeps its metadata: the index moves it without a stat.
// The caller holds f.mu exclusively with no host operation on either name
// in flight, and dst's parent is an upper directory.
func (f *FS) renameOutside(src, dst string, dinfo os.FileInfo) error {
	if f.hostFile(dst) {
		// The rename removes the name of the file it replaces.
		if err := f.ids.release(dinfo); err != nil {
			return err
		}
	}
	marked := f.mayHoldMarker(dst)
	op := f.begin(src, dst)
	from, to, marker := f.up(src), f.up(dst), f.whiteoutPath(dst)
	f.mu.Unlock()
	err := renameFile(from, to)
	var markerErr error
	if err == nil && marked {
		markerErr = removeMarker(marker)
	}
	f.mu.Lock()
	f.end(op)
	if err != nil {
		return err
	}
	f.dropIndex(dst)
	f.moveEntry(src, dst)
	f.dropIndex(src)
	e := f.index.entries[dst]
	e.Info = aliasInfo{FileInfo: unalias(e.Info), name: path.Base(dst)}
	f.index.set(dst, e)
	if err := f.touchDir(parent(src), true); err != nil {
		return err
	}
	if parent(dst) != parent(src) {
		if err := f.touchDir(parent(dst), true); err != nil {
			return err
		}
	}
	return markerErr
}

// moveEntry moves the index entry at a, with everything indexed below it,
// to b, where a rename put it on disk (or, for a memory file, nowhere),
// without traversing a renamed directory on disk.
func (f *FS) moveEntry(a, b string) {
	e := f.index.entries[a]
	if (e.Kind == KindFile || e.Kind == KindSymlink) && e.Origin == "" {
		e.Origin = a
	}
	f.index.set(b, e)
	f.moveIdentity(a, b)
	f.moveUnsettled(a, b)
	if m := f.files[a]; m != nil {
		delete(f.files, a)
		f.files[b] = m
		m.rename(a, b)
	}
	for n := range f.index.children[a] {
		f.moveEntry(path.Join(a, n), path.Join(b, n))
	}
}

// linkOutside gives src, an upper regular file on disk or in memory without
// open handles, the new name dst in an upper directory without holding f.mu
// across host I/O. A memory file moves to disk first, as a link needs its
// inode: staged from a copy and renamed into place like [FS.writeBack]
// does, while calls on its name wait. The caller holds f.mu exclusively with
// no host operation on either name in flight.
func (f *FS) linkOutside(src, dst string) error {
	m := f.files[src]
	var copied *memFile
	var tmp string
	if m != nil {
		copied = &memFile{data: slices.Clone(m.data), mode: m.mode, atime: m.atime, mtime: m.mtime, created: m.created, id: m.id, uid: m.uid, gid: m.gid}
		tmp = f.stagingName()
		m.installing = true
	}
	srcMarked, dstMarked := m != nil && m.marked, f.mayHoldMarker(dst)
	op := f.begin(src, dst)
	from, to, ids := f.up(src), f.up(dst), f.ids
	srcMarker, dstMarker := f.whiteoutPath(src), f.whiteoutPath(dst)
	f.mu.Unlock()
	var err error
	installed := false
	if m != nil {
		if _, err = stage(tmp, copied, ids); err == nil {
			err = renameFile(tmp, from)
		}
		if err != nil {
			ids.discard(tmp)
		} else if installed = true; srcMarked && removeMarker(srcMarker) == nil {
			srcMarked = false
		}
	}
	if err == nil {
		err = os.Link(from, to)
	}
	linked := err == nil
	if linked && dstMarked {
		err = removeMarker(dstMarker)
	}
	// After a link both names show the inode's new link count.
	var info os.FileInfo
	var statErr error
	if linked {
		info, statErr = statHost(to, ids)
	} else if installed {
		info, statErr = statHost(from, ids)
	}
	f.mu.Lock()
	f.end(op)
	if m != nil {
		m.installing = false
		m.marked = srcMarked
	}
	if statErr != nil {
		// Index what the host holds now the slow way, under f.mu.
		if installed {
			statErr = f.installed(m, nil)
		}
		if linked {
			statErr = errors.Join(statErr, f.refresh(dst), f.touchDir(parent(dst), true))
		}
		return errors.Join(err, statErr)
	}
	if installed {
		// src was busy, so m did not change since it was copied.
		f.installedAs(m, aliasInfo{FileInfo: unalias(info), name: path.Base(src)})
	}
	if !linked {
		return err
	}
	f.cacheInfo(dst, info)
	if terr := f.touchDir(parent(dst), true); err == nil {
		err = terr
	}
	return err
}

// busySync returns an operation its caller runs in flight on p, an
// ancestor or a descendant of p, leaving queued ones out; nil when none is.
func (f *FS) busySync(p string) *hostOp {
	return f.find([]string{p}, func(op *hostOp) bool { return !op.queued })
}

// nonEmpty returns the paths that are not "", which [FS.busy] would read as
// the whole tree.
func nonEmpty(paths ...string) []string {
	return slices.DeleteFunc(paths, func(p string) bool { return p == "" })
}
