//go:build unix

package prove

import (
	"os/exec"
	"syscall"
)

// setProcessGroup makes cancellation kill the whole test process tree, not
// just the shell, so a child cannot keep running against reverted files.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
