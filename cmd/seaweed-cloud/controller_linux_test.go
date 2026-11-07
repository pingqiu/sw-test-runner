//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// status runs direct-status on dir for run r1 and returns its state and exit code.
func status(t *testing.T, dir string) (string, string, int) {
	t.Helper()
	return statusOf(t, dir, "r1")
}

func statusOf(t *testing.T, dir, run string) (string, string, int) {
	t.Helper()
	var b bytes.Buffer
	rc := directStatus([]string{"--out", dir, "--run", run}, &b)
	var out struct{ State, Code, Reason string }
	if err := json.Unmarshal(b.Bytes(), &out); err != nil {
		t.Fatalf("direct-status printed %q: %v", b.String(), err)
	}
	return out.State, out.Code + out.Reason, rc
}

// record writes a controller record for pid as `direct` does for itself.
func record(t *testing.T, dir string, pid int, edit func(*controllerIdentity)) {
	t.Helper()
	id, err := processIdentity(pid)
	if err != nil {
		t.Fatal(err)
	}
	id.RunID = "r1"
	if edit != nil {
		edit(&id)
	}
	body, _ := json.Marshal(id)
	if err := writeNew(filepath.Join(dir, controllerFile), body); err != nil {
		t.Fatal(err)
	}
}

func start(t *testing.T, name string, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(name, args...)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return cmd
}

// pgrepMatches is the watcher this replaces: any process whose command line
// contains the pattern counts as the controller.
func pgrepMatches(t *testing.T, pattern string) bool {
	t.Helper()
	names, _ := filepath.Glob("/proc/[0-9]*/cmdline")
	for _, name := range names {
		b, _ := os.ReadFile(name)
		if strings.Contains(strings.ReplaceAll(string(b), "\x00", " "), pattern) {
			return true
		}
	}
	return false
}

// The 2026-11-07 soak: the controller exited with FAIL while the launch shell
// that started it (its argv holding "seaweed-cloud direct --out out") lived on,
// so a pgrep watcher reported the run alive for four hours. direct-status
// binds the recorded process, not a command line.
func TestDirectStatusIsNotFooledByASurvivingLaunchShell(t *testing.T) {
	dir := t.TempDir()
	start(t, "sh", "-c", "sleep 60; : nohup setsid ./seaweed-cloud direct --out out")
	controller := start(t, "sleep", "60")
	record(t, dir, controller.Process.Pid, nil)
	if state, why, rc := status(t, dir); state != "RUNNING" || rc != 0 {
		t.Fatalf("live controller: %s %s rc=%d", state, why, rc)
	}
	_ = controller.Process.Kill()
	_ = controller.Wait()
	if !pgrepMatches(t, "seaweed-cloud direct") {
		t.Fatal("fixture: the launch shell must still match the old pgrep watcher")
	}
	if state, why, rc := status(t, dir); state != "GONE_NO_RESULT" || rc != 11 {
		t.Fatalf("controller gone, launch shell alive: %s %s rc=%d, want GONE_NO_RESULT", state, why, rc)
	}
	if err := writeResult(dir, directResult{Schema: "seaweed.direct-run/v1", Code: "FAIL", Error: "soak: load task soak-load-m01 exited 1"}); err != nil {
		t.Fatal(err)
	}
	if state, code, rc := status(t, dir); state != "EXITED" || code != "FAIL" || rc != 10 {
		t.Fatalf("with its result: %s %s rc=%d, want EXITED FAIL", state, code, rc)
	}
}

// A live process under the recorded PID is the controller only if its start
// time, executable and boot all match the record.
func TestDirectStatusRefusesAReusedOrForeignPID(t *testing.T) {
	live := start(t, "sleep", "60")
	for name, edit := range map[string]func(*controllerIdentity){
		"start time": func(id *controllerIdentity) { id.StartTime += "0" },
		"exe":        func(id *controllerIdentity) { id.Exe = "/usr/bin/other" },
		"exe digest": func(id *controllerIdentity) { id.ExeSHA256 = strings.Repeat("0", 64) },
		"boot":       func(id *controllerIdentity) { id.BootID = "00000000-0000-0000-0000-000000000000" },
	} {
		dir := t.TempDir()
		record(t, dir, live.Process.Pid, edit)
		if state, why, rc := status(t, dir); state != "GONE_NO_RESULT" || rc != 11 {
			t.Errorf("%s mismatch: %s %s rc=%d, want GONE_NO_RESULT", name, state, why, rc)
		}
	}
}

