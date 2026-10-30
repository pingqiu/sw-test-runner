//go:build !linux

package testrunner

import (
	"errors"
	"os"
)

func recordUID(os.FileInfo) (uint32, error) {
	return 0, errors.New("S0b record ownership requires Linux")
}
func sameRecord(a, b os.FileInfo) bool { return false }
func openRecordFile(string) (*os.File, error) {
	return nil, errors.New("S0b record capture requires Linux")
}
