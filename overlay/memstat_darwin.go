package overlay

import (
	"encoding/binary"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// memStat builds darwin stat data for a memory file.
func memStat(i memInfo) any {
	s := &syscall.Stat_t{Nlink: 1, Uid: i.uid, Gid: i.gid, Ino: uint64(i.id), Size: i.size, Blocks: (i.size + 511) / 512, Blksize: 4096}
	s.Mode = uint16(unixMode(i.mode))
	s.Atimespec = syscall.NsecToTimespec(i.atime.UnixNano())
	s.Mtimespec = syscall.NsecToTimespec(i.mtime.UnixNano())
	s.Ctimespec = syscall.NsecToTimespec(i.ctime.UnixNano())
	s.Birthtimespec = syscall.NsecToTimespec(i.created.UnixNano())
	return s
}

// setCreationTime keeps a memory file's birth time when it moves to disk.
func setCreationTime(p string, t time.Time) error {
	var data [16]byte
	binary.NativeEndian.PutUint64(data[:8], uint64(t.Unix()))
	binary.NativeEndian.PutUint64(data[8:], uint64(t.Nanosecond()))
	attrs := unix.Attrlist{Bitmapcount: unix.ATTR_BIT_MAP_COUNT, Commonattr: unix.ATTR_CMN_CRTIME}
	return unix.Setattrlist(p, &attrs, data[:], unix.FSOPT_NOFOLLOW)
}

// setTimes gives a staged memory file its creation, modification and access
// times in one setattrlist(2), where os.Chtimes and setCreationTime took a
// path lookup and a call each. A zero time leaves that time alone, as
// os.Chtimes does.
func setTimes(p string, atime, mtime, created time.Time) error {
	if atime.IsZero() || mtime.IsZero() || created.IsZero() {
		if err := os.Chtimes(p, atime, mtime); err != nil {
			return err
		}
		return setCreationTime(p, created)
	}
	// The buffer holds the attributes in bitmap order: creation,
	// modification, access, each a struct timespec.
	var data [48]byte
	for i, t := range [...]time.Time{created, mtime, atime} {
		binary.NativeEndian.PutUint64(data[16*i:], uint64(t.Unix()))
		binary.NativeEndian.PutUint64(data[16*i+8:], uint64(t.Nanosecond()))
	}
	attrs := unix.Attrlist{Bitmapcount: unix.ATTR_BIT_MAP_COUNT, Commonattr: unix.ATTR_CMN_CRTIME | unix.ATTR_CMN_MODTIME | unix.ATTR_CMN_ACCTIME}
	return unix.Setattrlist(p, &attrs, data[:], unix.FSOPT_NOFOLLOW)
}

// birthTime returns the creation time host metadata records, which a stored
// layer entry's copy-up restores.
func birthTime(info os.FileInfo) (time.Time, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st == nil {
		return time.Time{}, false
	}
	return time.Unix(st.Birthtimespec.Unix()), true
}

// newOwner returns the owner a file created now in directory dir gets: the
// effective uid, and the directory's group, which BSD semantics always
// inherit.
func newOwner(dir os.FileInfo) (uid, gid uint32) {
	uid, gid = uint32(os.Geteuid()), uint32(os.Getegid())
	if st, ok := dirStat(dir); ok {
		gid = st.Gid
	}
	return uid, gid
}