// A failing direct run leaves both records, so its watcher sees EXITED FAIL
// from the output directory alone, however stdout was redirected.
func TestDirectLeavesItsIdentityAndResult(t *testing.T) {
	dir := t.TempDir()
	inv := filepath.Join(dir, "inv.json")
	cfg := filepath.Join(dir, "cfg.json")
	os.WriteFile(inv, []byte(`{"target":"onprem","run_id":"r-fail"}`), 0600)
	os.WriteFile(cfg, []byte(`{"ssh_key":"k","host_key":"h","files":{}}`), 0600)
	out := filepath.Join(dir, "out")
	if code, res := directCall(t, "--inventory", inv, "--config", cfg, "--out", out); code != 1 || res.Code != "FAIL" {
		t.Fatalf("exit %d %+v", code, res)
	}
	var id controllerIdentity
	body, err := os.ReadFile(filepath.Join(out, controllerFile))
	if err != nil || json.Unmarshal(body, &id) != nil || id.PID != os.Getpid() || id.RunID != "r-fail" {
		t.Fatalf("controller record %s: %v", body, err)
	}
	if state, code, rc := statusOf(t, out, "r-fail"); state != "EXITED" || code != "FAIL" || rc != 10 {
		t.Fatalf("status %s %s rc=%d, want EXITED FAIL", state, code, rc)
	}
}

// direct's own record is of itself, written once; a missing or partial record
// refuses rather than guessing.
func TestDirectRecordsItselfOnceAndStatusNeedsTheRecord(t *testing.T) {
	dir := t.TempDir()
	if state, _, rc := status(t, dir); state != "INPUT_REFUSED" || rc != 2 {
		t.Fatalf("no record: %s rc=%d", state, rc)
	}
	if err := recordController(dir, "claude-test-run"); err != nil {
		t.Fatal(err)
	}
	if err := recordController(dir, "claude-test-run"); err == nil {
		t.Fatal("a second record replaced the first")
	}
	var id controllerIdentity
	body, _ := os.ReadFile(filepath.Join(dir, controllerFile))
	if err := json.Unmarshal(body, &id); err != nil || id.PID != os.Getpid() || id.RunID != "claude-test-run" || len(id.ExeSHA256) != 64 {
		t.Fatalf("record %s: %v", body, err)
	}
	if state, why, rc := statusOf(t, dir, "claude-test-run"); state != "RUNNING" || rc != 0 {
		t.Fatalf("this test process: %s %s rc=%d", state, why, rc)
	}
	partial := t.TempDir()
	if err := writeNew(filepath.Join(partial, controllerFile), []byte(`{"schema":"seaweed.direct-controller/v1","pid":1}`)); err != nil {
		t.Fatal(err)
	}
	if state, _, rc := status(t, partial); state != "INPUT_REFUSED" || rc != 2 {
		t.Fatalf("partial record: %s rc=%d", state, rc)
	}
}

// codex01's negatives on 42d315e: every record that is not exactly what
// direct writes, and every inspection error other than "absent", refuses;
// nothing reads as RUNNING or as a PASS that was not recorded.
func TestDirectStatusRefusesAnythingButExactRecords(t *testing.T) {
	live := start(t, "sleep", "60")
	pid := live.Process.Pid
	for _, c := range []struct {
		name   string
		run    string
		edit   func(*controllerIdentity)
		result string // written verbatim as result.json; "dir" makes it a directory
	}{
		{"empty run_id", "r1", func(id *controllerIdentity) { id.RunID = "" }, ""},
		{"another run", "r2", nil, ""},
		{"result wrong schema", "r1", nil, `{"schema":"seaweed.other/v1","code":"PASS"}`},
		{"result missing schema", "r1", nil, `{"code":"PASS"}`},
		{"result duplicate code FAIL then PASS", "r1", nil, `{"schema":"seaweed.direct-run/v1","code":"FAIL","code":"PASS"}`},
		{"result unknown code", "r1", nil, `{"schema":"seaweed.direct-run/v1","code":"BANANA"}`},
		{"result not a file", "r1", nil, "dir"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			record(t, dir, pid, c.edit)
			switch c.result {
			case "":
			case "dir":
				if err := os.Mkdir(filepath.Join(dir, resultFile), 0o700); err != nil {
					t.Fatal(err)
				}
			default:
				if err := writeNew(filepath.Join(dir, resultFile), []byte(c.result)); err != nil {
					t.Fatal(err)
				}
			}
			if state, why, rc := statusOf(t, dir, c.run); state != "INPUT_REFUSED" || rc != 2 {
				t.Fatalf("%s rc=%d (%s), want INPUT_REFUSED", state, rc, why)
			}
		})
	}
	dir := t.TempDir()
	if err := writeNew(filepath.Join(dir, controllerFile), []byte(`{"schema":"seaweed.direct-controller/v1","run_id":"r1","pid":1,"starttime":"1","boot_id":"b","exe":"/x","exe_sha256":"`+strings.Repeat("0", 64)+`","extra":1}`)); err != nil {
		t.Fatal(err)
	}
	if state, _, rc := status(t, dir); state != "INPUT_REFUSED" || rc != 2 {
		t.Fatalf("controller record with an unknown field: %s rc=%d", state, rc)
	}
}
