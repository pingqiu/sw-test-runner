package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pingqiu/sw-test-runner/packs/seaweed"
	"github.com/pingqiu/sw-test-runner/provision/smoke"
)

// The child uses Controller.Execute with an injected pack and Native processes;
// recovery uses the actual CLI. Terraform/az are local PATH fixtures, no account/SSH.
func TestKilledControllerSavedStateCleanup(t *testing.T) {
	if os.Getenv("CLOUD_KILL_CHILD") == "1" {
		b, e := os.ReadFile(os.Getenv("CLOUD_CONFIG"))
		var c smoke.Config
		if e != nil || smoke.StrictJSON(b, &c) != nil {
			os.Exit(2)
		}
		r := (smoke.Controller{Command: smoke.Native, Pack: func(context.Context, []byte, seaweed.CloudConfig) (string, error) { return "", nil }}).Execute(context.Background(), "run", c)
		_ = json.NewEncoder(os.Stdout).Encode(r)
		os.Exit(0)
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	bin := filepath.Join(home, "bin")
	if e := os.Mkdir(bin, 0700); e != nil {
		t.Fatal(e)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	t.Setenv("FAKE_CLOUD_HOME", home)
	write := func(p, s string, mode os.FileMode) {
		t.Helper()
		if e := os.WriteFile(p, []byte(s), mode); e != nil {
			t.Fatal(e)
		}
	}
	write(filepath.Join(bin, "terraform"), `#!/bin/sh
case "$1" in
 apply) echo live > "$FAKE_CLOUD_HOME/live" ;;
 output) echo ready > "$FAKE_CLOUD_HOME/ready"; sleep 3 ;;
 destroy) rm -f "$FAKE_CLOUD_HOME/live" ;;
esac
`, 0700)
	write(filepath.Join(bin, "az"), `#!/bin/sh
if test -e "$FAKE_CLOUD_HOME/live"; then printf '["/subscriptions/test/live-vm"]'; else printf '[]'; fi
`, 0700)
	source, e := filepath.Abs("../..")
	if e != nil {
		t.Fatal(e)
	}
	_, pin, e := smoke.Sources(source)
	if e != nil {
		t.Fatal(e)
	}
	root := filepath.Join(home, ".local/state/seaweed-cloud/runs")
	if e = os.MkdirAll(root, 0700); e != nil {
		t.Fatal(e)
	}
	hard := time.Now().UTC().Add(50 * time.Minute).Truncate(time.Minute)
	vars, _ := json.Marshal(map[string]any{"cluster": true, "subscription_id": "11111111-1111-1111-1111-111111111111", "resource_group_name": "test", "location": "westus2", "vm_size": "Standard_D2s_v5", "run_declaration": map[string]any{"work_deadline": hard.Add(-20 * time.Minute), "hard_deadline": hard, "cloud_cleanup_reserve_seconds": 1200, "budget_usd": 3, "abort_estimate_usd": 2.5}})
	vp := filepath.Join(home, "vars.json")
	write(vp, string(vars), 0600)
	cfg := smoke.Config{Source: source, ModuleSHA256: pin, Variables: vp, VariablesSHA256: fmt.Sprintf("%x", sha256.Sum256(vars)), Output: filepath.Join(root, "kill-control"), EstimatedUSD: .12, Cloud: seaweed.CloudConfig{SSHKey: filepath.Join(home, "client-key"), Files: map[string]seaweed.CloudFile{}}}
	for _, name := range []string{"weed", "weed-volume", "native.tar"} {
		p := filepath.Join(home, name)
		write(p, name, 0600)
		cfg.Cloud.Files[name] = seaweed.CloudFile{Path: p, SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(name)))}
	}
	cb, _ := json.Marshal(cfg)
	cp := filepath.Join(home, "config.json")
	write(cp, string(cb), 0600)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestKilledControllerSavedStateCleanup$")
	child.Env = append(os.Environ(), "CLOUD_KILL_CHILD=1", "CLOUD_CONFIG="+cp)
	var logs bytes.Buffer
	child.Stdout = &logs
	child.Stderr = &logs
	if e = child.Start(); e != nil {
		t.Fatal(e)
	}
	defer func() { _ = child.Process.Kill() }()
	for {
		if _, e = os.Stat(filepath.Join(home, "ready")); e == nil {
			break
		}
		if ctx.Err() != nil {
			t.Fatal("child did not reach settled apply", logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if e = child.Process.Kill(); e != nil {
		t.Fatal(e)
	}
	_ = child.Wait()
	// No finally ran: the simulated billable VM is still present.
	if _, e = os.Stat(filepath.Join(home, "live")); e != nil {
		t.Fatal("kill fixture did not retain resource")
	}
	write(cp, "original config unavailable", 0600)
	write(vp, "original vars unavailable", 0600)
	var output bytes.Buffer
	if run([]string{"cleanup", "--run", "kill-control", "--dry-run"}, &output) != 0 {
		t.Fatal(output.String())
	}
	if _, e = os.Stat(filepath.Join(home, "live")); e != nil {
		t.Fatal("dry-run performed cleanup")
	}
	output.Reset()
	if rc := run([]string{"cleanup", "--run", "kill-control", "--json"}, &output); rc != 0 {
		t.Fatalf("cleanup rc=%d %s", rc, output.String())
	}
	var r smoke.Result
	if json.Unmarshal(output.Bytes(), &r) != nil || r.Code != "COMPLETE" || !r.RemainingObserved || !strings.Contains(r.RemainingSummary, "remaining: 0 (checked") {
		t.Fatal(output.String())
	}
	if _, e = os.Stat(filepath.Join(home, "live")); !os.IsNotExist(e) {
		t.Fatal("billable resource survived cleanup")
	}
	output.Reset()
	if run([]string{"cleanup", "--run", "missing"}, &output) == 0 {
		t.Fatal("missing saved state accepted")
	}
	output.Reset()
	if run([]string{"cleanup", "--run", "../kill-control"}, &output) == 0 {
		t.Fatal("unsafe run ID accepted")
	}
}
