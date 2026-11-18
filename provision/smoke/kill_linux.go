//go:build linux

package smoke

import (
	"os/exec"
	"syscall"
)

// killGroup puts the tool in its own process group and, when its context
// ends while it is still running, kills the whole group: terraform's provider
// plugins are its children and would otherwise outlive the work/hard deadline
// (deadline watchdog).
func killGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

// retireGroup kills whatever is left in the tool's process group after its
// leader exited - normally, with an error, or cancelled. CommandContext stops
// watching the context once the leader is reaped, so a child the tool left
// behind (`sleep 600 & exit 0`) would otherwise outlive the work and cost
// deadlines. Call it after Run/Output/Wait returns; ESRCH (group already
// empty) is not an error.
func retireGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
