//go:build linux

package testrunner

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestWorkerFlockConflictErrnos(t *testing.T) {
	if mode := os.Getenv("S0B_FLOCK_ERRNO_CHILD"); mode != "" {
		workerFlockErrnoChild(t, mode)
		return
	}
	for _, mode := range []string{"local-conflict", "cifs-conflict", "open-denied", "owner-denied", "second-lock-granted"} {
		t.Run(mode, func(t *testing.T) {
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			lock := filepath.Join(t.TempDir(), "canonical.lock")
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "bash", "-c", `exec "$1" -test.run '^TestWorkerFlockConflictErrnos$' -test.v 9>"$2"`, "flock-control", exe, lock)
			cmd.Env = append(os.Environ(), "S0B_FLOCK_ERRNO_CHILD="+mode, "S0B_FLOCK_ERRNO_PATH="+lock)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%s: %v\n%s", mode, err, out)
			}
		})
	}
}

func workerFlockErrnoChild(t *testing.T, mode string) {
	lock := os.Getenv("S0B_FLOCK_ERRNO_PATH")
	flock, open := unix.Flock, unix.Open
	probeSeen, granted := false, false
	flock = func(fd, how int) error {
		if fd == 9 && mode == "owner-denied" {
			return unix.EACCES
		}
		if fd == 9 && mode == "second-lock-granted" {
			return nil // Intentionally leave the real FD9 unlocked.
		}
		err := unix.Flock(fd, how)
		if fd != 9 && how == unix.LOCK_EX|unix.LOCK_NB {
			probeSeen = true
			if mode == "cifs-conflict" {
				if err != unix.EWOULDBLOCK && err != unix.EAGAIN {
					t.Fatalf("real independent contender was not excluded: %v", err)
				}
				return unix.EACCES // Confirmed CIFS errno for this same conflict.
			}
			granted = err == nil
		}
		return err
	}
	if mode == "open-denied" {
		open = func(string, int, uint32) (int, error) { return -1, unix.EACCES }
	}
	err := verifyWorkerFlockWith(lock, flock, open)
	switch mode {
	case "local-conflict", "cifs-conflict":
		if err != nil || !probeSeen {
			t.Fatalf("excluded independent contender refused: %v probe=%v", err, probeSeen)
		}
	case "open-denied", "owner-denied":
		if !errors.Is(err, unix.EACCES) || probeSeen {
			t.Fatalf("permission failure admitted: %v probe=%v", err, probeSeen)
		}
	case "second-lock-granted":
		if err == nil || !strings.Contains(err.Error(), "exclusion not established") || !granted {
			t.Fatalf("granted second lock admitted: %v granted=%v", err, granted)
		}
	default:
		t.Fatal("unrecognized control")
	}
}
