package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
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
	tRG  = "rg-box-standing-20261120"
	tBox = "c04-standing"
	tSub = "11111111-2222-3333-4444-555555555555"
	tTok = "0123456789abcdef01234567"
)

type toolCall struct {
	dir, tool string
	args      []string
}

// fakeStanding replaces the external command boundary. failApply fails terraform apply; a plan whose -var-file
// contains FAILPLAN fails.
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
	case tool == "terraform" && args[0] == "plan":
		for _, a := range args {
			if p, ok := strings.CutPrefix(a, "-var-file="); ok {
				if b, _ := os.ReadFile(p); bytes.Contains(b, []byte("FAILPLAN")) {
					return []byte("Error: No value for required variable"), errors.New("exit 1")
				}
			}
		}
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

func writeVars(t *testing.T, extra string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "box.json")
	if err := os.WriteFile(p, []byte(`{"subscription_id":"`+tSub+`","resource_group_name":"`+tRG+`","name":"`+tBox+`"`+extra+`}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func setupStanding(t *testing.T) (*fakeStanding, string) {
	t.Helper()
	f := &fakeStanding{rgExists: "false"}
	home := t.TempDir()
	oldTool, oldOS, oldHome := standingTool, standingGOOS, standingHome
	standingTool, standingGOOS, standingHome = f.native, "linux", func() (string, error) { return home, nil }
	t.Cleanup(func() { standingTool, standingGOOS, standingHome = oldTool, oldOS, oldHome })
	return f, writeVars(t, "")
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

var tModule = filepath.Join("..", "..", "provision", "standing")

func target(rg string) []string { return []string{"--name", tBox, "--resource-group", rg} }
func planArgs(vars string) []string {
	return append(append([]string{"plan"}, target(tRG)...), "--module", tModule, "--vars", vars)
}
func tokArgs(action string) []string {
	return append(append([]string{action}, target(tRG)...), "--launch-token", tTok)
}

func standingBox() string {
	home, _ := standingHome()
	return filepath.Join(home, ".local", "state", "seaweed-cloud", "standing", tBox)
}

// snapshot maps every regular file under dir (recursively) to its content.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			b, _ := os.ReadFile(p)
			out[p] = string(b)
		}
		return nil
	})
	return out
}

func sameSnapshot(t *testing.T, what string, before, after map[string]string) {
	t.Helper()
	for k, v := range before {
		if after[k] != v {
			t.Fatalf("%s changed %s", what, k)
		}
	}
	for k := range after {
		if _, ok := before[k]; !ok {
			t.Fatalf("%s created %s", what, k)
		}
	}
}

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
	f, _ := setupStanding(t)
	other := filepath.Join(t.TempDir(), "other.json")
	_ = os.WriteFile(other, []byte(`{"subscription_id":"`+tSub+`","resource_group_name":"rg-box-standing-20261121","name":"`+tBox+`"}`), 0o600)
	for _, c := range []struct {
		args []string
		code string
	}{
		{nil, "USAGE"},
		{append([]string{"reboot"}, target(tRG)...), "USAGE"},
		{append([]string{"status"}, target("rg-runner-azure-test-20261029")...), "USAGE"},
		{append([]string{"plan"}, target(tRG)...), "USAGE"},
		{append([]string{"apply"}, target(tRG)...), "REFUSED"},
		{append(append([]string{"destroy"}, target(tRG)...), "--launch-token", "xyz"), "REFUSED"},
		{append([]string{"status"}, target(tRG)...), "REFUSED"}, // nothing applied
		{tokArgs("apply"), "REFUSED"},                           // no eligible plan
		{tokArgs("destroy"), "REFUSED"},                         // nothing applied
		{planArgs(other), "REFUSED"},                            // vars name another RG
	} {
		if r := standingRun(t, c.args...); r.Code != c.code {
			t.Fatalf("%v: code %s (%s), want %s", c.args, r.Code, r.Reason, c.code)
		}
	}
	if len(f.calls) != 0 {
		t.Fatalf("refusals reached the command boundary: %+v", f.calls)
	}
}

func TestStandingLifecycleArgvAndPinnedScope(t *testing.T) {
	f, vars := setupStanding(t)
	if r := standingRun(t, planArgs(vars)...); r.Code != "OK" || r.Generation != "g0001" {
		t.Fatalf("plan: %+v", r)
	}
	box := standingBox()
	work := filepath.Join(box, "g0001")
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
	if b, _ := os.ReadFile(filepath.Join(box, standingCurrentFile)); string(b) != "g0001" || !exists(filepath.Join(work, standingReadyFile)) {
		t.Fatalf("CURRENT %q / READY after a completed plan", b)
	}
	n := len(f.calls)
	if r := standingRun(t, tokArgs("apply")...); r.Code != "OK" || r.Generation != "g0001" {
		t.Fatalf("apply: %+v", r)
	}
	if got := strings.Join(f.calls[n].args, " "); got != "apply -input=false -no-color standing.tfplan" || f.calls[n].dir != work {
		t.Fatalf("apply argv %q in %s: options must precede the saved plan", got, f.calls[n].dir)
	}
	// a different RG for the applied box is refused before any tool call
	n = len(f.calls)
	if r := standingRun(t, append([]string{"status"}, target("rg-box-standing-20261121")...)...); r.Code != "REFUSED" || len(f.calls) != n {
		t.Fatalf("wrong-RG status: %+v calls %d->%d", r, n, len(f.calls))
	}
	if r := standingRun(t, append([]string{"stop"}, target(tRG)...)...); r.Code != "OK" || r.Billing != "no" {
		t.Fatalf("stop: %+v", r)
	}
	if got := strings.Join(f.calls[n].args, " "); got != "vm deallocate --resource-group "+tRG+" --name "+tBox+" --subscription "+tSub+" --output json --only-show-errors" {
		t.Fatalf("stop argv %q", got)
	}
	n = len(f.calls)
	if r := standingRun(t, tokArgs("destroy")...); r.Code != "OK" {
		t.Fatalf("destroy: %+v", r)
	}
	if got := strings.Join(f.calls[n].args, " "); got != "destroy -input=false -no-color "+pinned+" -auto-approve" || f.calls[n].dir != work {
		t.Fatalf("destroy argv %q: must reuse the applied generation's pinned inputs", got)
	}
	if got := strings.Join(f.calls[n+1].args, " "); got != "group exists --name "+tRG+" --subscription "+tSub+" --output json --only-show-errors" {
		t.Fatalf("verify argv %q: must check the pinned RG in the pinned subscription", got)
	}
	if r := standingRun(t, planArgs(vars)...); r.Code != "OK" || r.Generation != "g0002" {
		t.Fatalf("plan after a verified destroy: %+v", r)
	}
}

// Invariant 1: apply binds exactly one successful plan generation; starting any re-plan, even one that fails,
// revokes the previous plan's eligibility; teardown is bound to the generation actually applied.
func TestStandingFailedReplanRevokesTheOldPlan(t *testing.T) {
	f, vars := setupStanding(t)
	standingRun(t, planArgs(vars)...) // A = g0001, eligible
	box := standingBox()
	genA := snapshot(t, filepath.Join(box, "g0001"))
	if r := standingRun(t, planArgs(writeVars(t, `,"vm_size":"FAILPLAN"`))...); r.Code != "FAILED" || r.Generation != "g0002" {
		t.Fatalf("failing re-plan B: %+v", r)
	}
	sameSnapshot(t, "failed re-plan B (generation A)", genA, snapshot(t, filepath.Join(box, "g0001")))
	if exists(filepath.Join(box, standingCurrentFile)) {
		t.Fatal("a failed re-plan must leave no CURRENT generation")
	}
	n := len(f.calls)
	if r := standingRun(t, tokArgs("apply")...); r.Code != "REFUSED" || len(f.calls) != n {
		t.Fatalf("apply after a failed re-plan must create nothing: %+v calls %d->%d", r, n, len(f.calls))
	}
	if r := standingRun(t, planArgs(vars)...); r.Code != "OK" || r.Generation != "g0003" {
		t.Fatalf("re-plan C: %+v", r)
	}
	n = len(f.calls)
	if r := standingRun(t, tokArgs("apply")...); r.Code != "OK" || r.Generation != "g0003" || f.calls[n].dir != filepath.Join(box, "g0003") {
		t.Fatalf("apply must use generation C: %+v in %s", r, f.calls[n].dir)
	}
	n = len(f.calls)
	if r := standingRun(t, tokArgs("destroy")...); r.Code != "OK" || f.calls[n].dir != filepath.Join(box, "g0003") ||
		strings.Join(f.calls[n].args, " ") != "destroy -input=false -no-color -var-file="+filepath.Join(box, "g0003", standingVarsFile)+" -auto-approve" {
		t.Fatalf("teardown must be bound to the applied generation C: %+v %+v", r, f.calls[n])
	}
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

// The lead disposition (2): an unknown Lstat error on generation evidence is UNKNOWN (fail closed), never "absent".
func TestStandingEvidenceReadErrorIsUnknown(t *testing.T) {
	old := standingLstat
	t.Cleanup(func() { standingLstat = old })
	status := append([]string{"status"}, target(tRG)...)
	for _, c := range []struct {
		evidence string
		actions  [][]string // each must answer UNKNOWN with zero tool calls and no write
	}{
		{standingAttemptFile, [][]string{planArgs(""), tokArgs("apply"), tokArgs("destroy"), status}}, // the live-generation scan
		{standingIdentFile, [][]string{planArgs("")}},                                                 // plan's previous-identity check
		{standingReadyFile, [][]string{tokArgs("apply")}},                                             // apply eligibility
	} {
		standingLstat = old
		f, vars := setupStanding(t)
		standingRun(t, planArgs(vars)...)
		standingLstat = func(p string) (os.FileInfo, error) {
			if filepath.Base(p) == c.evidence {
				return nil, fs.ErrPermission
			}
			return os.Lstat(p)
		}
		before := snapshot(t, standingBox())
		n := len(f.calls)
		for _, a := range c.actions {
			if a[0] == "plan" {
				a = planArgs(vars)
			}
			if r := standingRun(t, a...); r.Code != "UNKNOWN" || len(f.calls) != n {
				t.Fatalf("%s read error: %s -> %+v, calls %d->%d", c.evidence, a[0], r, n, len(f.calls))
			}
		}
		sameSnapshot(t, c.evidence+" read error", before, snapshot(t, standingBox()))
	}
}

// CURRENT may only ever name a completed plan: a CURRENT naming a generation without READY is not eligible.
func TestStandingCurrentWithoutReadyIsNotEligible(t *testing.T) {
	f, vars := setupStanding(t)
	standingRun(t, planArgs(vars)...)
	box := standingBox()
	_ = os.Remove(filepath.Join(box, "g0001", standingReadyFile))
	n := len(f.calls)
	if r := standingRun(t, tokArgs("apply")...); r.Code != "REFUSED" || len(f.calls) != n {
		t.Fatalf("apply of a generation without READY: %+v calls %d->%d", r, n, len(f.calls))
	}
}

// Invariant 2: a missing receipt in a possibly-used generation is UNKNOWN: no create, no re-plan, no clean listing.
func TestStandingMissingReceiptAfterAttemptIsUnknown(t *testing.T) {
	f, vars := setupStanding(t)
	standingRun(t, planArgs(vars)...)
	f.failApply = true
	if r := standingRun(t, tokArgs("apply")...); r.Code != "UNRESOLVED" {
		t.Fatalf("failed apply: %+v", r)
	}
	f.failApply = false
	work := filepath.Join(standingBox(), "g0001")
	if err := os.Remove(filepath.Join(work, standingApplyFile)); err != nil {
		t.Fatal(err)
	}
	n := len(f.calls)
	if r := standingRun(t, tokArgs("apply")...); r.Code != "UNRESOLVED" || len(f.calls) != n {
		t.Fatalf("apply after a lost receipt: %+v calls %d->%d", r, n, len(f.calls))
	}
	if r := standingRun(t, planArgs(vars)...); r.Code != "UNRESOLVED" || len(f.calls) != n {
		t.Fatalf("plan after a lost receipt: %+v", r)
	}
	if r := standingRun(t, tokArgs("destroy")...); r.Code != "UNRESOLVED" {
		t.Fatalf("destroy + empty RG after a lost receipt must stay non-clean: %+v", r)
	}
	// Terraform state without any marker also counts as a used generation
	f2, vars2 := setupStanding(t)
	standingRun(t, planArgs(vars2)...)
	w2 := filepath.Join(standingBox(), "g0001")
	_ = os.WriteFile(filepath.Join(w2, "terraform.tfstate"), []byte(`{}`), 0o600)
	n = len(f2.calls)
	if r := standingRun(t, tokArgs("apply")...); r.Code != "UNRESOLVED" || len(f2.calls) != n {
		t.Fatalf("apply over leftover state: %+v", r)
	}
}

func TestStandingUnresolvedCreateIsNeverClosedByAnEmptyObservation(t *testing.T) {
	f, vars := setupStanding(t)
	standingRun(t, planArgs(vars)...)
	f.failApply = true
	if r := standingRun(t, tokArgs("apply")...); r.Code != "UNRESOLVED" {
		t.Fatalf("failed apply: %+v", r)
	}
	f.failApply = false
	if r := standingRun(t, tokArgs("destroy")...); r.Code != "UNRESOLVED" {
		t.Fatalf("destroy after unresolved create must stay non-clean: %+v", r)
	}
	n := len(f.calls)
	if r := standingRun(t, tokArgs("apply")...); r.Code != "UNRESOLVED" || len(f.calls) != n {
		t.Fatalf("apply after unresolved must refuse without a tool call (no blind retry): %+v", r)
	}
	if r := standingRun(t, planArgs(vars)...); r.Code != "UNRESOLVED" || len(f.calls) != n {
		t.Fatalf("plan after unresolved: %+v", r)
	}
}

// A second plan against an applied box must not touch anything, and the box stays destroyable.
func TestStandingReplanNeverOverwritesAnAppliedBox(t *testing.T) {
	f, vars := setupStanding(t)
	standingRun(t, planArgs(vars)...)
	if r := standingRun(t, tokArgs("apply")...); r.Code != "OK" {
		t.Fatalf("apply: %+v", r)
	}
	box := standingBox()
	before := snapshot(t, box)
	n := len(f.calls)
	if r := standingRun(t, planArgs(writeVars(t, `,"vm_size":"Standard_D2s_v5"`))...); r.Code != "REFUSED" || len(f.calls) != n {
		t.Fatalf("re-plan of an applied box: %+v, calls %d->%d", r, n, len(f.calls))
	}
	sameSnapshot(t, "re-plan of an applied box", before, snapshot(t, box))
	if r := standingRun(t, tokArgs("apply")...); r.Code != "REFUSED" || len(f.calls) != n {
		t.Fatalf("second apply of an applied box: %+v", r)
	}
	if r := standingRun(t, tokArgs("destroy")...); r.Code != "OK" {
		t.Fatalf("applied box must stay destroyable: %+v", r)
	}
}

// An unreadable or unknown receipt fails closed: never retry, never clean.
func TestStandingUnreadableReceiptFailsClosed(t *testing.T) {
	for _, receipt := range []string{"not json", `{"state":"weird"}`} {
		f, vars := setupStanding(t)
		standingRun(t, planArgs(vars)...)
		_ = os.WriteFile(filepath.Join(standingBox(), "g0001", standingApplyFile), []byte(receipt), 0o600)
		n := len(f.calls)
		if r := standingRun(t, tokArgs("apply")...); r.Code != "UNRESOLVED" || len(f.calls) != n {
			t.Fatalf("%q: apply %+v calls %d->%d", receipt, r, n, len(f.calls))
		}
		if r := standingRun(t, planArgs(vars)...); r.Code != "UNRESOLVED" || len(f.calls) != n {
			t.Fatalf("%q: plan %+v", receipt, r)
		}
		if r := standingRun(t, tokArgs("destroy")...); r.Code != "UNRESOLVED" {
			t.Fatalf("%q: destroy + empty RG must stay non-clean: %+v", receipt, r)
		}
	}
}

// The reviewer P2 fixture: a viable module and a real read error on the newest generation's identity: the guard alone
// refuses, with zero tool calls and nothing written.
func TestStandingUnreadableIdentityRefusesAViablePlan(t *testing.T) {
	f, vars := setupStanding(t)
	standingRun(t, planArgs(vars)...)
	box := standingBox()
	ident := filepath.Join(box, "g0001", standingIdentFile)
	_ = os.Remove(ident)
	if err := os.Mkdir(ident, 0o700); err != nil { // reading a directory fails: a real read error
		t.Fatal(err)
	}
	before := snapshot(t, box)
	n := len(f.calls)
	if r := standingRun(t, planArgs(vars)...); r.Code != "REFUSED" || len(f.calls) != n {
		t.Fatalf("plan over an unreadable identity: %+v calls %d->%d", r, n, len(f.calls))
	}
	sameSnapshot(t, "plan over an unreadable identity", before, snapshot(t, box))
}

// The pinned tfvars must be the reviewed bytes naming the pinned identity before any mutation.
func TestStandingPinnedVarsDriftRefused(t *testing.T) {
	f, vars := setupStanding(t)
	standingRun(t, planArgs(vars)...)
	if r := standingRun(t, tokArgs("apply")...); r.Code != "OK" {
		t.Fatalf("apply: %+v", r)
	}
	_ = os.WriteFile(filepath.Join(standingBox(), "g0001", standingVarsFile), []byte(`{"subscription_id":"`+tSub+`","resource_group_name":"rg-box-standing-20261121","name":"`+tBox+`"}`), 0o600)
	n := len(f.calls)
	for _, a := range [][]string{tokArgs("destroy"), append([]string{"status"}, target(tRG)...)} {
		if r := standingRun(t, a...); r.Code != "REFUSED" || len(f.calls) != n {
			t.Fatalf("%v with drifted pinned vars: %+v calls %d->%d", a, r, n, len(f.calls))
		}
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

// The controller reaper deallocates exactly the boxes reported "stopped" (powered off by the idle guard, still billing).
func TestStandingReaperDeallocatesOnlyStoppedBoxes(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("bash reaper runs on Linux")
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	fake := filepath.Join(dir, "seaweed-cloud")
	_ = os.WriteFile(fake, []byte(`#!/bin/bash
echo "$*" >> "`+log+`"
case "$2:$4" in
  status:a) echo '{"power_state":"stopped","code":"OK"}' ;;
  status:b) echo '{"power_state":"running","code":"OK"}' ;;
  status:c) echo '{"code":"UNKNOWN"}' ;;
  stop:a) echo '{"power_state":"deallocated","code":"OK"}' ;;
