package checkout

import (
	"os"
	"path/filepath"
	"time"
)

// lockWait bounds how long lockState waits for another server to release the
// state lock, such as a daemon still flushing on its way out. A variable so
// tests can shorten it.
var lockWait = 60 * time.Second

// lockPoll is how often lockState tries the lock again while it waits.
const lockPoll = 50 * time.Millisecond

// lockState prevents another server from concurrently rewriting the same
// manifest. It polls a nonblocking lock, because a blocking one cannot stop at
// lockWait.
func lockState(state string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(state, "state.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, coded(err)
	}
	deadline := time.Now().Add(lockWait)
	for {
		held, err := tryLock(f)
		if err != nil {
			f.Close()
			return nil, coded(err)
		}
		if held {
			return f, nil
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, failure("exists", "state directory is already open: %s (waited %s)", state, lockWait)
		}
		time.Sleep(lockPoll)
	}
}
