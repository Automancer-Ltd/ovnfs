//go:build !windows

package proc

import (
	"os/exec"
	"syscall"
)

// setProcessGroup starts the child in its own process group and makes
// cancellation kill the whole group: a command run through the shell, or git
// running a helper such as git-lfs or ssh, may leave a grandchild that would
// otherwise survive.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
}