esac
`), 0o755)
	cmd := exec.Command("bash", filepath.Join("..", "..", "provision", "standing", "reaper.sh"), fake, "a:rg-a-standing-20261121", "b:rg-b-standing-20261121", "c:rg-c-standing-20261121")
	cmd.Env = append(os.Environ(), "REAP_ONCE=1", "REAP_STATE="+filepath.Join(dir, "state"))
	out, err := cmd.CombinedOutput()
	if err != nil { // c is UNKNOWN once: logged, not yet an alert (REAP_ALERT_AFTER default 3)
		t.Fatalf("reaper: %v: %s", err, out)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "state", "last")); len(b) == 0 {
		t.Fatal("reaper must stamp its last pass for the cost line")
	}
	calls, _ := os.ReadFile(log)
	var stops []string
	for _, l := range strings.Split(strings.TrimSpace(string(calls)), "\n") {
		if strings.HasPrefix(l, "standing stop") {
			stops = append(stops, l)
		}
	}
	if len(stops) != 1 || stops[0] != "standing stop --name a --resource-group rg-a-standing-20261121" {
		t.Fatalf("reaper must deallocate only the stopped box a: %q (output %s)", stops, out)
	}
}

// The architect F2: repeated UNKNOWN status or a failing deallocate must not stay silent: ALERT and exit 3 after
// REAP_ALERT_AFTER consecutive passes; a later good pass resets the count.
func TestStandingReaperAlertsOnRepeatedFailure(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("bash reaper runs on Linux")
	}
	dir := t.TempDir()
	mode := filepath.Join(dir, "mode")
	fake := filepath.Join(dir, "seaweed-cloud")
	_ = os.WriteFile(fake, []byte(`#!/bin/bash
