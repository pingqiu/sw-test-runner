//go:build !linux

package smoke

import "errors"

// procStart is Linux-only (the runner's run/destroy modes are Linux-only).
func procStart(pid int) (string, error) { return "", errors.New("process start time is Linux-only") }
