package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

const (
	tRG  = "rg-claude04-standing-20261120"
	tBox = "c04-standing"
	tSub = "11111111-2222-3333-4444-555555555555"
	tTok = "0123456789abcdef01234567"
)

type toolCall struct {
	dir, tool string
	args      []string
}

// fakeStanding replaces the external command boundary; failApply makes terraform apply fail.
type fakeStanding struct {
	calls     []toolCall
	failApply bool
	rgExists  string
}

func (f *fakeStanding) native(_ context.Context, dir, tool string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, toolCall{dir, tool, append([]string(nil), args...)})
	j := strings.Join(args, " ")
	switch {
	case tool == "terraform" && args[0] == "apply" && f.failApply:
		return []byte("Error: timeout while waiting for create"), errors.New("exit 1")
	case tool == "terraform" && args[0] == "show":
		return []byte(`{"resource_changes":[{"address":"azurerm_linux_virtual_machine.box","change":{"actions":["create"]}}]}`), nil
	case tool == "az" && strings.HasPrefix(j, "group exists"):
		return []byte(f.rgExists), nil
	case tool == "az" && strings.HasPrefix(j, "resource list"):
		return []byte("[]"), nil
	case tool == "az" && strings.Contains(j, "get-instance-view"):
		return []byte(`{"instanceView":{"statuses":[{"code":"PowerState/deallocated"}]}}`), nil
	}
	return nil, nil
}

