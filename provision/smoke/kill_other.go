//go:build !linux

package smoke

import "os/exec"

// killGroup is Linux-only (lifecycle commands run on the Linux controller);
// elsewhere exec.CommandContext's default kill of the direct child applies.
func killGroup(cmd *exec.Cmd) {}
