//go:build linux

package testrunner

import (
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// FD9 is inherited from the unchanged worker's canonical flock subshell.
// Never open another holder, close/unlock FD9, or recursively block on flock.
func verifyWorkerFlock(name string) error {
	return verifyWorkerFlockWith(name, unix.Flock, unix.Open)
}

// The syscall seam keeps the real FD/inode checks and isolates filesystem errno controls.
func verifyWorkerFlockWith(name string, flock func(int, int) error, open func(string, int, uint32) (int, error)) error {
	return verifyWorkerFlockEvidence(name, flock, open, os.ReadFile)
}

func verifyWorkerFlockEvidence(name string, flock func(int, int) error, open func(string, int, uint32) (int, error), read func(string) ([]byte, error)) error {
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
	// Re-flocking the inherited OFD itself returns EACCES on CIFS. Inspect
	// its descriptor-local kernel proof instead. The acquiring helper may
	// already have exited, so its PID (including 0) is not an identity proof.
	raw, err := read("/proc/self/fdinfo/9")
	if err != nil {
		return fmt.Errorf("read worker FD9 lock proof: %w", err)
	}
	if err := workerFD9LockProof(raw, fd); err != nil {
		return err
	}
	// A separate open description must see exclusion. It is never a holder.
	probe, err := open(name, unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open independent worker-lock probe: %w", err)
	}
	defer unix.Close(probe)
	if err = flock(probe, unix.LOCK_EX|unix.LOCK_NB); err == nil {
		_ = flock(probe, unix.LOCK_UN)
		return errors.New("canonical flock exclusion not established")
	}
	// CIFS reports contention as EACCES here; an unsuccessful open above still refuses.
	if err != unix.EWOULDBLOCK && err != unix.EAGAIN && err != unix.EACCES {
		return fmt.Errorf("independent worker-lock conflict proof: %w", err)
	}
	return nil
}

func workerFD9LockProof(raw []byte, fd unix.Stat_t) error {
	if len(raw) > 64*1024 {
		return errors.New("oversized worker FD9 lock proof")
	}
	found := false
	for _, line := range strings.Split(string(raw), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || f[0] != "lock:" {
			continue
		}
		if found || len(f) != 9 || f[2] != "FLOCK" || f[3] != "ADVISORY" || f[4] != "WRITE" || f[7] != "0" || f[8] != "EOF" {
			return errors.New("invalid worker FD9 full-range FLOCK WRITE proof")
		}
		devino := strings.Split(f[6], ":")
		if len(devino) != 3 {
			return errors.New("invalid worker FD9 lock device/inode")
		}
		major, e1 := strconv.ParseUint(devino[0], 16, 32)
		minor, e2 := strconv.ParseUint(devino[1], 16, 32)
		ino, e3 := strconv.ParseUint(devino[2], 10, 64)
		if e1 != nil || e2 != nil || e3 != nil || major != uint64(unix.Major(fd.Dev)) || minor != uint64(unix.Minor(fd.Dev)) || ino != fd.Ino {
			return errors.New("worker FD9 lock device/inode mismatch")
		}
		found = true
	}
	if !found {
		return errors.New("missing worker FD9 full-range FLOCK WRITE proof")
	}
	return nil
}
