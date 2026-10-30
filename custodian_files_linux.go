//go:build linux

package testrunner

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"golang.org/x/sys/unix"
)

// Execute the bytes we hashed, not a pathname that may change after enrollment.
func snapshotExecutable(filename, expected string) (_ *os.File, err error) {
	sourceFD, err := unix.Open(filename, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	source := os.NewFile(uintptr(sourceFD), filename)
	defer source.Close()
	info, err := source.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&0111 == 0 || info.Size() > 512<<20 {
		return nil, errors.New("executable must be a regular executable <=512 MiB")
	}
	fd, err := unix.MemfdCreate("owned-executable", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), "owned-executable")
	defer func() {
		if err != nil {
			_ = file.Close()
		}
	}()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(file, h), io.LimitReader(source, (512<<20)+1))
	if err != nil {
		return nil, err
	}
	if n > 512<<20 || hex.EncodeToString(h.Sum(nil)) != expected {
		return nil, errors.New("executable digest mismatch")
	}
	if err = file.Chmod(0500); err != nil {
		return nil, err
	}
	if _, err = unix.FcntlInt(file.Fd(), unix.F_ADD_SEALS, unix.F_SEAL_WRITE|unix.F_SEAL_GROW|unix.F_SEAL_SHRINK|unix.F_SEAL_SEAL); err != nil {
		return nil, err
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return file, nil
}

// Pin a private directory with no symlink ancestor, then create every log
// relative to that descriptor. No pathname re-resolution can escape the root.
func custodyLogRoot(dirname string) (*os.File, error) {
	if !path.IsAbs(dirname) || dirname == "/" || path.Clean(dirname) != dirname {
		return nil, errors.New("invalid output root")
	}
	fd, err := unix.Openat2(unix.AT_FDCWD, dirname, &unix.OpenHow{Flags: unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC, Resolve: unix.RESOLVE_NO_SYMLINKS})
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), dirname)
	var stat unix.Stat_t
	if err = unix.Fstat(fd, &stat); err != nil || stat.Uid != uint32(os.Geteuid()) || stat.Mode&0077 != 0 {
		f.Close()
		return nil, fmt.Errorf("output root is not private and owned: %w", errors.Join(err, errors.New("ownership/mode check")))
	}
	return f, nil
}

func custodyLog(root *os.File, dirname, filename string) (*os.File, error) {
	if path.Clean(filename) != filename || !strings.HasPrefix(filename, dirname+"/") {
		return nil, errors.New("log outside output root")
	}
	rel := strings.TrimPrefix(filename, dirname+"/")
	fd, err := unix.Openat2(int(root.Fd()), rel, &unix.OpenHow{Flags: unix.O_CREAT | unix.O_EXCL | unix.O_WRONLY | unix.O_CLOEXEC, Mode: 0600, Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS})
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), filename), nil
}