func setupStanding(t *testing.T) (*fakeStanding, string) {
	t.Helper()
	f := &fakeStanding{rgExists: "false"}
	home := t.TempDir()
	oldTool, oldOS, oldHome := standingTool, standingGOOS, standingHome
	standingTool, standingGOOS, standingHome = f.native, "linux", func() (string, error) { return home, nil }
	t.Cleanup(func() { standingTool, standingGOOS, standingHome = oldTool, oldOS, oldHome })
	vars := filepath.Join(t.TempDir(), "box.json")
	if err := os.WriteFile(vars, []byte(`{"subscription_id":"`+tSub+`","resource_group_name":"`+tRG+`","name":"`+tBox+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return f, vars
}

func standingRun(t *testing.T, args ...string) standingResult {
	t.Helper()
	var out bytes.Buffer
	run(append([]string{"standing"}, args...), &out)
	var r standingResult
	if err := json.Unmarshal(out.Bytes(), &r); err != nil {
		t.Fatalf("%v: %v (%s)", args, err, out.String())
	}
	return r
}

func target(rg string) []string { return []string{"--name", tBox, "--resource-group", rg} }

func TestClassifyPowerBillingOnlyWhenNotDeallocated(t *testing.T) {
	view := func(code string) []byte {
		return []byte(`{"instanceView":{"statuses":[{"code":"ProvisioningState/succeeded"},{"code":"` + code + `"}]}}`)
	}
	for _, c := range []struct{ code, state, billing string }{
		{"PowerState/deallocated", "deallocated", "no"},
		{"PowerState/deallocating", "deallocating", "no"},
		{"PowerState/running", "running", "yes"},
		{"PowerState/stopped", "stopped", "yes"}, // OS shutdown: allocated, compute still billing
		{"PowerState/weird", "weird", "UNKNOWN"},
	} {
		if s, b := classifyPower(view(c.code)); s != c.state || b != c.billing {
			t.Fatalf("%s: got %s/%s, want %s/%s", c.code, s, b, c.state, c.billing)
		}
	}
	if s, b := classifyPower([]byte(`{"instanceView":{"statuses":[]}}`)); s != "UNKNOWN" || b != "UNKNOWN" {
		t.Fatalf("no power status: got %s/%s", s, b)
	}
}

// Every refusal happens before the command boundary: zero tool calls.
func TestStandingRefusalsMakeNoToolCall(t *testing.T) {
	f, vars := setupStanding(t)
	other := filepath.Join(t.TempDir(), "other.json")
	_ = os.WriteFile(other, []byte(`{"subscription_id":"`+tSub+`","resource_group_name":"rg-claude04-standing-20261121","name":"`+tBox+`"}`), 0o600)
	for _, c := range []struct {
		args []string
		code string
	}{
		{nil, "USAGE"},
		{append([]string{"reboot"}, target(tRG)...), "USAGE"},
		{append([]string{"status"}, target("rg-codex02-azure-test-20261029")...), "USAGE"},
		{append([]string{"plan"}, target(tRG)...), "USAGE"},
		{append([]string{"apply"}, target(tRG)...), "REFUSED"},
		{append(append([]string{"destroy"}, target(tRG)...), "--launch-token", "xyz"), "REFUSED"},
		{append([]string{"status"}, target(tRG)...), "REFUSED"},                                         // no pinned identity yet
		{append(append([]string{"plan"}, target(tRG)...), "--module", "x", "--vars", other), "REFUSED"}, // vars name another RG
	} {
		if r := standingRun(t, c.args...); r.Code != c.code {
			t.Fatalf("%v: code %s (%s), want %s", c.args, r.Code, r.Reason, c.code)
		}
	}
	if len(f.calls) != 0 {
		t.Fatalf("refusals reached the command boundary: %+v", f.calls)
	}
	_ = vars
}

func TestStandingLifecycleArgvAndPinnedScope(t *testing.T) {
	f, vars := setupStanding(t)
	module := filepath.Join("..", "..", "provision", "standing")
	if r := standingRun(t, append(append([]string{"plan"}, target(tRG)...), "--module", module, "--vars", vars)...); r.Code != "OK" {
		t.Fatalf("plan: %+v", r)
	}
	home, _ := standingHome()
	work := filepath.Join(home, ".local", "state", "seaweed-cloud", "standing", tBox)
	pinned := "-var-file=" + filepath.Join(work, standingVarsFile)
	want := [][]string{
		{"init", "-input=false", "-no-color", "-backend=false"},
		{"plan", "-input=false", "-no-color", pinned, "-out=standing.tfplan"},
		{"show", "-json", "standing.tfplan"},
	}
	for i, w := range want {
		if strings.Join(f.calls[i].args, " ") != strings.Join(w, " ") || f.calls[i].dir != work {
			t.Fatalf("plan call %d: %+v, want %v in %s", i, f.calls[i], w, work)
		}
	}
	if b, err := os.ReadFile(filepath.Join(work, "main.tf")); err != nil || !bytes.Contains(b, []byte("features {}")) {
		t.Fatalf("module not copied with its provider block: %v", err)
	}
	// a different RG for the pinned box is refused before any tool call
	n := len(f.calls)
	if r := standingRun(t, append([]string{"status"}, target("rg-claude04-standing-20261121")...)...); r.Code != "REFUSED" || len(f.calls) != n {
		t.Fatalf("wrong-RG status: %+v calls %d->%d", r, n, len(f.calls))
	}
	if r := standingRun(t, append(append([]string{"apply"}, target(tRG)...), "--launch-token", tTok)...); r.Code != "OK" {
		t.Fatalf("apply: %+v", r)
	}
	if got := strings.Join(f.calls[n].args, " "); got != "apply -input=false -no-color standing.tfplan" {
		t.Fatalf("apply argv %q: options must precede the saved plan", got)
	}
	if a, _ := readApply(work); a.State != "applied" {
		t.Fatalf("apply receipt %+v", a)
	}
	n = len(f.calls)
	if r := standingRun(t, append([]string{"stop"}, target(tRG)...)...); r.Code != "OK" || r.Billing != "no" {
		t.Fatalf("stop: %+v", r)
	}
	if got := strings.Join(f.calls[n].args, " "); got != "vm deallocate --resource-group "+tRG+" --name "+tBox+" --subscription "+tSub+" --output json --only-show-errors" {
		t.Fatalf("stop argv %q", got)
	}
	n = len(f.calls)
	if r := standingRun(t, append(append([]string{"destroy"}, target(tRG)...), "--launch-token", tTok)...); r.Code != "OK" {
		t.Fatalf("destroy: %+v", r)
	}
	if got := strings.Join(f.calls[n].args, " "); got != "destroy -input=false -no-color "+pinned+" -auto-approve" {
		t.Fatalf("destroy argv %q: must reuse the pinned inputs", got)
	}
	if got := strings.Join(f.calls[n+1].args, " "); got != "group exists --name "+tRG+" --subscription "+tSub+" --output json --only-show-errors" {
		t.Fatalf("verify argv %q: must check the pinned RG in the pinned subscription", got)
	}
}

func TestStandingUnresolvedCreateIsNeverClosedByAnEmptyObservation(t *testing.T) {
	f, vars := setupStanding(t)
	module := filepath.Join("..", "..", "provision", "standing")
	standingRun(t, append(append([]string{"plan"}, target(tRG)...), "--module", module, "--vars", vars)...)
	f.failApply = true
	if r := standingRun(t, append(append([]string{"apply"}, target(tRG)...), "--launch-token", tTok)...); r.Code != "UNRESOLVED" {
		t.Fatalf("failed apply: %+v", r)
	}
	// a later controller (fresh process: only the files remain) destroys and sees an empty RG
	f.failApply = false
	if r := standingRun(t, append(append([]string{"destroy"}, target(tRG)...), "--launch-token", tTok)...); r.Code != "UNRESOLVED" {
		t.Fatalf("destroy after unresolved create must stay non-clean: %+v", r)
	}
	n := len(f.calls)
	if r := standingRun(t, append(append([]string{"apply"}, target(tRG)...), "--launch-token", tTok)...); r.Code != "UNRESOLVED" || len(f.calls) != n {
		t.Fatalf("apply after unresolved must refuse without a tool call (no blind retry): %+v", r)
	}
	if r := standingRun(t, append(append([]string{"plan"}, target(tRG)...), "--module", module, "--vars", vars)...); r.Code != "UNRESOLVED" {
		t.Fatalf("plan after unresolved: %+v", r)
	}
}

func TestStandingModuleConfiguresProvider(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "provision", "standing", "main.tf"))
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`(?s)provider "azurerm" \{\s*features \{\}\s*subscription_id\s*= var\.subscription_id\s*resource_provider_registrations = "none"\s*\}`)
	if !re.Match(b) {
		t.Fatal(`main.tf must configure provider "azurerm" { features {} subscription_id = var.subscription_id resource_provider_registrations = "none" }`)
	}
	if bytes.Contains(b, []byte("identity {")) {
		t.Fatal("the box must hold no identity")
	}
}

