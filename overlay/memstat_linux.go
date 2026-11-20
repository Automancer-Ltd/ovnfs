package overlay

import (
	"os"
	"syscall"
	"time"
)

// setCount stores n in a stat field whose width differs between
// architectures, such as Nlink.
func setCount[T ~uint16 | ~uint32 | ~uint64](field *T, n uint32) { *field = T(n) }

// memStat builds linux stat data for a memory file.
func memStat(i memInfo) any {
	s := &syscall.Stat_t{Uid: i.uid, Gid: i.gid, Ino: uint64(i.id), Size: i.size, Blocks: (i.size + 511) / 512, Blksize: 4096}
	setCount(&s.Nlink, i.nlink)
	s.Mode = unixMode(i.mode)
	s.Atim = syscall.NsecToTimespec(i.atime.UnixNano())
	s.Mtim = syscall.NsecToTimespec(i.mtime.UnixNano())
	s.Ctim = syscall.NsecToTimespec(i.ctime.UnixNano())
	return s
}

// setCreationTime leaves the Linux creation time to the filesystem.
func setCreationTime(string, time.Time) error { return nil }

// setTimes gives a staged memory file its access and modification times,
// leaving its creation time to the filesystem.
func setTimes(p string, atime, mtime, _ time.Time) error { return os.Chtimes(p, atime, mtime) }

// birthTime reports no creation time: Linux leaves it to the filesystem.
func birthTime(os.FileInfo) (time.Time, bool) { return time.Time{}, false }

// newOwner returns the owner a file created now in directory dir gets: the
// effective uid, and the effective gid unless dir is setgid, whose group it
// then inherits.
func newOwner(dir os.FileInfo) (uid, gid uint32) {
	uid, gid = euid, egid
	if st, ok := dirStat(dir); ok && st.Mode&syscall.S_ISGID != 0 {
		gid = st.Gid
	}
	return uid, gid
}
