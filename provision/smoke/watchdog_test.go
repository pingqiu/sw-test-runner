package smoke

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pingqiu/sw-test-runner/packs/seaweed"
)

// A run that is still busy when the watchdog fires (a pack that ignores its
// context) records watchdog.json and exits 3 through the injected Exit.
func TestWatchdogFiresOnAStuckRun(t *testing.T) {
	c := fixture(t)
	f := &fake{}
	ctl := controller(f)
	var code atomic.Int32
	fired := make(chan struct{})
	release := make(chan struct{})
	ctl.Exit = func(rc int) { code.Store(int32(rc)); close(fired) }
	ctl.dogAfter = 300 * time.Millisecond
	ctl.Pack = func(ctx context.Context, inv []byte, cfg seaweed.CloudConfig) (string, error) {
		<-release // ignores ctx: stands for a hang the work deadline cannot end
		return "", nil
	}
	done := make(chan Result, 1)
	go func() { done <- ctl.Execute(context.Background(), "run", c) }()
	select {
	case <-fired:
	case <-time.After(10 * time.Second):
		t.Fatal("watchdog did not fire")
	}
	if code.Load() != 3 {
		t.Fatalf("exit code %d, want 3", code.Load())
	}
	b, e := os.ReadFile(filepath.Join(c.Output, "watchdog.json"))
	var rec map[string]string
	if e == nil {
		e = json.Unmarshal(b, &rec)
	}
	if e != nil || rec["schema"] != "seaweed.cloud-watchdog/v1" || rec["mode"] != "run" {
		t.Fatalf("watchdog.json: %v %v", rec, e)
	}
	close(release)
	<-done
}

// A run that finishes in time stops the watchdog: Exit is never called.
func TestWatchdogStoppedWhenRunReturns(t *testing.T) {
	c := fixture(t)
	f := &fake{}
	ctl := controller(f)
	ctl.Exit = func(rc int) { t.Errorf("watchdog fired after the run returned (rc %d)", rc) }
	ctl.dogAfter = 500 * time.Millisecond
	ctl.Execute(context.Background(), "run", c)
	time.Sleep(800 * time.Millisecond)
	if _, e := os.Stat(filepath.Join(c.Output, "watchdog.json")); !os.IsNotExist(e) {
		t.Fatalf("watchdog.json written after a run that returned in time (%v)", e)
	}
}

// A refused run (bad token, no output dir) never arms the watchdog.
func TestRefusedRunNeverArmsWatchdog(t *testing.T) {
	c := fixture(t)
	c.LaunchToken = ""
	ctl := Controller{Command: func(context.Context, string, string, ...string) ([]byte, error) { return nil, nil }, dogAfter: time.Millisecond}
	ctl.Exit = func(rc int) { t.Errorf("watchdog fired for a refused run (rc %d)", rc) }
	if r := ctl.Execute(context.Background(), "run", c); r.Code != "LAUNCH_REFUSED" {
		t.Fatalf("code %s", r.Code)
	}
	time.Sleep(50 * time.Millisecond)
}

// Every admitted run leaves outcome.json with the final remaining state.
func TestRunWritesOutcome(t *testing.T) {
	c := fixture(t)
	f := &fake{}
	r := controller(f).Execute(context.Background(), "run", c)
	b, e := os.ReadFile(filepath.Join(c.Output, "outcome.json"))
	var o runOutcome
	if e == nil {
		e = StrictJSON(b, &o)
	}
	if e != nil || o.Schema != "seaweed.cloud-outcome/v1" || o.Code != r.Code || o.Mode != "run" {
		t.Fatalf("outcome.json %+v %v (result code %s)", o, e, r.Code)
	}
}

