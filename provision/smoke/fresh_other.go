//go:build !linux

package smoke

import "os"

func freshOwner(os.FileInfo) bool { return false }
