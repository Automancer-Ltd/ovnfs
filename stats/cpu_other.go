//go:build !darwin && !linux && !windows

package stats

import "time"

// cpuTimes is CPU time used so far.
type cpuTimes struct{ user, sys time.Duration }

// processCPU returns zero: CPU time is not measured on this platform.
func processCPU() cpuTimes { return cpuTimes{} }

// threadCPU returns zero: CPU time is not measured on this platform.
func threadCPU() time.Duration { return 0 }
