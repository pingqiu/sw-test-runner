//go:build linux

package smoke

import (
	"crypto/sha256"
	"errors"
	"fmt"
	tr "github.com/pingqiu/sw-test-runner"
	"io"
	"os"
	"syscall"
)

func freshOwner(i os.FileInfo) bool {
	s, ok := i.Sys().(*syscall.Stat_t)
	return ok && (s.Uid == 0 || s.Uid == uint32(os.Geteuid()))
}

func openFreshFile(name string) (*os.File, error) {
	fd, e := syscall.Open(name, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if e != nil {
		return nil, e
	}
	return os.NewFile(uintptr(fd), name), nil
}
func sameFreshRecord(a, b os.FileInfo) bool {
	x, ok := a.Sys().(*syscall.Stat_t)
	y, yes := b.Sys().(*syscall.Stat_t)
	return ok && yes && os.SameFile(a, b) && a.Size() == b.Size() && a.Mode() == b.Mode() && a.ModTime() == b.ModTime() && x.Ctim == y.Ctim && x.Uid == y.Uid && x.Gid == y.Gid
}
func verifyFreshImage(pin tr.FilePin) error {
	name, e := os.Executable()
	if e != nil || name != pin.Path {
		return errors.New("wrong materializer path")
	}
	// This kernel reference follows the running image, including an unlinked
	// inode. The independently protected pathname must still name that image.
	f, e := os.Open("/proc/self/exe")
	if e != nil {
		return e
	}
	defer f.Close()
	before, e := f.Stat()
	if e != nil {
		return e
	}
	named, e := os.Lstat(pin.Path)
	if e != nil || !sameFreshRecord(before, named) || !before.Mode().IsRegular() || before.Size() > 512<<20 || !freshOwner(before) || before.Mode().Perm()&0022 != 0 {
		return errors.New("running materializer inode differs from protected install")
	}
	h := sha256.New()
	n, e := io.Copy(h, io.LimitReader(f, (512<<20)+1))
	if e != nil {
		return e
	}
	after, e := f.Stat()
	if e != nil {
		return e
	}
	terminal, e := os.Lstat(pin.Path)
	if e != nil || !sameFreshRecord(before, after) || !sameFreshRecord(before, terminal) || n != before.Size() || fmt.Sprintf("%x", h.Sum(nil)) != pin.SHA256 {
		return errors.New("running materializer image/pin changed")
	}
	return nil
}
