//go:build linux

package base

import (
	"os"
	"syscall"
	"time"

	"github.com/stencil-hq/ovnfs/internal/durable"
	"golang.org/x/sys/unix"
)

// volumeID distinguishes filesystems that cannot share reflink extents.
func volumeID(p string) (uint64, error) {
	info, err := os.Stat(p)
	if err != nil {
		return 0, err
	}
	return uint64(info.Sys().(*syscall.Stat_t).Dev), nil
}

// fileIdentity returns the device and inode that tell one file from a lookalike.
func fileIdentity(info os.FileInfo, _, _ string) (uint64, uint64, error) {
	s := info.Sys().(*syscall.Stat_t)
	return uint64(s.Dev), uint64(s.Ino), nil
}

// birthTime returns when the entry at host was made, in Unix nanoseconds;
// false for a filesystem or kernel that reports no birth time. It costs a
// statx: the stat data info carries has none.
func birthTime(host string, _ os.FileInfo) (int64, bool) {
	var stx unix.Statx_t
	if err := unix.Statx(unix.AT_FDCWD, host, unix.AT_SYMLINK_NOFOLLOW|unix.AT_STATX_DONT_SYNC, unix.STATX_BTIME, &stx); err != nil || stx.Mask&unix.STATX_BTIME == 0 {
		return 0, false
	}
	return stx.Btime.Sec*int64(time.Second) + int64(stx.Btime.Nsec), true
}

// onVolume reports whether info describes an entry of volume (see volumeID).
func onVolume(info os.FileInfo, volume uint64) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return !ok || uint64(st.Dev) == volume
}

// setTimes preserves symlink timestamps rather than changing their targets.
func setTimes(p string, t time.Time, symlink bool) error {
	flags := 0
	if symlink {
		flags = unix.AT_SYMLINK_NOFOLLOW
	}
	ts := unix.NsecToTimespec(t.UnixNano())
	return unix.UtimesNanoAt(unix.AT_FDCWD, p, []unix.Timespec{ts, ts}, flags)
}

// syncDirectory makes the metadata rename durable across crashes.
func syncDirectory(p string) error { return durable.Dir(p) }
