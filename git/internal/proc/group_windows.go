//go:build windows

package proc

import "os/exec"

// setProcessGroup keeps exec's default cancellation, killing the child.
func setProcessGroup(cmd *exec.Cmd) {}
