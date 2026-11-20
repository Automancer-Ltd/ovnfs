package lowprio

import "golang.org/x/sys/unix"

// Thread demotes the calling thread, which the caller's goroutine is locked to.
func Thread() error {
	return unix.Setpriority(unix.PRIO_DARWIN_THREAD, 0, unix.PRIO_DARWIN_BG)
}
