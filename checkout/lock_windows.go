//go:build windows

package checkout

import (
	"os"
	"syscall"
	"unsafe"
)

// tryLock takes an exclusive nonblocking byte-range lock on f, held until the
// file closes. It reports false when another handle holds the lock.
func tryLock(f *os.File) (bool, error) {
	lock := syscall.NewLazyDLL("kernel32.dll").NewProc("LockFileEx")
	var overlapped syscall.Overlapped
	result, _, callErr := lock.Call(f.Fd(), 3, 0, 1, 0, uintptr(unsafe.Pointer(&overlapped)))
	if result != 0 {
		return true, nil
	}
	if callErr == syscall.Errno(33) {
		return false, nil
	}
	return false, callErr
}