// A refused run against an existing directory must not overwrite that
// directory's outcome record.
func TestRefusedRunKeepsAnEarlierOutcome(t *testing.T) {
	c := fixture(t)
	if e := os.MkdirAll(c.Output, 0700); e != nil {
		t.Fatal(e)
	}
	keep := []byte(`{"schema":"seaweed.cloud-outcome/v1","mode":"run","code":"COMPLETE","remaining_observed":true,"remaining_resource_ids":[],"create_unresolved":false,"at":"x"}`)
	os.WriteFile(filepath.Join(c.Output, "outcome.json"), keep, 0600)
	controller(&fake{}).Execute(context.Background(), "run", c) // refused: output exists
	if b, _ := os.ReadFile(filepath.Join(c.Output, "outcome.json")); string(b) != string(keep) {
		t.Fatalf("the earlier outcome was overwritten: %s", b)
	}
}

// The next session destroys first: an earlier guarded run with an apply and
// no clean outcome blocks a new run before any call.
func TestUnfinishedRunBlocksNewRun(t *testing.T) {
	sibling := func(t *testing.T, c Config, launch, apply bool, outcome string) string {
		d := filepath.Join(filepath.Dir(c.Output), "earlier-run")
		os.MkdirAll(d, 0700)
		if launch {
			os.WriteFile(filepath.Join(d, "launch.json"), []byte(`{}`), 0600)
		}
		if apply {
			os.WriteFile(filepath.Join(d, "apply-started"), []byte("x"), 0600)
		}
		if outcome != "" {
			os.WriteFile(filepath.Join(d, "outcome.json"), []byte(outcome), 0600)
		}
		return d
	}
	clean := `{"schema":"seaweed.cloud-outcome/v1","mode":"run","code":"PACK_FAILED","remaining_observed":true,"remaining_resource_ids":[],"create_unresolved":false,"at":"x"}`
	left := `{"schema":"seaweed.cloud-outcome/v1","mode":"run","code":"CLEANUP_INCOMPLETE","remaining_observed":true,"remaining_resource_ids":["/subscriptions/x/vm"],"create_unresolved":false,"at":"x"}`
	unobserved := `{"schema":"seaweed.cloud-outcome/v1","mode":"run","code":"CLEANUP_INCOMPLETE","remaining_observed":false,"remaining_resource_ids":[],"create_unresolved":false,"at":"x"}`
	failedDestroy := `{"schema":"seaweed.cloud-outcome/v1","mode":"destroy","code":"CLEANUP_INCOMPLETE","remaining_observed":true,"remaining_resource_ids":[],"create_unresolved":false,"at":"x"}`
	unresolved := `{"schema":"seaweed.cloud-outcome/v1","mode":"run","code":"CREATE_UNRESOLVED","remaining_observed":true,"remaining_resource_ids":[],"create_unresolved":true,"at":"x"}`
	for _, tc := range []struct {
		name          string
		launch, apply bool
		outcome       string
		block         bool
	}{
		{"no outcome", true, true, "", true},
		{"resources remain", true, true, left, true},
		{"remaining not observed", true, true, unobserved, true},
		{"create unresolved", true, true, unresolved, true},
		{"failed destroy, empty list", true, true, failedDestroy, true},
		{"malformed outcome", true, true, `{"code":`, true},
		{"clean (failed pack, empty RG)", true, true, clean, false},
		{"refused before apply", true, false, "", false},
		{"pre-guard dir (no launch.json)", false, true, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := fixture(t)
			sibling(t, c, tc.launch, tc.apply, tc.outcome)
			calls := 0
			ctl := controller(&fake{})
			inner := ctl.Command
			ctl.Command = func(ctx context.Context, dir, tool string, args ...string) ([]byte, error) {
				calls++
				return inner(ctx, dir, tool, args...)
			}
			r := ctl.Execute(context.Background(), "run", c)
			if tc.block {
				if r.Code != "RECOVERY_REQUIRED" || calls != 0 || !strings.Contains(r.Error, "earlier-run") {
					t.Fatalf("code %s calls %d error %q, want RECOVERY_REQUIRED naming earlier-run with 0 calls", r.Code, calls, r.Error)
				}
				if _, e := os.Stat(c.Output); !os.IsNotExist(e) {
					t.Fatal("output dir created for a blocked run")
				}
			} else if r.Code == "RECOVERY_REQUIRED" || calls == 0 {
				t.Fatalf("code %s calls %d: a %s sibling must not block", r.Code, calls, tc.name)
			}
		})
	}
}
