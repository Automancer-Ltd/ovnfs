// Package proc prepares the child processes git operations start, so that
// cancelling one ends everything it started and never leaves the caller
// waiting on a grandchild's pipes.
package proc

import (
	"os/exec"
	"time"
)

// Prepare makes cancellation of cmd kill the child and everything it started,
// and bounds how long Wait waits for pipes a grandchild keeps open.
func Prepare(cmd *exec.Cmd) {
	setProcessGroup(cmd)
	cmd.WaitDelay = 2 * time.Second
}
