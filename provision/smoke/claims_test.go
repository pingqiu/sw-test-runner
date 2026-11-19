package smoke

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// withGroup rewrites the fixture's tfvars to another resource group.
func withGroup(t *testing.T, c Config, group string) Config {
	t.Helper()
	b, _ := os.ReadFile(c.Variables)
	var v map[string]any
	if e := json.Unmarshal(b, &v); e != nil {
		t.Fatal(e)
	}
	v["resource_group_name"] = group
	b, _ = json.Marshal(v)
	if e := os.WriteFile(c.Variables, b, 0600); e != nil {
		t.Fatal(e)
	}
	c.VariablesSHA256 = digest(b)
	return c
}

func claimFiles(t *testing.T, dir string) []string {
	t.Helper()
	m, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	for i := range m {
		m[i] = filepath.Base(m[i])
	}
	return m
}

// #140: an unfinished run keeps its RG claim; a second run on that RG is
// refused before any cloud call even though its output is not a sibling
// (the sibling-dir gate cannot see it); a run on another RG proceeds.
func TestRGClaimBlocksSameGroupAcrossOutputParents(t *testing.T) {
	claims := t.TempDir()
	first := fixture(t) // RG dedicated-test, output under its own temp parent
	f1 := &fake{fail: "destroy"}
	ctl := controller(f1)
	ctl.ClaimsDir = claims
	if r := ctl.Execute(context.Background(), "run", first); r.Code != "CLEANUP_INCOMPLETE" {
		t.Fatalf("first run: %+v", r)
	}
	if got := claimFiles(t, claims); len(got) != 1 || got[0] != "11111111-1111-1111-1111-111111111111__dedicated-test.json" {
		t.Fatalf("unclean run must keep its claim: %v", got)
	}

	same := fixture(t) // same RG, another output parent
	f2 := &fake{}
	ctl2 := controller(f2)
	ctl2.ClaimsDir = claims
	r := ctl2.Execute(context.Background(), "run", same)
	if r.Code != "RG_CLAIMED" || !strings.Contains(r.Error, "claimed by run output") || len(f2.events) != 0 {
		t.Fatalf("same RG, non-sibling output: %+v events=%v", r, f2.events)
	}
	if _, e := os.Stat(same.Output); !os.IsNotExist(e) {
		t.Fatal("a refused run created its output dir")
	}

	other := withGroup(t, fixture(t), "dedicated-other")
	f3 := &fake{}
	ctl3 := controller(f3)
	ctl3.ClaimsDir = claims
	if r := ctl3.Execute(context.Background(), "run", other); r.Code != "COMPLETE" {
		t.Fatalf("disjoint RG: %+v", r)
	}
	if got := claimFiles(t, claims); len(got) != 1 {
		t.Fatalf("a clean run must release its claim, the unclean one stays: %v", got)
	}

	// Recovery: destroy of the first run reaching a clean outcome releases it.
	f1.fail = ""
	if r := ctl.Execute(context.Background(), "destroy", first); r.Code != "COMPLETE" {
		t.Fatalf("destroy: %+v", r)
	}
	if got := claimFiles(t, claims); len(got) != 0 {
		t.Fatalf("clean destroy must release the claim: %v", got)
	}
	if r := ctl2.Execute(context.Background(), "run", same); r.Code != "COMPLETE" {
		t.Fatalf("after recovery the RG is free: %+v", r)
	}
}

// A run refused before any apply (RG not empty) owns nothing: its claim goes.
func TestRGClaimReleasedWhenNoApplyStarted(t *testing.T) {
	claims := t.TempDir()
	c := fixture(t)
	f := &fake{live: true}
	ctl := controller(f)
	ctl.ClaimsDir = claims
	f.lists = 1 // the first RG list (pre-create) already sees the VM
	if r := ctl.Execute(context.Background(), "run", c); r.Code != "RG_NOT_EMPTY" {
		t.Fatalf("%+v", r)
	}
	if got := claimFiles(t, claims); len(got) != 0 {
		t.Fatalf("a run that never applied must not hold the RG: %v", got)
	}
}

