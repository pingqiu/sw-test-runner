//go:build !linux

package testrunner

import "errors"

func generationMonotonic() (float64, error) {
	return 0, errors.New("generation restart requires Linux monotonic clock")
}
