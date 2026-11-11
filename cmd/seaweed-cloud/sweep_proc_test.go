package main

import (
	"os"
	"path/filepath"
	"testing"
)

// A fake /proc tree: a PID whose cmdline cannot be read but whose directory is still there is not proof that no
// controller exists; only a PID that is gone is (codex01 r2 R2). The real-/proc positive test is sweep_linux_test.go.
func TestLiveControllerFailsClosedOnAnUnreadablePresentPID(t *testing.T) {
	root := t.TempDir()
	write := func(pid, cmdline string) {
		if err := os.MkdirAll(filepath.Join(root, pid), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, pid, "cmdline"), []byte(cmdline), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("100", "/usr/bin/bash\x00-l\x00")
	write("101", "/x/seaweed-cloud\x00sweep\x00--receipts\x00/r/quiet-run\x00")
	if liveControllerAt(root, "quiet-run") {
		t.Fatal("a readable tree with no controller was reported live")
	}
	write("102", "/x/seaweed-cloud\x00run\x00--config\x00/s/runs/quiet-run/config.json\x00")
	if !liveControllerAt(root, "quiet-run") {
		t.Fatal("a readable controller naming the run was not seen")
	}
	// A present PID whose cmdline read errors (here: cmdline is a directory).
	other := t.TempDir()
	if err := os.MkdirAll(filepath.Join(other, "999999", "cmdline"), 0700); err != nil {
		t.Fatal(err)
	}
	if !liveControllerAt(other, "possibly-active-run") {
		t.Fatal("an unreadable cmdline of a still-present PID was treated as no live controller")
	}
	if !liveControllerAt(filepath.Join(other, "no-such-proc"), "any-run") {
		t.Fatal("an unreadable /proc must answer live")
	}
}
