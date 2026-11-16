//go:build !windows

package checkout

import (
	"errors"
	"os"
	"syscall"
)

// tryLock takes an exclusive nonblocking flock on f. It reports false when
// another open file holds the lock.
func tryLock(f *os.File) (bool, error) {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return false, nil
	}
	return err == nil, err
}
