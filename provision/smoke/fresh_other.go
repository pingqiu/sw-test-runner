//go:build !linux

package smoke

import (
	"errors"
	tr "github.com/pingqiu/sw-test-runner"
	"os"
)

func freshOwner(os.FileInfo) bool                   { return false }
func openFreshFile(string) (*os.File, error)        { return nil, errors.New("Linux controller required") }
func sameFreshRecord(os.FileInfo, os.FileInfo) bool { return false }
func verifyFreshImage(tr.FilePin) error             { return errors.New("Linux controller required") }
