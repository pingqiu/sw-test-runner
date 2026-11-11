//go:build linux

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// A seaweed-cloud process whose command line names the run is live; an absent run is not; the sweep itself never
// counts as a controller. The processes are /bin/sh with argv[0] .../seaweed-cloud, running a script named by
// argv[1] that only sleeps, so /proc shows exactly the command line a controller or a sweep would have.
func TestLiveControllerReadsProc(t *testing.T) {
	if liveController("no-such-run-anywhere-1f3a") {
		t.Fatal("an absent run was reported live")
	}
	dir := t.TempDir()
	for _, name := range []string{"run", "sweep"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("sleep 30; true\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	start := func(verb, run string) *exec.Cmd {
		cmd := &exec.Cmd{Path: "/bin/sh", Args: []string{filepath.Join(dir, "seaweed-cloud"), verb, run}, Dir: dir}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
		return cmd
	}
	start("run", "azms-live-run-7c2e")
	start("sweep", "azms-sweep-run-9d1b")
	time.Sleep(300 * time.Millisecond)
	if !liveController("azms-live-run-7c2e") {
		t.Fatal("a running controller naming the run was not seen")
	}
	if liveController("azms-sweep-run-9d1b") {
		t.Fatal("a sweep was counted as the run's controller")
	}
}
