package checkout

import (
	"strings"
	"testing"
	"time"
)

// TestLockStateWaitsForTheHolder releases a held state lock after 300 ms, as a
// daemon in its final flush does: a second lockState waits and then takes it.
func TestLockStateWaitsForTheHolder(t *testing.T) {
	state := t.TempDir()
	held, err := lockState(state)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	time.AfterFunc(300*time.Millisecond, func() { held.Close() })
	f, err := lockState(state)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if waited := time.Since(start); waited < 250*time.Millisecond {
		t.Fatalf("took the lock after %s, before its holder released it", waited)
	}
}

// TestLockStateGivesUpAfterTheBound keeps the state lock held: lockState waits
// lockWait, then fails as a second open of the state directory.
func TestLockStateGivesUpAfterTheBound(t *testing.T) {
	saved := lockWait
	lockWait = 200 * time.Millisecond
	t.Cleanup(func() { lockWait = saved })
	state := t.TempDir()
	held, err := lockState(state)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	start := time.Now()
	f, err := lockState(state)
	if err == nil {
		f.Close()
		t.Fatal("took a lock that is still held")
	}
	if waited := time.Since(start); waited < lockWait {
		t.Fatalf("gave up after %s, want at least %s", waited, lockWait)
	}
	if msg := err.Error(); !strings.Contains(msg, "already open") || !strings.Contains(msg, "waited 200ms") {
		t.Fatalf("error = %q, want already open and waited 200ms", msg)
	}
}
