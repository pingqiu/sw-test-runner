//go:build !linux

package seaweed

import "errors"

func controllerMono() (float64, error) { return 0, errors.New("soak clock capture requires Linux") }
