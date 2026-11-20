//go:build !windows

package kfs

import (
	"os"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"
)

// TestRoundTrip measures GETATTR round trips of KFS_RTT_CONNS connections
// (default 1) at once, 20,000 per connection, and logs their median and p99.
// It runs only with KFS_RTT set, against the kernel share at KFS_RTT_SOCK (a
// daemon's) or else an in-process fixture.
func TestRoundTrip(t *testing.T) {
	if os.Getenv("KFS_RTT") == "" {
		t.Skip("set KFS_RTT to run")
	}
	sock := os.Getenv("KFS_RTT_SOCK")
	if sock == "" {
		sock = newFixture(t, "vm").serve(t, "vm", true)
	}
	conns := 1
	if n, err := strconv.Atoi(os.Getenv("KFS_RTT_CONNS")); err == nil && n > 0 {
		conns = n
	}
	const calls = 20000
	took := make([][]time.Duration, conns)
	var wg sync.WaitGroup
	for i := range conns {
		c := dial(t, sock)
		wg.Go(func() {
			d := make([]time.Duration, calls)
			for j := range d {
				start := time.Now()
				if _, err := c.Getattr(RootID); err != nil {
					t.Error(err)
					return
				}
				d[j] = time.Since(start)
			}
			took[i] = d
		})
	}
	wg.Wait()
	all := slices.Concat(took...)
	slices.Sort(all)
	t.Logf("GETATTR x %d over %d connections: median %v p99 %v", len(all), conns, all[len(all)/2], all[len(all)*99/100])
}
