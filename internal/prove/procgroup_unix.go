//go:build unix

package prove

import (
	"errors"
	"os/exec"
	"syscall"
)

// processAlive reports whether pid exists (EPERM still means it does).
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// setProcessGroup makes cancellation kill the whole test process tree, not
// just the shell, so a child cannot keep running against reverted files.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
