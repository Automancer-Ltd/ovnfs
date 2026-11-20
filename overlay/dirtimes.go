package overlay

import (
	"os"
	"path/filepath"
	"time"

	"github.com/stencil-hq/ovnfs/internal/ident"
)

// Directory times with the memory tier. A name change in an upper directory
// moves its modification time, and copy-up, spill and deletion markers must
// not: clients cache a directory's listing until its time changes. Without
// the memory tier the host keeps the times: each change stats the directory
// afterwards, and each invisible one restores the time it read first. With
// it, while the index is loaded, the index holds every upper directory's
// time and the host's may differ: a name change stamps the index alone
// ([FS.touchDir]), an invisible host change leaves it alone
// ([FS.preserveDirTime]), and neither makes a syscall. A directory whose host
// time differs is unsettled; [FS.settleDirs] writes the index's times to the
// host before anything reads the upper from disk: at [FS.Flush], [FS.Freeze]
// and [FS.Cool], and [FS.SyncFile] settles the directories it syncs. A crash
// leaves a time the host chose, which clients see as one more change.

// lazyDirs reports whether f keeps its directories' times in the index (see
// above): only with the memory tier, whose contract it extends.
func (f *FS) lazyDirs() bool { return f.mem != nil && f.index != nil }

// touchDir stamps directory d's modification and change times as a host
// create, unlink or rename in it would, for a name change made only in the
// index (a memory file's) or already made on the host. Where index entries
// can carry their own times this makes no syscall; elsewhere the host's
// clock stamps the directory and the index reads it back.
func (f *FS) touchDir(d string, onHost bool) error {
	if f.lazyDirs() {
		if e, ok := f.index.entries[d]; ok && e.Kind == KindDir {
			now := time.Now()
			if info, ok := retimed(e.Info, now, now); ok {
				e.Info = info
				f.index.set(d, e)
				f.unsettled[d] = struct{}{}
				return nil
			}
		}
	}
	if !onHost {
		if err := os.Chtimes(f.up(d), time.Time{}, time.Now()); err != nil {
			return err
		}
	}
	return f.refreshDir(d)
}

// preserveDirTime runs fn, a host change in directory d that clients must
// not see as one (a copy-up, a spill, a deletion marker), keeping d's time.
func (f *FS) preserveDirTime(d string, fn func() error) error {
	if f.lazyDirs() {
		if _, ok := f.index.entries[d]; ok {
			err := fn()
			f.unsettled[d] = struct{}{}
			return err
		}
	}
	info, err := os.Lstat(f.up(d))
	if err != nil {
		return err
	}
	opErr := fn()
	timeErr := os.Chtimes(f.up(d), info.ModTime(), info.ModTime())
	refreshErr := f.refresh(d)
	if opErr != nil {
		return opErr
	}
	if timeErr != nil {
		return timeErr
	}
	return refreshErr
}

// refreshDir indexes directory d's host metadata after a visible change of
// its names on the host. An unsettled d keeps the index's later time: the
// host's clock may lag it (Linux stamps files with a coarse clock).
func (f *FS) refreshDir(d string) error {
	info, err := f.stat(d)
	if err != nil {
		return err
	}
	if _, ok := f.unsettled[d]; ok {
		if logical := f.index.entries[d].Info; logical != nil && info.ModTime().Before(logical.ModTime()) {
			if timed, ok := retimed(info, logical.ModTime(), time.Time{}); ok {
				info = timed
			} else {
				delete(f.unsettled, d)
			}
		} else {
			delete(f.unsettled, d)
		}
	}
	f.cacheInfo(d, info)
	return nil
}

// refreshUnsettled indexes unsettled directory d's host metadata after a
// change the index's time hides from clients: d keeps its indexed time.
// Where entries cannot carry their own times the host takes it now.
func (f *FS) refreshUnsettled(d string) error {
	info, err := f.stat(d)
	if err != nil {
		return err
	}
	logical := f.index.entries[d].Info
	if logical == nil {
		delete(f.unsettled, d)
		f.cacheInfo(d, info)
		return nil
	}
	if timed, ok := retimed(info, logical.ModTime(), time.Time{}); ok {
		f.cacheInfo(d, timed)
		return nil
	}
	if err := f.settleDir(d, f.up(d)); err != nil {
		return err
	}
	if info, err = f.stat(d); err != nil {
		return err
	}
	f.cacheInfo(d, info)
	return nil
}

// settleDir writes directory d's indexed time to the host directory real
// and forgets that it was unsettled.
func (f *FS) settleDir(d, real string) error {
	e, ok := f.index.entries[d]
	if !ok || e.Info == nil {
		delete(f.unsettled, d)
		return nil
	}
	if err := os.Chtimes(real, time.Time{}, e.Info.ModTime()); err != nil {
		return err
	}
	delete(f.unsettled, d)
	return nil
}

// settleDirs settles every unsettled directory of the upper rooted at root.
// The caller holds f.mu exclusively.
func (f *FS) settleDirs(root string) error {
	for d := range f.unsettled {
		if err := f.settleDir(d, filepath.Join(root, filepath.FromSlash(d))); err != nil {
			return err
		}
	}
	return nil
}

// settleAncestors writes the indexed times of p's unsettled ancestors to the
// host without forgetting them, for [FS.SyncFile], which holds f.mu shared:
// a later settle writes the same times again.
func (f *FS) settleAncestors(p string) error {
	if len(f.unsettled) == 0 {
		return nil
	}
	for d := parent(p); ; d = parent(d) {
		if _, ok := f.unsettled[d]; ok {
			if err := os.Chtimes(f.up(d), time.Time{}, f.index.entries[d].Info.ModTime()); err != nil {
				return err
			}
		}
		if d == "" {
			return nil
		}
	}
}

// moveUnsettled carries the unsettled state of directory a to b, where a
// rename moved it on the host.
func (f *FS) moveUnsettled(a, b string) {
	if _, ok := f.unsettled[a]; ok {
		delete(f.unsettled, a)
		f.unsettled[b] = struct{}{}
	}
}

// timedInfo is a directory's host metadata with the times the index keeps
// for it, in its native stat record too (see [retimed]).
type timedInfo struct {
	os.FileInfo
	mtime time.Time
	sys   any
}

// ModTime returns the indexed modification time.
func (i timedInfo) ModTime() time.Time { return i.mtime }

// Sys returns the native stat record with the indexed modification and change times.
func (i timedInfo) Sys() any { return i.sys }

// Identity forwards the identity of the directory, which embedding hides.
func (i timedInfo) Identity() ident.ID { id, _ := ident.Of(i.FileInfo); return id }

// untimed returns the host metadata beneath a [timedInfo].
func untimed(info os.FileInfo) os.FileInfo {
	if t, ok := info.(timedInfo); ok {
		return t.FileInfo
	}
	return info
}
