//go:build linux

package testrunner

import (
	"errors"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
)

// FD9 is inherited from the unchanged worker's canonical flock subshell.
// Never open another holder, close/unlock FD9, or recursively block on flock.
func verifyWorkerFlock(name string) error {
	return verifyWorkerFlockWith(name, unix.Flock, unix.Open)
}

// The syscall seam keeps the real FD/inode checks and isolates filesystem errno controls.
func verifyWorkerFlockWith(name string, flock func(int, int) error, open func(string, int, uint32) (int, error)) error {
	if !filepath.IsAbs(name) || filepath.Clean(name) != name || name == "/" {
		return errors.New("noncanonical worker lock")
	}
	info, err := os.Lstat(name)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("worker lock is not regular")
	}
	var fd, at unix.Stat_t
	if err = unix.Fstat(9, &fd); err != nil {
		return err
	}
	if err = unix.Lstat(name, &at); err != nil {
		return err
	}
	if fd.Dev != at.Dev || fd.Ino != at.Ino || fd.Mode&unix.S_IFMT != unix.S_IFREG {
		return errors.New("inherited FD9 does not match canonical lock device/inode")
	}
	if err = flock(9, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return err
	}
	// A separate open description must see exclusion. It is never a holder.
	probe, err := open(name, unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(probe)
	if err = flock(probe, unix.LOCK_EX|unix.LOCK_NB); err == nil {
		_ = flock(probe, unix.LOCK_UN)
		return errors.New("canonical flock exclusion not established")
	}
	// CIFS reports contention as EACCES here; an unsuccessful open above still refuses.
	if err != unix.EWOULDBLOCK && err != unix.EAGAIN && err != unix.EACCES {
		return err
	}
	return nil
}