// The idle guard script from cloud-init, run with stubbed ss/find/systemctl/logger.
func TestStandingIdleGuardFailsClosed(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("bash idle guard runs on Linux")
	}
	b, err := os.ReadFile(filepath.Join("..", "..", "provision", "standing", "cloud-init.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?s)standing-idle-guard\n    permissions: "0755"\n    content: \|\n(.*?)\n  - path:`).FindSubmatch(b)
	if m == nil {
		t.Fatal("idle guard script not found")
	}
	script := strings.ReplaceAll(regexp.MustCompile(`(?m)^      `).ReplaceAllString(string(m[1]), ""), "${idle_minutes}", "60")
	script = strings.ReplaceAll(script, "$${", "${")
	dir := t.TempDir()
	guard := filepath.Join(dir, "guard")
	stubs := filepath.Join(dir, "bin")
	_ = os.MkdirAll(stubs, 0o755)
	_ = os.WriteFile(guard, []byte(script), 0o755)
	realFind, _ := exec.LookPath("find")
	for name, body := range map[string]string{
		"ss":        `[ -n "${SS_FAIL:-}" ] && exit 1; printf '%s' "${SS_OUT:-}"`,
		"find":      `[ -n "${FIND_FAIL:-}" ] && exit 1; exec ` + realFind + ` "$@"`,
		"systemctl": `echo "$*" >> "$STUB_LOG"`,
		"logger":    `echo "logger $*" >> "$STUB_LOG"`,
	} {
		_ = os.WriteFile(filepath.Join(stubs, name), []byte("#!/bin/bash\n"+body+"\n"), 0o755)
	}
	for _, c := range []struct {
		name     string
		stale    bool
		job      bool
		env      []string
		poweroff bool
	}{
		{"first run of a boot starts the clock", false, false, nil, false},
		{"stale clock, no activity", true, false, nil, true},
		{"stale clock, job marker", true, true, nil, false},
		{"stale clock, ssh session", true, false, []string{"SS_OUT=ESTAB 0 0 10.0.0.4:22 192.0.2.1:5000"}, false},
		{"stale clock, ss fails", true, false, []string{"SS_FAIL=1"}, false},
		{"stale clock, marker scan fails", true, false, []string{"FIND_FAIL=1"}, false},
	} {
		runDir := filepath.Join(t.TempDir(), "run")
		_ = os.MkdirAll(runDir, 0o755)
		if c.stale {
			st := filepath.Join(runDir, "last-active")
			_ = os.WriteFile(st, nil, 0o644)
			old := time.Now().Add(-2 * time.Hour)
			_ = os.Chtimes(st, old, old)
		}
		if c.job {
			_ = os.WriteFile(filepath.Join(runDir, "job-ci"), nil, 0o644)
		}
		log := filepath.Join(t.TempDir(), "log")
		cmd := exec.Command("bash", guard)
		cmd.Env = append(append(os.Environ(), "PATH="+stubs+":"+os.Getenv("PATH"), "STANDING_RUN_DIR="+runDir, "STUB_LOG="+log), c.env...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: guard exit %v: %s", c.name, err, out)
		}
		got, _ := os.ReadFile(log)
		if strings.Contains(string(got), "poweroff") != c.poweroff {
			t.Fatalf("%s: poweroff=%v, want %v (log %q)", c.name, !c.poweroff, c.poweroff, got)
		}
	}
}
