package stats

import (
	"time"

	"golang.org/x/sys/windows"
)

// cpuTimes is CPU time used so far.
type cpuTimes struct{ user, sys time.Duration }

// processCPU returns the CPU time of the process; zero when the OS refuses.
func processCPU() cpuTimes {
	var created, exited, kernel, user windows.Filetime
	if windows.GetProcessTimes(windows.CurrentProcess(), &created, &exited, &kernel, &user) != nil {
		return cpuTimes{}
	}
	return cpuTimes{user: ticks(user), sys: ticks(kernel)}
}

// ticks converts a duration counted in 100 ns units.
func ticks(t windows.Filetime) time.Duration {
	return time.Duration(int64(t.HighDateTime)<<32|int64(t.LowDateTime)) * 100
}

// threadCPU returns zero: thread CPU time is not measured on Windows, so a
// job records its duration only.
func threadCPU() time.Duration { return 0 }
