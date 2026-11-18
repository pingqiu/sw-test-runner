//go:build !linux

package smoke

import "os/exec"

// killGroup and retireGroup are Linux-only (lifecycle commands run on the Linux
// controller); elsewhere exec.CommandContext's default kill of the direct child
// applies and nothing is retired after the leader exits.
func killGroup(cmd *exec.Cmd) {}

func retireGroup(cmd *exec.Cmd) {}
