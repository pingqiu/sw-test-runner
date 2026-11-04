//go:build !linux

package seaweed

import (
	"context"
	"errors"
)

// runDirectSoak needs the M01 controller clock, so it only runs on Linux.
func runDirectSoak(context.Context, []byte, CloudConfig) (string, error) {
	return "", errors.New("kv_soak requires Linux (the controller runs on M01)")
}
