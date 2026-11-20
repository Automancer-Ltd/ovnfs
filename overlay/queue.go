package overlay

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"slices"
	"time"

	"github.com/stencil-hq/ovnfs/internal/hostfile"
	"github.com/stencil-hq/ovnfs/internal/ident"
)

// The queue. With the memory tier, while the upper's index is loaded, the
// index is the truth for names too, and some host changes trail it: unlink,
// rename and link of upper regular files, and mkdir and rmdir of
// directories only the upper has. Their calls change the index and queue
// the host change; up to appliersMax workers per overlay make the queued
// changes, each one host call, without f.mu, changes on related names in
// the order the index saw them ([FS.next]). A queued change keeps its names
// busy ([FS.queuedOn]) until made: a call that needs the host state of a
// name (an open of a file on disk, a create on disk, a spill install, a
// sync) first waits for the changes on it, which the workers then make
// first, while calls that change the index alone queue behind. Flush and
// Freeze wait for the whole queue, so the upper on disk is complete for
// anything that reads it.
//
// A change that fails parks the queue: the applier retries it every
// queueRetry, new changes go to the host themselves until it succeeds, and
// the memory budget reports it ([Memory.QueuesParked]). A crash loses the
// queued changes. Each one is a single host call, made after every older
// change on a related name, so the upper on disk always loads like any
// other upper and shows every name in a state it had: names removed since
// show again, renamed ones under their old names. Without the memory tier
// nothing is queued.

// queueMax bounds the changes queued per overlay: a call that queues one
// past it waits for the applier, so a crash loses at most this many.
const queueMax = 1024

// queueRetry is how often a parked queue retries its failed change.
const queueRetry = time.Second

// queueable reports whether a change may queue its host part: only with the
// memory tier, on hosts whose native metadata the index can adjust
// ([relinked], [newDirInfo]), and while the queue is not parked.
func (f *FS) queueable() bool { return queueHost && f.lazyDirs() && f.parkedErr == nil }

// enqueue queues host change apply on paths after the index already shows
// it, and applied, if not nil, to finish it under f.mu once made. The
// caller holds f.mu exclusively; past queueMax it waits for the applier
// with f.mu released, after its own change is complete in the index.
func (f *FS) enqueue(paths []string, apply func() error, applied func()) {
	f.enqueueChanging(paths, nil, apply, applied)
}

// enqueueChanging is [FS.enqueue] for a change that reads paths and changes
// only changes on the host.
func (f *FS) enqueueChanging(paths, changes []string, apply func() error, applied func()) {
	f.enqueueNow(paths, changes, apply, applied)
	for f.queued > queueMax && f.parkedErr == nil {
		f.waitOp(f.head())
	}
}

// enqueueNow is [FS.enqueueChanging] without waiting past queueMax, for a
// caller in the middle of a change that must keep f.mu: the next queued
// change waits for both.
func (f *FS) enqueueNow(paths, changes []string, apply func() error, applied func()) {
	op := &hostOp{paths: paths, changes: changes, done: make(chan struct{}), queued: true, apply: apply, applied: applied}
	seen := make(map[*hostOp]bool)
	for _, p := range paths {
		f.byPath.each(p, func(e *hostOp) bool {
			if e.queued && !seen[e] {
				seen[e] = true
				op.before = append(op.before, e)
				e.after = append(e.after, op)
			}
			return true
		})
	}
	op.deps = len(op.before)
	f.trackOp(op)
	if op.deps == 0 {
		f.ready = append(f.ready, op)
	}
	f.queued++
	f.startApplier()
}

// head returns the oldest queued change; nil when none is queued.
func (f *FS) head() *hostOp {
	for _, op := range f.ops {
		if op.queued {
			return op
		}
	}
	return nil
}

