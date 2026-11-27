//go:build linux

package v3block

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// The unit wrapper, run by a real shell: it prints the HOLDER's pid (the
// child running argv, not the wrapper), and writes the holder's own exit
// status to the exit file once it exits; the holder's output goes to its log.
func TestHolderLaunchCmdKeepsTheHoldersIdentityAndWritesItsStatus(t *testing.T) {
	for _, status := range []string{"75", "0", "78"} {
		dir := t.TempDir()
		logPath := filepath.Join(dir, "a-v1.1.log")
		argv := []string{"sh", "-c", "echo exiting status=99; sleep 0.5; exit " + status, "holder-" + status}
		out, err := exec.Command("sh", "-c", holderLaunchCmd(argv, logPath)).Output()
		if err != nil {
			t.Fatalf("launch: %v", err)
		}
		pid := strings.TrimSpace(string(out))
		cmd, err := os.ReadFile(filepath.Join("/proc", pid, "cmdline"))
		if err != nil || !strings.Contains(string(cmd), "holder-"+status) || strings.Contains(string(cmd), "wait") {
			t.Fatalf("pid %s is not the holder: %q %v", pid, cmd, err)
		}
		var got string
		for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
			if b, err := os.ReadFile(logPath + ".exit"); err == nil {
				got = string(b)
				break
			}
		}
		if f := strings.Fields(got); len(f) != 2 || f[0] != status {
			t.Errorf("exit file %q, want status %s and a time", got, status)
		}
		if b, _ := os.ReadFile(logPath); !strings.Contains(string(b), "status=99") {
			t.Errorf("the holder's output is not in its log: %q", b)
		}
	}
}
