//go:build linux

package v3block

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Controls of scenarios/v3block/m0-own.sh, the M0 run's ownership: a run that
// did not acquire its work directory changes nothing; teardown deletes only
// its own NQN prefix's controllers (a fake sysfs here); a recorded pid whose
// start time no longer matches is another process and is left alone.
// Real bash, /proc and sleep processes: Linux only (the runner CI).

const ownScript = "../../scenarios/v3block/m0-own.sh"

func needLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("m0-own.sh needs bash and /proc")
	}
}

func own(t *testing.T, env []string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command("bash", append([]string{ownScript}, args...)...)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return string(out), code
}

// fakeController makes a fake sysfs controller with a subsystem NQN and a
// delete_controller file that records the write.
func fakeController(t *testing.T, sys, name, nqn string) string {
	t.Helper()
	d := filepath.Join(sys, name)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "subsysnqn"), []byte(nqn+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	del := filepath.Join(d, "delete_controller")
	if err := os.WriteFile(del, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return del
}

func deleted(t *testing.T, del string) bool {
	b, _ := os.ReadFile(del)
	return strings.TrimSpace(string(b)) == "1"
}

// sleeper starts a process the test owns and returns its pid.
func sleeper(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
	go func() { _, _ = cmd.Process.Wait() }()
	return cmd.Process.Pid
}

func running(pid int) bool { return syscall.Kill(pid, 0) == nil }

func startOf(t *testing.T, pid int) string {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	return strings.Fields(s[strings.LastIndex(s, ")")+2:])[19]
}

func TestOwnClaimRefusesAnExistingWorkAndItsTeardownTouchesNothing(t *testing.T) {
	needLinux(t)
	dir := t.TempDir()
	work, sys := filepath.Join(dir, "work"), filepath.Join(dir, "sys")
	if err := os.Mkdir(work, 0o755); err != nil { // another run's directory
		t.Fatal(err)
	}
	pid := sleeper(t)
	_ = os.WriteFile(filepath.Join(work, "owned"), []byte(strconv.Itoa(pid)+" "+startOf(t, pid)+" theirs\n"), 0o644)
	del := fakeController(t, sys, "nvme0", "nqn.2026-11.io.seaweedfs:op-run-1-v1")
	out, code := own(t, nil, "claim", work)
	if code == 0 || strings.TrimSpace(out) == "" {
		t.Fatalf("claim of an existing directory: exit %d %q", code, out)
	}
	// The claim failed, so this run has no token: its teardown changes nothing.
	out, code = own(t, []string{"NVME_SYSFS=" + sys}, "teardown", work, "", "nqn.2026-11.io.seaweedfs:op-run-1-")
	if code != 0 || !strings.Contains(out, "nothing touched") {
		t.Fatalf("teardown without a token: exit %d %q", code, out)
	}
	time.Sleep(200 * time.Millisecond)
	if !running(pid) || deleted(t, del) {
		t.Fatal("a run that acquired nothing stopped a process or deleted a controller")
	}
	// A token that is not this directory's is no better.
	out, _ = own(t, []string{"NVME_SYSFS=" + sys}, "teardown", work, strings.Repeat("a", 32), "nqn.2026-11.io.seaweedfs:op-run-1-")
	if !strings.Contains(out, "nothing touched") || !running(pid) || deleted(t, del) {
		t.Fatalf("a foreign token touched something: %q", out)
	}
}

func TestOwnTeardownDeletesOnlyItsOwnPrefixAndStopsOnlyItsOwnProcesses(t *testing.T) {
	needLinux(t)
	dir := t.TempDir()
	work, sys := filepath.Join(dir, "work"), filepath.Join(dir, "sys")
	tok, code := own(t, nil, "claim", work)
	tok = strings.TrimSpace(tok)
	if code != 0 || len(tok) != 32 {
		t.Fatalf("claim: %d %q", code, tok)
	}
	ours := fakeController(t, sys, "nvme0", "nqn.2026-11.io.seaweedfs:op-run-1-v1")
	theirs := fakeController(t, sys, "nvme1", "nqn.2026-11.io.seaweedfs:op-run-2-v1") // the same volume name, another run
	mine := sleeper(t)
	if out, code := own(t, nil, "record", work, tok, "mine", strconv.Itoa(mine)); code != 0 {
		t.Fatalf("record: %q", out)
	}
	// A recycled pid: a live process recorded with another start time.
	recycled := sleeper(t)
	f, _ := os.OpenFile(filepath.Join(work, "owned"), os.O_APPEND|os.O_WRONLY, 0o644)
	_, _ = f.WriteString(strconv.Itoa(recycled) + " 1 an-earlier-process\n")
	_ = f.Close()
	// The controller of our prefix disappears once deleted, as the kernel does.
	go func() {
		for i := 0; i < 50 && !deleted(t, ours); i++ {
			time.Sleep(100 * time.Millisecond)
		}
		_ = os.RemoveAll(filepath.Dir(ours))
	}()
	out, code := own(t, []string{"NVME_SYSFS=" + sys}, "teardown", work, tok, "nqn.2026-11.io.seaweedfs:op-run-1-")
	if code != 0 {
		t.Fatalf("teardown: exit %d %q", code, out)
	}
	if deleted(t, theirs) {
		t.Error("another run's controller (same volume name) was deleted")
	}
	time.Sleep(300 * time.Millisecond)
	if running(mine) {
		t.Error("this run's own process still runs")
	}
	if !running(recycled) || !strings.Contains(out, "recycled: untouched") {
		t.Errorf("a recorded pid with another start time was stopped: %q", out)
	}
}

func TestOwnTeardownRefusesAPrefixThatIsNotRunScoped(t *testing.T) {
	needLinux(t)
	dir := t.TempDir()
	work := filepath.Join(dir, "work")
	tok, _ := own(t, nil, "claim", work)
	if out, code := own(t, []string{"NVME_SYSFS=" + filepath.Join(dir, "sys")}, "teardown", work, strings.TrimSpace(tok), "nqn.2026-11.io.seaweedfs:op"); code == 0 {
		t.Fatalf("a prefix without the run's trailing separator was accepted: %q", out)
	}
}
