package overlay

import (
	"os"
	"syscall"
	"time"

	"github.com/stencil-hq/ovnfs/internal/ident"
)

// retimed returns directory metadata info with modification time mtime and
// change time ctime (zero: info's own), in its native stat record too, which
// frontends read for change times.
func retimed(info os.FileInfo, mtime, ctime time.Time) (os.FileInfo, bool) {
	info = untimed(info)
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st == nil {
		return nil, false
	}
	c := *st
	c.Mtim = syscall.NsecToTimespec(mtime.UnixNano())
	if !ctime.IsZero() {
		c.Ctim = syscall.NsecToTimespec(ctime.UnixNano())
	}
	return timedInfo{FileInfo: info, mtime: mtime, sys: &c}, true
}

// queueHost: the index can describe queued host changes here (see [FS.queueable]).
const queueHost = true

// relinked returns metadata info with its link count changed by delta, in
// its native stat record too; false when info carries none.
func relinked(info os.FileInfo, delta int) (os.FileInfo, bool) {
	inner, mtime := untimed(info), info.ModTime()
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st == nil {
		return nil, false
	}
	c := *st
	for ; delta > 0; delta-- {
		c.Nlink++
	}
	for ; delta < 0 && c.Nlink > 1; delta++ {
		c.Nlink--
	}
	return timedInfo{FileInfo: inner, mtime: mtime, sys: &c}, true
}

// newDirInfo describes directory name as a host mkdir by this process,
// with mode perm and owner uid and gid, would at time t, under identity id.
func newDirInfo(name string, perm os.FileMode, t time.Time, uid, gid uint32, id ident.ID) (os.FileInfo, bool) {
	ts := syscall.NsecToTimespec(t.UnixNano())
	mode := unixMode(perm)&^syscall.S_IFMT | syscall.S_IFDIR
	st := &syscall.Stat_t{Mode: mode, Nlink: 2, Uid: uid, Gid: gid, Size: 64, Blksize: 4096}
	st.Atim, st.Mtim, st.Ctim = ts, ts, ts
	return dirInfo{name: name, mode: perm, mtime: t, id: id, sys: st}, true
}