// next returns a queued change a worker may make now: one no worker is
// making, with every older queued change on a related name made, since
// changes on related names stay in the order the index saw them, so the
// host always shows, for every name, a state it had. The oldest such
// change, unless a call waits for one (urgent): then one of the changes it
// waits for, transitively, that may be made now. nil when none may.
func (f *FS) next() *hostOp {
	for u := range f.urgent {
		if op := readyBefore(u, make(map[*hostOp]bool)); op != nil {
			return op
		}
	}
	for len(f.ready) > 0 && (f.ready[0].ended || f.ready[0].making) {
		f.ready = f.ready[1:]
	}
	for _, op := range f.ready {
		if !op.ended && !op.making {
			return op
		}
	}
	return nil
}

// readyBefore returns a change op waits for, op itself included, that may
// be made now; nil when none may.
func readyBefore(op *hostOp, seen map[*hostOp]bool) *hostOp {
	if op.ended || seen[op] {
		return nil
	}
	seen[op] = true
	if op.deps == 0 {
		if op.making {
			return nil
		}
		return op
	}
	for _, b := range op.before {
		if r := readyBefore(b, seen); r != nil {
			return r
		}
	}
	return nil
}

// appliersMax bounds the workers making one overlay's queued changes at
// once: host calls on different names run in parallel, as the calls that
// queued them could have.
const appliersMax = 4

// startApplier starts another worker if the queue has a change none is
// making and fewer than appliersMax run. The caller holds f.mu exclusively.
func (f *FS) startApplier() {
	if f.appliers < appliersMax && f.next() != nil && (f.applyLimit == nil || *f.applyLimit > 0) {
		f.appliers++
		go f.applier()
	}
}

// applier is one worker: it makes queued changes ([FS.next]) until none is
// left it may make. A failure parks the queue, and the worker retries the
// change every queueRetry while others go on with unrelated ones.
func (f *FS) applier() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for {
		op := f.next()
		if op == nil || f.applyLimit != nil && *f.applyLimit <= 0 {
			f.appliers--
			return
		}
		if f.applyLimit != nil {
			*f.applyLimit--
		}
		op.making = true
		f.startApplier()
		for {
			f.mu.Unlock()
			err := op.apply()
			f.mu.Lock()
			if err == nil || op.ended {
				break
			}
			f.parkQueue(op, err)
			f.mu.Unlock()
			time.Sleep(queueRetry)
			f.mu.Lock()
			if op.ended {
				break // discarded with its upper meanwhile
			}
		}
		op.making = false
		if op.ended {
			continue
		}
		if f.parkedOp == op {
			f.parkedOp, f.parkedErr = nil, nil
			f.mem.unparkQueue(f)
		}
		if op.applied != nil {
			op.applied()
		}
		f.queued--
		f.end(op)
	}
}

// parkQueue records that queued change op failed with err.
func (f *FS) parkQueue(op *hostOp, err error) {
	if f.parkedErr == nil {
		f.mem.parkQueue(f, err)
	}
	f.parkedOp, f.parkedErr = op, err
	close(f.parkSignal)
	f.parkSignal = make(chan struct{})
}

// drainLocked waits, holding f.mu exclusively and releasing it while it
// waits, until no host operation is in flight or queued: what Flush and
// Freeze need before the upper on disk is complete. While the queue is
// parked it returns the failure instead of waiting for it.
func (f *FS) drainLocked() error {
	for len(f.ops) != 0 {
		op := f.ops[0]
		if op.queued && f.parkedErr != nil {
			if sync := f.find([]string{""}, func(o *hostOp) bool { return !o.queued }); sync != nil {
				f.waitOp(sync)
				continue
			}
			return f.parkedErr
		}
		signal := f.parkSignal
		f.mu.Unlock()
		select {
		case <-op.done:
		case <-signal:
		}
		f.mu.Lock()
	}
	return nil
}

// dropQueue forgets every queued change, for an upper about to be deleted.
// The caller holds f.mu exclusively.
func (f *FS) dropQueue() {
	for _, op := range slices.Clone(f.ops) {
		if op.queued {
			op.after = nil
			f.end(op)
		}
	}
	f.ready = nil
	f.queued = 0
	if f.parkedErr != nil {
		f.parkedOp, f.parkedErr = nil, nil
		f.mem.unparkQueue(f)
	}
}

