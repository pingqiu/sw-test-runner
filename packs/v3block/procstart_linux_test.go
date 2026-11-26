//go:build linux

package v3block

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// statLine is a /proc/PID/stat line with the given command name, parent pid
// and start time (field 22); the other fields are distinct filler.
func statLine(pid, comm, ppid, start string) string {
	f := []string{pid, "(" + comm + ")", "S", ppid}
	for i := 5; i <= 21; i++ {
		f = append(f, "9"+strings.Repeat("1", i%4))
	}
	f = append(f, start, "4096", "100")
	return strings.Join(f, " ") + "\n"
}

// The generated extraction, run by a real shell against controlled stat
// contents: it reads field 22, not the parent pid, also for a name with
// spaces and parentheses and for two processes under one parent.
func TestProcStartCmdReadsTheStartTimeField(t *testing.T) {
	proc := t.TempDir()
	write := func(pid, line string) {
		if err := os.MkdirAll(filepath.Join(proc, pid), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(proc, pid, "stat"), []byte(line), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("101", statLine("101", "blockvolume", "7", "555"))
	write("102", statLine("102", "a (b) c", "7", "777")) // same parent, another start
	run := func(pid string) (string, int) {
		out, err := exec.Command("sh", "-c", procStartCmd(proc, pid)).Output()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
		return strings.TrimSpace(string(out)), code
	}
	for pid, want := range map[string]string{"101": "start 555", "102": "start 777", "999": "absent"} {
		if got, code := run(pid); got != want || code != 0 {
			t.Errorf("pid %s: %q exit %d, want %q", pid, got, code, want)
		}
	}
	// A stat that exists but cannot be read, or is short, is a failure, not a start time.
	write("103", "103 (short) S 7\n")
	if got, code := run("103"); code == 0 {
		t.Errorf("a short stat gave %q", got)
	}
	if os.Getuid() != 0 {
		write("104", statLine("104", "x", "7", "1"))
		_ = os.Chmod(filepath.Join(proc, "104", "stat"), 0)
		if got, code := run("104"); code == 0 {
			t.Errorf("an unreadable stat gave %q", got)
		}
	}
}
