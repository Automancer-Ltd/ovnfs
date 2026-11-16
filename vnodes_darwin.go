package main

import (
	"fmt"
	"math/bits"

	"golang.org/x/sys/unix"
)

// vnodeWarning explains a kern.maxvnodes too small for a full walk of
// checkout name, which holds files files, or returns "" when the cap covers it.
// Each walked file takes two vnodes: the mount client's and the daemon's for
// its source file. Past the cap the kernel recycles the walk's own vnodes and
// their name-cache entries before the next walk reaches them, so every warm
// git status sends one NFS LOOKUP per file. The status reply carries it.
func vnodeWarning(name string, files int) string {
	limit, err := unix.SysctlUint32("kern.maxvnodes")
	need := 2 * files
	if err != nil || int(limit) >= need {
		return ""
	}
	raise := 1 << bits.Len(uint(need-1))
	return fmt.Sprintf("kern.maxvnodes is %s, below the %s vnodes a full walk of checkout %s (%s files) needs, so each git status there looks every file up again; run `sudo sysctl kern.maxvnodes=%d` and add kern.maxvnodes=%d to /etc/sysctl.conf",
		commas(int64(limit)), commas(int64(need)), name, commas(int64(files)), raise, raise)
}
