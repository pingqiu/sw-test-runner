//go:build linux

package testrunner

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestWorkerRecordMetadataSettle(t *testing.T) {
	for _, mode := range []string{"local", "cifs-once", "cifs-late", "window-boundary-change", "stabilizes-too-late", "keeps-moving", "content-changed", "capture-unstable", "missing", "original-deadline", "capture-over-deadline"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "acquire.json")
			expected := []byte("{\"record\":1}\n")
			if err := os.WriteFile(path, expected, 0600); err != nil {
				t.Fatal(err)
			}
			base, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			clock := time.Unix(1000, 0)
			start := clock
			hard := clock.Add(time.Minute)
			if mode == "original-deadline" {
				hard = clock.Add(50 * time.Millisecond)
			}
			stats, captures, sleeps := 0, 0, 0
			changed := false
			stat := func(name string) (os.FileInfo, error) {
				stats++
				if name != path {
					t.Fatal("foreign record")
				}
				if mode == "missing" {
					return nil, os.ErrNotExist
				}
				mtime := base.ModTime()
				shift := time.Duration(0)
				if mode == "cifs-once" && stats == 2 {
					shift = 42 * time.Millisecond
				}
				if mode == "keeps-moving" {
					shift = time.Duration(stats) * time.Millisecond
				}
				if mode == "stabilizes-too-late" && clock.Sub(start) <= 5*time.Second {
					shift = time.Duration(stats) * time.Millisecond
				}
				at := map[string]time.Duration{"cifs-late": time.Second, "window-boundary-change": 2500 * time.Millisecond}
				if when, ok := at[mode]; ok && !changed && clock.Sub(start) >= when {
					shift = 42 * time.Millisecond
					changed = true
				}
				if shift != 0 {
					if err := os.Chtimes(name, mtime, mtime.Add(shift)); err != nil {
						t.Fatal(err)
					}
				}
				return os.Lstat(name)
			}
			capture := func(name string, owner *uint32, protected bool) ([]byte, error) {
				captures++
				if owner != nil || protected || name != path {
					t.Fatal("capture contract changed")
				}
				if mode == "cifs-once" && stats < 3 {
					return nil, errors.New("record changed during capture")
				}
				if mode == "capture-unstable" {
					return nil, errors.New("record changed during capture")
				}
				if mode == "capture-over-deadline" {
					clock = hard
				}
				if mode == "content-changed" {
					if err := os.WriteFile(path, []byte("{\"record\":2}\n"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				return snapshotRecord(name, owner, protected)
			}
			sleep := func(d time.Duration) {
				if d <= 0 || d > 250*time.Millisecond {
					t.Fatal("invalid settle interval", d)
				}
				sleeps++
				clock = clock.Add(d)
			}
			err = settleWorkerRecordWith(path, expected, hard, stat, capture, func() time.Time { return clock }, sleep)
			if mode == "local" || mode == "cifs-once" || mode == "cifs-late" || mode == "window-boundary-change" {
				if err != nil || captures != 1 {
					t.Fatalf("settled record refused: %v captures%d", err, captures)
				}
				want := map[string]time.Duration{"local": 2500 * time.Millisecond, "cifs-once": 2750 * time.Millisecond, "cifs-late": 3500 * time.Millisecond, "window-boundary-change": 5 * time.Second}[mode]
				if clock.Sub(start) != want {
					t.Fatalf("stable window not proved: age%v want%v stats%d sleeps%d", clock.Sub(start), want, stats, sleeps)
				}
			} else if err == nil {
				t.Fatal("unstable/changed/expired record admitted")
			}
			if (mode == "keeps-moving" || mode == "stabilizes-too-late") && (clock.Sub(start) != 6*time.Second || captures != 0) {
				t.Fatalf("settle bound violated: %v captures%d", clock.Sub(start), captures)
			}
			if mode == "original-deadline" && (!clock.Equal(hard) || captures != 0) {
				t.Fatal("original hard deadline exceeded")
			}
			if mode == "capture-unstable" && !strings.Contains(err.Error(), "changed during capture") {
				t.Fatal("strict capture refusal lost", err)
			}
		})
	}
}

func TestWorkerLeaseSettleWrites(t *testing.T) {
	if os.Getenv("S0B_SETTLE_WRITER_CHILD") == "1" {
		lock := os.Getenv("S0B_SETTLE_WRITER_LOCK")
		if err := unix.Flock(9, unix.LOCK_EX|unix.LOCK_NB); err != nil {
			t.Fatal(err)
		}
		l := &workerOwnedLease{policy: TrustedPolicy{bootstrap: PolicyBootstrap{LedgerDir: filepath.Dir(lock), WorkerLease: &WorkerLeaseConfig{LockFile: lock}}}, binding: RunBinding{LeaseID: "0123456789abcdef0123456789abcdef", LeaseOwner: "cpu-control", RunID: "settle-writes", HardDeadline: time.Now().Add(20 * time.Second)}, stop: make(chan struct{}), done: make(chan struct{})}
		l.acquired, _ = json.Marshal(l.row("started"))
		check := func(label string, call func() error) {
			start := time.Now()
			if err := call(); err != nil {
				t.Fatal(label, err)
			}
			if time.Since(start) < 2400*time.Millisecond {
				t.Fatal(label, "skipped 2.5s stable window")
			}
		}
		check("acquire", func() error { return l.writeNew("acquire", l.acquired) })
		check("renew", l.renew)
		close(l.done) // This method-level CPU fixture has no background renewal writer.
		check("release", func() error { return l.finish(true) })
		if !l.released {
			t.Fatal("settled release not retained")
		}
		if err := l.writeNew("acquire", l.acquired); !os.IsExist(err) {
			t.Fatal("exclusive acquire replaced", err)
		}
		return
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(t.TempDir(), "canonical.lock")
	cmd := exec.CommandContext(ctx, "bash", "-c", `exec "$1" -test.run '^TestWorkerLeaseSettleWrites$' -test.v 9>"$2"`, "settle-control", exe, lock)
	cmd.Env = append(os.Environ(), "S0B_SETTLE_WRITER_CHILD=1", "S0B_SETTLE_WRITER_LOCK="+lock)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("writer caller control: %v\n%s", err, out)
	}
}
