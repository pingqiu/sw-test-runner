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
