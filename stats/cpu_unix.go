//go:build darwin || linux

package stats

import (
	"time"

	"golang.org/x/sys/unix"
)

// cpuTimes is CPU time used so far.
type cpuTimes struct{ user, sys time.Duration }

// processCPU returns the CPU time of the process; zero when the OS refuses.
func processCPU() cpuTimes {
	var ru unix.Rusage
	if unix.Getrusage(unix.RUSAGE_SELF, &ru) != nil {
		return cpuTimes{}
	}
	return cpuTimes{user: time.Duration(ru.Utime.Nano()), sys: time.Duration(ru.Stime.Nano())}
}

// threadCPU returns the CPU time of the calling thread; zero when the OS refuses.
func threadCPU() time.Duration {
	var ts unix.Timespec
	if unix.ClockGettime(unix.CLOCK_THREAD_CPUTIME_ID, &ts) != nil {
		return 0
	}
	return time.Duration(ts.Nano())
}