// A stale claim is replaced only when its holder is dead on this host AND its
// output is settled; a live holder, another host, or an unsettled output keep it.
func TestRGClaimReplacement(t *testing.T) {
	host, _ := os.Hostname()
	for _, tc := range []struct {
		name         string
		host         string
		pid          int
		applied      bool
		clean        bool
		wantReplaced bool
	}{
		{"dead holder, clean outcome", host, 1 << 30, true, true, true},
		{"dead holder, never applied", host, 1 << 30, false, false, true},
		{"dead holder, unclean", host, 1 << 30, true, false, false},
		{"live holder (this process), never applied", host, os.Getpid(), false, false, false},
		{"other host, clean outcome", "elsewhere", 1 << 30, true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.pid == os.Getpid() && runtime.GOOS != "linux" {
				t.Skip("liveness needs /proc")
			}
			claims, held := t.TempDir(), t.TempDir()
			if tc.applied {
				os.WriteFile(filepath.Join(held, "apply-started"), []byte("apply may have issued creates\n"), 0600)
			}
			if tc.clean {
				rec, _ := json.Marshal(runOutcome{Schema: "seaweed.cloud-outcome/v1", Mode: "run", Code: "COMPLETE", RemainingObserved: true, Remaining: []string{}, At: time.Now().UTC().Format(time.RFC3339)})
				os.WriteFile(filepath.Join(held, "outcome.json"), rec, 0600)
			}
			start := ""
			if tc.pid == os.Getpid() {
				start, _ = procStart(tc.pid)
			}
			rec, _ := json.Marshal(rgClaim{Schema: "seaweed.cloud-claim/v1", Subscription: "s", ResourceGroup: "g", Run: "held", Output: held, Host: tc.host, PID: tc.pid, ProcStart: start, CreatedAt: "2026-11-19T00:00:00Z"})
			p := filepath.Join(claims, "s__g.json")
			os.WriteFile(p, rec, 0600)
			_, err := takeClaim(claims, "s", "G", filepath.Join(t.TempDir(), "new"), testLaunchToken)
			if replaced := err == nil; replaced != tc.wantReplaced {
				t.Fatalf("replaced=%v err=%v", replaced, err)
			}
			got, _ := readClaim(p)
			if want := map[bool]string{true: "new", false: "held"}[tc.wantReplaced]; got.Run != want {
				t.Fatalf("claim holder %q, want %q", got.Run, want)
			}
		})
	}
}

// The claim records who holds it, and Claims reports liveness without
// releasing anything.
func TestRGClaimRecordAndStatus(t *testing.T) {
	claims := t.TempDir()
	out := filepath.Join(t.TempDir(), "run-a")
	if _, err := takeClaim(claims, "Sub-1", "RG-A", out, testLaunchToken); err != nil {
		t.Fatal(err)
	}
	st, err := Claims(claims)
	if err != nil || len(st) != 1 {
		t.Fatalf("%v %v", st, err)
	}
	host, _ := os.Hostname()
	c := st[0]
	if c.Run != "run-a" || c.Output != out || c.Host != host || c.PID != os.Getpid() || c.LaunchToken != testLaunchToken || c.ResourceGroup != "RG-A" || !c.Settled {
		t.Fatalf("%+v", c)
	}
	if _, err := os.Stat(filepath.Join(claims, "sub-1__rg-a.json")); err != nil {
		t.Fatal("the key is lower-case subscription__rg:", err)
	}
	// the holder (this process) is live on Linux: a case variant of the RG is the same claim
	if _, err := takeClaim(claims, "sub-1", "rg-a", filepath.Join(t.TempDir(), "run-b"), testLaunchToken); runtime.GOOS == "linux" && err == nil {
		t.Fatal("case-variant RG name took a held claim")
	}
	if _, err := takeClaim(claims, "s", "../etc", out, testLaunchToken); err == nil {
		t.Fatal("unsafe RG name accepted")
	}
}
