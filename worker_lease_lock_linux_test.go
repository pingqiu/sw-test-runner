//go:build linux

package testrunner

import (
	"context"
	"errors"
	"fmt"
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
	for _, mode := range []string{"local-conflict", "cifs-conflict", "open-denied", "cifs-inherited", "second-lock-granted", "no-lock-line", "wrong-ino", "wrong-dev", "read-lock", "posix-lock", "short-range", "missing-fdinfo", "unheld-fd9", "foreign-fd9"} {
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
	if mode != "unheld-fd9" {
		if err := unix.Flock(9, unix.LOCK_EX|unix.LOCK_NB); err != nil {
			t.Fatal(err)
		}
	}
	flock = func(fd, how int) error {
		if fd == 9 {
			t.Fatal("verifier must never re-flock FD9 (CIFS returns EACCES)")
		}
		err := unix.Flock(fd, how)
		if fd != 9 && how == unix.LOCK_EX|unix.LOCK_NB {
			probeSeen = true
			if mode == "cifs-conflict" || mode == "cifs-inherited" {
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
	read := func(name string) ([]byte, error) {
		if name != "/proc/self/fdinfo/9" {
			t.Fatalf("foreign proof path %q", name)
		}
		raw, err := os.ReadFile(name)
		if err != nil {
			return nil, err
		}
		var st unix.Stat_t
		if err := unix.Fstat(9, &st); err != nil {
			t.Fatal(err)
		}
		token := fmt.Sprintf("%02x:%02x:%d", unix.Major(st.Dev), unix.Minor(st.Dev), st.Ino)
		s := string(raw)
		switch mode {
		case "no-lock-line":
			var lines []string
			for _, line := range strings.Split(s, "\n") {
				if !strings.HasPrefix(line, "lock:") {
					lines = append(lines, line)
				}
			}
			s = strings.Join(lines, "\n")
		case "wrong-ino":
			s = strings.ReplaceAll(s, token, fmt.Sprintf("%02x:%02x:%d", unix.Major(st.Dev), unix.Minor(st.Dev), st.Ino+1))
		case "wrong-dev":
			s = strings.ReplaceAll(s, token, fmt.Sprintf("%02x:%02x:%d", unix.Major(st.Dev), unix.Minor(st.Dev)+1, st.Ino))
		case "read-lock":
			s = strings.ReplaceAll(s, "WRITE", "READ")
		case "posix-lock":
			s = strings.ReplaceAll(s, "FLOCK", "POSIX")
		case "short-range":
			s = strings.ReplaceAll(s, " 0 EOF", " 0 100")
		case "missing-fdinfo":
			return nil, os.ErrNotExist
		case "second-lock-granted":
			// A seemingly valid captured row must not bypass the live contender.
			if err := unix.Flock(9, unix.LOCK_UN); err != nil {
				t.Fatal(err)
			}
		}
		return []byte(s), nil
	}
	if mode == "foreign-fd9" {
		lock += ".foreign"
		if err := os.WriteFile(lock, nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	err := verifyWorkerFlockEvidence(lock, flock, open, read)
	switch mode {
	case "local-conflict", "cifs-conflict", "cifs-inherited":
		if err != nil || !probeSeen {
			t.Fatalf("excluded independent contender refused: %v probe=%v", err, probeSeen)
		}
	case "open-denied":
		if !errors.Is(err, unix.EACCES) || probeSeen {
			t.Fatalf("permission failure admitted: %v probe=%v", err, probeSeen)
		}
	case "second-lock-granted":
		if err == nil || !strings.Contains(err.Error(), "exclusion not established") || !granted {
			t.Fatalf("granted second lock admitted: %v granted=%v", err, granted)
		}
	case "no-lock-line", "wrong-ino", "wrong-dev", "read-lock", "posix-lock", "short-range", "missing-fdinfo", "unheld-fd9", "foreign-fd9":
		if err == nil || probeSeen {
			t.Fatalf("invalid inherited proof admitted: %v probe=%v", err, probeSeen)
		}
	default:
		t.Fatal("unrecognized control")
	}
}
