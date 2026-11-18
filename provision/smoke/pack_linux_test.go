//go:build linux

package smoke

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// execFixture pins a shell script as the run's exec pack.
func execFixture(t *testing.T, script string) Config {
	t.Helper()
	c := fixture(t)
	exe := filepath.Join(t.TempDir(), "pack.sh")
	if e := os.WriteFile(exe, []byte("#!/bin/sh\n"+script), 0700); e != nil {
		t.Fatal(e)
	}
	sum, e := fileSHA256(exe)
	if e != nil {
		t.Fatal(e)
	}
	c.PackName, c.PackExec = "exec", &ExecPack{Path: exe, SHA256: sum, Args: []string{"--mode", "density"}}
	return c
}

// The exec pack gets the run's exact inventory, its keys and its own output
// dir through $SEAWEED_CLOUD_PACK_CONTEXT; exit 0 passes and destroy follows.
func TestExecPackGetsInventoryAndPasses(t *testing.T) {
	c := execFixture(t, `ctx="$SEAWEED_CLOUD_PACK_CONTEXT"; test -s "$ctx" || exit 9
echo "args:$*"; cat "$ctx"
inv=$(sed -n 's/.*"inventory":"\([^"]*\)".*/\1/p' "$ctx"); test -s "$inv" || exit 8
cp "$inv" got-inventory.json
exit 0
`)
	f := &fake{}
	ctl := controller(f)
	ctl.Pack = nil // the configured exec pack, not the test default
	var inv []byte
	inner := ctl.Command
	ctl.Command = func(ctx context.Context, dir, tool string, args ...string) ([]byte, error) {
		b, e := inner(ctx, dir, tool, args...)
		if tool == "terraform" && args[0] == "output" && len(args) > 2 && args[2] == "inventory_json" {
			inv = b
		}
		return b, e
	}
	r := ctl.Execute(context.Background(), "run", c)
	if r.Code != "COMPLETE" {
		t.Fatalf("code %s (%s), want COMPLETE", r.Code, r.Error)
	}
	got, e := os.ReadFile(filepath.Join(c.Output, "pack", "got-inventory.json"))
	if e != nil || string(got) != string(inv) || len(inv) == 0 {
		t.Fatalf("pack inventory %q (err %v) != exported %q", got, e, inv)
	}
	out, _ := os.ReadFile(filepath.Join(c.Output, "pack", "stdout.txt"))
	if !strings.Contains(string(out), "args:--mode density") {
		t.Fatalf("pack args not passed: %s", out)
	}
	var pc map[string]any
	b, _ := os.ReadFile(filepath.Join(c.Output, "pack", "context.json"))
	if json.Unmarshal(b, &pc) != nil || pc["schema"] != "seaweed.cloud-pack-context/v1" || pc["ssh_key"] != c.Cloud.SSHKey || pc["host_key"] == "" {
		t.Fatalf("context.json: %s", b)
	}
	if !strings.Contains(strings.Join(f.events, ","), "terraform destroy") {
		t.Fatalf("no destroy after the pack: %v", f.events)
	}
}

// A failing exec pack is PACK_FAILED and destroy still runs.
func TestExecPackFailureStillDestroys(t *testing.T) {
	c := execFixture(t, "echo boom >&2; exit 3\n")
	f := &fake{}
	ctl := controller(f)
	ctl.Pack = nil
	r := ctl.Execute(context.Background(), "run", c)
	if r.Code != "PACK_FAILED" || !strings.Contains(r.Error, "exit status 3") {
		t.Fatalf("code %s error %q, want PACK_FAILED (exit status 3)", r.Code, r.Error)
	}
	if !strings.Contains(strings.Join(f.events, ","), "terraform destroy") {
		t.Fatalf("no destroy after a failed pack: %v", f.events)
	}
}

