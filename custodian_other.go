//go:build !linux

package testrunner

import "errors"

func OwnedEntrypoint(args []string) (bool, error) {
	if len(args) > 0 && (args[0] == "owned-custodian" || args[0] == "owned-exec") {
		return true, errors.New("owned custody requires Linux pidfds")
	}
	return false, nil
}
