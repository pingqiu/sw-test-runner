//go:build !linux

package testrunner

import "errors"

func verifyWorkerFlock(string) error {
	return errors.New("worker-owned lease requires Linux inherited canonical flock")
}