m=$(cat "`+mode+`")
case "$2:$m" in
  status:fail) echo '{"power_state":"stopped","code":"OK"}' ;;
  stop:fail) echo '{"code":"FAILED","reason":"az vm deallocate: throttled"}'; exit 2 ;;
  status:good) echo '{"power_state":"deallocated","code":"OK"}' ;;
esac
`), 0o755)
	pass := func(m string) (string, int) {
		_ = os.WriteFile(mode, []byte(m), 0o644)
		cmd := exec.Command("bash", filepath.Join("..", "..", "provision", "standing", "reaper.sh"), fake, "a:rg-a-standing-20261121")
		cmd.Env = append(os.Environ(), "REAP_ONCE=1", "REAP_ALERT_AFTER=2", "REAP_STATE="+filepath.Join(dir, "state"))
		out, _ := cmd.CombinedOutput()
		return string(out), cmd.ProcessState.ExitCode()
	}
	if out, rc := pass("fail"); rc != 0 || strings.Contains(out, "ALERT") || !strings.Contains(out, "FAILED") {
		t.Fatalf("first failed stop: rc %d, want 0, logged FAILED, no ALERT yet: %s", rc, out)
	}
	if out, rc := pass("fail"); rc != 3 || !strings.Contains(out, "ALERT a: 2 consecutive") {
		t.Fatalf("second failed stop: rc %d, want 3 with ALERT: %s", rc, out)
	}
	if out, rc := pass("good"); rc != 0 || strings.Contains(out, "ALERT") {
		t.Fatalf("a good pass must reset the count: rc %d: %s", rc, out)
	}
}
