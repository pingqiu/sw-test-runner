//go:build linux

package smoke

import (
	"os/exec"
	"syscall"
)

// killGroup puts the tool in its own process group and, when its context
// ends, kills the whole group: terraform's provider plugins are its children
// and would otherwise outlive the work/hard deadline (deadline watchdog).
func killGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
