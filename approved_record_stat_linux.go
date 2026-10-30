//go:build linux

package testrunner

import (
	"errors"
	"os"
	"syscall"
)

func recordUID(info os.FileInfo) (uint32, error) {
	s, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, errors.New("record UID unavailable")
	}
	return s.Uid, nil
}

func openRecordFile(name string) (*os.File, error) {
	fd, err := syscall.Open(name, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}
func sameRecord(a, b os.FileInfo) bool {
	x, ok := a.Sys().(*syscall.Stat_t)
	y, yes := b.Sys().(*syscall.Stat_t)
	return ok && yes && os.SameFile(a, b) && a.Size() == b.Size() && a.Mode() == b.Mode() && a.ModTime() == b.ModTime() && x.Ctim == y.Ctim && x.Uid == y.Uid && x.Gid == y.Gid
}