// queueRemove removes upper regular file p, described by info, which has no
// lower entry to mask, from the index and queues its unlink; alias, another
// name of the file if not "", shows one link fewer. The identity is
// released right before the unlink, as the inode may be reused once the last
// name is gone.
func (f *FS) queueRemove(p string, info os.FileInfo, alias string) error {
	ids, real := f.ids, f.up(p)
	paths := append([]string{p}, f.otherNames(p)...)
	f.dropIndex(p)
	if alias != "" {
		if e := f.index.entries[alias]; e.Info != nil {
			if fewer, ok := relinked(unalias(e.Info), -1); ok {
				f.cacheInfo(alias, aliasInfo{FileInfo: fewer, name: path.Base(alias)})
			}
		}
	}
	err := f.touchDir(parent(p), true)
	f.enqueue(paths, func() error {
		if err := ids.release(info); err != nil {
			return err
		}
		// Unlike os.Remove on Windows, keeps a read-only hard link's other names read-only.
		return hostfile.Remove(real)
	}, nil)
	return err
}

// otherNames returns the file's names other than p the index holds: a
// queued change of its link count keeps them busy too, as a stat through
// any of them would show the count before the change.
func (f *FS) otherNames(p string) []string {
	key, ok := f.pathIDs[p]
	if !ok {
		return nil
	}
	names := f.aliases[key]
	var out []string
	if names.first != "" && names.first != p {
		out = append(out, names.first)
	}
	for n := range names.others {
		if n != p {
			out = append(out, n)
		}
	}
	return out
}

// queueRename moves upper regular file src, which has no lower entry to
// mask, to dst, absent or an upper regular file with no other name that it
// replaces (dinfo describes it then), in the index and queues the host
// rename. The file keeps its metadata.
func (f *FS) queueRename(src, dst string, dinfo os.FileInfo) error {
	replaced := f.hostFile(dst)
	marked := f.mayHoldMarker(dst)
	ids, from, to, marker := f.ids, f.up(src), f.up(dst), f.whiteoutPath(dst)
	f.dropIndex(dst)
	f.moveEntry(src, dst)
	f.dropIndex(src)
	e := f.index.entries[dst]
	e.Info = aliasInfo{FileInfo: unalias(e.Info), name: path.Base(dst)}
	f.index.set(dst, e)
	err := f.touchDir(parent(src), true)
	if parent(dst) != parent(src) {
		if terr := f.touchDir(parent(dst), true); err == nil {
			err = terr
		}
	}
	f.enqueue([]string{src, dst}, func() error {
		if replaced {
			// The rename removes the name of the file it replaces.
			if err := ids.release(dinfo); err != nil {
				return err
			}
		}
		if err := renameFile(from, to); err != nil {
			return err
		}
		if marked {
			return removeMarker(marker)
		}
		return nil
	}, nil)
	return err
}

// queueLink gives upper regular file src on disk the new name dst in an
// upper directory in the index, both names showing one link more, and
// queues the host link.
func (f *FS) queueLink(src, dst string) error {
	more, ok := relinked(unalias(f.index.entries[src].Info), 1)
	if !ok {
		return errNotQueued
	}
	marked := f.mayHoldMarker(dst)
	from, to, marker := f.up(src), f.up(dst), f.whiteoutPath(dst)
	paths := append([]string{src, dst}, f.otherNames(src)...)
	f.cacheInfo(dst, aliasInfo{FileInfo: more, name: path.Base(dst)})
	err := f.touchDir(parent(dst), true)
	f.enqueueChanging(paths, []string{dst}, func() error {
		if err := os.Link(from, to); err != nil {
			return err
		}
		if marked {
			return removeMarker(marker)
		}
		return nil
	}, nil)
	return err
}

// queueMkdir makes directory p, which no lower provides and no marker
// names, with permissions perm in the index and queues the host mkdir. Its
// identity is a new Synthetic one, persisted on the directory when it is
// made; until then the index describes it as the host will.
func (f *FS) queueMkdir(p string, perm os.FileMode) error {
	return f.queueMkdirAs(p, perm&^umask, 0, time.Now())
}

