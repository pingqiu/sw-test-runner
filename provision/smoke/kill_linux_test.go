//go:build linux

package smoke

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// When a tool's context ends, its whole process group dies: a grandchild (as
// terraform's provider plugins are) must not outlive the deadline.
func TestNativeKillsTheWholeProcessGroup(t *testing.T) {
	dir := t.TempDir()
	pidfile := filepath.Join(dir, "grandchild.pid")
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _ = Native(ctx, dir, "sh", "-c", "sleep 300 & echo $! > "+pidfile+"; wait")
	if el := time.Since(start); el > 10*time.Second {
		t.Fatalf("Native returned after %v; the context ended at 500 ms", el)
	}
	b, e := os.ReadFile(pidfile)
	if e != nil {
		t.Fatal(e)
	}
	pid, e := strconv.Atoi(strings.TrimSpace(string(b)))
	if e != nil {
		t.Fatal(e)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		alive := syscall.Kill(pid, 0) == nil
		if alive {
			// a zombie still answers kill -0; it is dead if its state is Z
			if s, e := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat"); e == nil && strings.Contains(string(s), ") Z ") {
				alive = false
			}
		}
		if !alive {
			return
		}
		if time.Now().After(deadline) {
			syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("grandchild %d outlived the tool's context", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
