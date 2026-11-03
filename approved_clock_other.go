//go:build !linux

package testrunner

import "errors"

func authorizeMono() (float64, error) { return 0, errors.New("authorize clock requires Linux") }