// queueMkdirAs is [FS.queueMkdir] for a directory with exactly mode, and
// identity id (0: a new one) and modification time t, which the guest of
// the kernel client chose.
func (f *FS) queueMkdirAs(p string, mode os.FileMode, id ident.ID, t time.Time) error {
	d := parent(p)
	pe := f.index.entries[d]
	if pe.Info == nil || pe.Info.Mode()&os.ModeSetgid != 0 {
		// The host gives a setgid directory's subdirectories its own bits.
		return errNotQueued
	}
	uid, gid := newOwner(pe.Info)
	info, ok := newDirInfo(path.Base(p), mode, t, uid, gid, id)
	more, ok2 := relinked(pe.Info, 1)
	if !ok || !ok2 {
		return errNotQueued
	}
	if id == 0 {
		var err error
		if id, err = f.ids.syn.allocate(); err != nil {
			return err
		}
		info, _ = newDirInfo(path.Base(p), mode, t, uid, gid, id)
	}
	f.index.set(p, entry(info))
	f.register(p, info)
	f.unsettled[p] = struct{}{} // made on disk later, with the time it is made then
	pe.Info = more
	f.index.set(d, pe)
	if err := f.touchDir(d, true); err != nil {
		return err
	}
	ids, real := f.ids, f.up(p)
	f.enqueue([]string{p}, func() error {
		if err := os.Mkdir(real, mode.Perm()); err != nil {
			return err
		}
		if err := os.Chmod(real, mode); err != nil {
			return err
		}
		if err := keepMode(real, mode); err != nil {
			return err
		}
		return ids.persist(real, id)
	}, nil)
	return nil
}

// queueRmdir removes empty directory p, described by info, which no lower
// provides, from the index and queues its removal.
func (f *FS) queueRmdir(p string, info os.FileInfo) error {
	d := parent(p)
	pe := f.index.entries[d]
	fewer, ok := relinked(pe.Info, -1)
	if !ok {
		return errNotQueued
	}
	ids, real := f.ids, f.up(p)
	f.dropIndex(p)
	pe.Info = fewer
	f.index.set(d, pe)
	err := f.touchDir(d, true)
	f.enqueue([]string{p}, func() error {
		// A directory the queue made is indexed without its inode, which
		// only the host has.
		if host, err := os.Lstat(real); err == nil {
			info = host
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := ids.release(info); err != nil {
			return err
		}
		// Merged-empty: only markers can remain beneath it.
		return os.RemoveAll(real)
	}, nil)
	return err
}

// dirInfo is the metadata of a directory the index has made before the
// host: what a host mkdir by this process would give it.
type dirInfo struct {
	name  string
	mode  os.FileMode
	mtime time.Time
	id    ident.ID
	sys   any
}

// Name returns the base name.
func (i dirInfo) Name() string { return i.name }

// Size is a new directory's.
func (dirInfo) Size() int64 { return 64 }

// Mode returns the directory's mode.
func (i dirInfo) Mode() os.FileMode { return i.mode | os.ModeDir }

// ModTime returns when it was made in the index.
func (i dirInfo) ModTime() time.Time { return i.mtime }

// IsDir is always true.
func (dirInfo) IsDir() bool { return true }

// Sys returns the native stat record a host mkdir would give it.
func (i dirInfo) Sys() any { return i.sys }

// Identity returns the Synthetic identity the directory keeps on disk.
func (i dirInfo) Identity() ident.ID { return i.id }

// errNotQueued tells a caller that a change cannot queue its host part
// after all, before it changed anything: it makes the change itself.
var errNotQueued = errors.New("overlay: change not queueable")

// Settle waits until the host holds every change the index shows: no host
// change is queued or in flight. Flush does as much and more; Settle is for
// callers that read the upper's names on disk without spilling memory
// files. It returns the parked queue's failure instead of waiting for it.
func (f *FS) Settle() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.drainLocked()
}
