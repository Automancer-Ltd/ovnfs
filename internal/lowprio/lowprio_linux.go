package lowprio

import (
	"errors"

	"golang.org/x/sys/unix"
)

// ioprio_set(2) arguments: the thread named by its id, in the idle class.
const (
	ioprioWhoProcess = 1
	ioprioClassIdle  = 3 << 13
)

// Thread demotes the calling thread, which the caller's goroutine is locked to.
func Thread() error {
	tid := unix.Gettid()
	err := unix.Setpriority(unix.PRIO_PROCESS, tid, 19)
	if _, _, errno := unix.Syscall(unix.SYS_IOPRIO_SET, ioprioWhoProcess, uintptr(tid), ioprioClassIdle); errno != 0 {
		err = errors.Join(err, errno)
	}
	return err
}
