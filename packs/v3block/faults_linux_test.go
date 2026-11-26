//go:build linux

package v3block

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The generated kill, run by a real shell against controlled /proc contents.
// The pid is one no process can hold, so a signal that goes out fails (exit 4)
// and nothing real is touched.
func TestKillCmdSignalsOnlyTheRecordedIdentity(t *testing.T) {
	const pid = "999999999"
	proc := t.TempDir()
	dir := filepath.Join(proc, pid)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "stat"), []byte(statLine(pid, "a (b) c", "7", "555")), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "cmdline"), []byte("/bin/x\x00--a\x00"), 0o644)
	run := func(p, start string) (string, int) {
		out, err := exec.Command("sh", "-c", killCmd(proc, p, start)).Output()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
		return strings.TrimSpace(string(out)), code
	}
	// Another start time: another process, nothing sent.
	if out, code := run(pid, "554"); out != "gone" || code != 0 {
		t.Errorf("recycled pid: %q exit %d", out, code)
	}
	// No such pid.
	if out, code := run("999999998", "555"); out != "gone" || code != 0 {
		t.Errorf("absent pid: %q exit %d", out, code)
	}
	// The recorded identity: the signal is attempted (and fails for this pid).
	if out, code := run(pid, "555"); code != 4 {
		t.Errorf("recorded identity: %q exit %d, want the kill attempted (exit 4)", out, code)
	}
	// A short stat is unreadable: nothing sent.
	_ = os.WriteFile(filepath.Join(dir, "stat"), []byte(pid+" (x) S 7\n"), 0o644)
	if out, code := run(pid, "555"); code != 3 {
		t.Errorf("short stat: %q exit %d", out, code)
	}
}