// A pack that outlives its work context is killed with its whole process group.
func TestExecPackKilledAtWorkDeadline(t *testing.T) {
	pidfile := filepath.Join(t.TempDir(), "child.pid")
	c := execFixture(t, "sleep 300 & echo $! > "+pidfile+"; wait\n")
	exe := c.PackExec
	c.Cloud.Output = t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _ = execPack(*exe)(ctx, []byte(`{}`), c.Cloud)
	if el := time.Since(start); el > 10*time.Second {
		t.Fatalf("pack ran %v past a 700 ms context", el)
	}
	b, e := os.ReadFile(pidfile)
	if e != nil {
		t.Skipf("pack did not start (%v)", e)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	for i := 0; i < 100; i++ {
		if syscall.Kill(pid, 0) != nil {
			return
		}
		if s, e := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat"); e == nil && strings.Contains(string(s), ") Z ") {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	syscall.Kill(pid, syscall.SIGKILL)
	t.Fatalf("the pack's child %d outlived the work context", pid)
}

// The pinned executable is rechecked just before it runs.
func TestExecPackRefusesChangedExecutable(t *testing.T) {
	c := execFixture(t, "exit 0\n")
	c.Cloud.Output = t.TempDir()
	os.WriteFile(c.PackExec.Path, []byte("#!/bin/sh\nexit 0 # changed\n"), 0700)
	if _, err := execPack(*c.PackExec)(context.Background(), []byte(`{}`), c.Cloud); err == nil || !strings.Contains(err.Error(), "changed since admission") {
		t.Fatalf("changed executable ran: %v", err)
	}
}

// childDead waits (bounded) until pid is gone or a zombie.
func childDead(t *testing.T, pid int) bool {
	t.Helper()
	for i := 0; i < 100; i++ {
		if syscall.Kill(pid, 0) != nil {
			return true
		}
		if s, e := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat"); e == nil && strings.Contains(string(s), ") Z ") {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	syscall.Kill(pid, syscall.SIGKILL)
	return false
}

// readPid reads the child pid a script wrote.
func readPid(t *testing.T, path string) int {
	t.Helper()
	b, e := os.ReadFile(path)
	if e != nil {
		t.Fatalf("the script did not record its child: %v", e)
	}
	pid, e := strconv.Atoi(strings.TrimSpace(string(b)))
	if e != nil {
		t.Fatal(e)
	}
	return pid
}

// The leader exits first, leaving a child behind: the pack's group is retired on
// a normal and on a failed exit, so no child outlives the pack (codex01 P1:
// CommandContext stops watching once the leader is reaped).
func TestExecPackLeaderExitsFirstRetiresChildren(t *testing.T) {
	for _, tc := range []struct {
		name, exit, want string
	}{{"exit 0", "0", "COMPLETE"}, {"exit 4", "4", "PACK_FAILED"}} {
		t.Run(tc.name, func(t *testing.T) {
			pidfile := filepath.Join(t.TempDir(), "child.pid")
			c := execFixture(t, "sleep 600 >/dev/null 2>&1 & echo $! > "+pidfile+"; exit "+tc.exit+"\n")
			f := &fake{}
			ctl := controller(f)
			ctl.Pack = nil
			start := time.Now()
			r := ctl.Execute(context.Background(), "run", c)
			if r.Code != tc.want {
				t.Fatalf("code %s (%s), want %s", r.Code, r.Error, tc.want)
			}
			if el := time.Since(start); el > 30*time.Second {
				t.Fatalf("the run took %v", el)
			}
			if pid := readPid(t, pidfile); !childDead(t, pid) {
				t.Fatalf("the pack's child %d outlived the pack (leader exited %s)", pid, tc.exit)
			}
		})
	}
}

// Native (terraform/az) gets the same retirement: a tool that exits leaving a
// child behind must not leave it running.
func TestNativeRetiresGroupAfterLeaderExit(t *testing.T) {
	for _, exit := range []string{"0", "5"} {
		pidfile := filepath.Join(t.TempDir(), "child.pid")
		_, _ = Native(context.Background(), t.TempDir(), "sh", "-c", "sleep 600 >/dev/null 2>&1 & echo $! > "+pidfile+"; exit "+exit)
		if pid := readPid(t, pidfile); !childDead(t, pid) {
			t.Fatalf("a child of the tool outlived it (leader exit %s)", exit)
		}
	}
}

// A pinned file that cannot be executed is refused at admission, before any call.
func TestExecPackNotExecutableRefused(t *testing.T) {
	c := execFixture(t, "exit 0\n")
	if e := os.Chmod(c.PackExec.Path, 0o600); e != nil {
		t.Fatal(e)
	}
	calls := 0
	r := (Controller{Command: func(context.Context, string, string, ...string) ([]byte, error) { calls++; return nil, nil }}).Execute(context.Background(), "run", c)
	if r.Code != "INPUT_REFUSED" || !strings.Contains(r.Error, "not executable") || calls != 0 {
		t.Fatalf("code %s error %q calls %d, want INPUT_REFUSED (not executable) with 0 calls", r.Code, r.Error, calls)
	}
}

// An earlier run whose markers cannot be observed (permission denied) blocks a
// new run as unknown, never counts as absent.
func TestUnobservableSiblingBlocksNewRun(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads mode-000 directories")
	}
	c := fixture(t)
	d := filepath.Join(filepath.Dir(c.Output), "earlier-run")
	os.MkdirAll(d, 0700)
	os.WriteFile(filepath.Join(d, "launch.json"), []byte(`{}`), 0600)
	os.Chmod(d, 0)
	defer os.Chmod(d, 0700)
	calls := 0
	r := (Controller{Command: func(context.Context, string, string, ...string) ([]byte, error) { calls++; return nil, nil }}).Execute(context.Background(), "run", c)
	if r.Code != "RECOVERY_REQUIRED" || calls != 0 || !strings.Contains(r.Error, "earlier-run") {
		t.Fatalf("code %s error %q calls %d, want RECOVERY_REQUIRED naming earlier-run", r.Code, r.Error, calls)
	}
}
