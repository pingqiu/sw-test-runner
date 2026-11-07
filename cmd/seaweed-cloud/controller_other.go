//go:build !linux

package main

import (
	"encoding/json"
	"io"
)

// The controller identity reads /proc; direct runs only on a Linux controller.
func recordController(string, string) error  { return nil }
func writeResult(string, directResult) error { return nil }
func directStatus(_ []string, w io.Writer) int {
	_ = json.NewEncoder(w).Encode(map[string]string{"schema": "seaweed.direct-status/v1", "state": "LINUX_REQUIRED"})
	return 2
}
