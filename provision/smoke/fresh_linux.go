//go:build linux

package smoke

import (
	"os"
	"syscall"
)

func freshOwner(i os.FileInfo) bool {
	s, ok := i.Sys().(*syscall.Stat_t)
	return ok && (s.Uid == 0 || s.Uid == uint32(os.Geteuid()))
}
