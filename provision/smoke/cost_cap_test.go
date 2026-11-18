package smoke

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pingqiu/sw-test-runner/packs/seaweed"
)

// setDecl rewrites the fixture's run_declaration and re-pins the variables file.
func setDecl(t *testing.T, c *Config, edit func(run map[string]any)) {
	t.Helper()
	b, e := os.ReadFile(c.Variables)
	var v map[string]any
	if e == nil {
		e = json.Unmarshal(b, &v)
	}
	if e != nil {
		t.Fatal(e)
	}
	run, _ := v["run_declaration"].(map[string]any)
	edit(run)
	v["run_declaration"] = run
	b, _ = json.Marshal(v)
	if e = os.WriteFile(c.Variables, b, 0600); e != nil {
		t.Fatal(e)
	}
	c.VariablesSHA256 = digest(b)
}

// The cost cap is admitted before any cloud call: a missing/invalid rate, a rate
// that would pass budget_usd before the hard deadline, or an abort estimate that
// does not even cover the cleanup reserve, refuses with 0 tool calls and no
// output dir. The fixture: hard deadline ~50 min out, reserve 1200 s, budget 3,
// abort 2.5.
func TestCostCapAdmission(t *testing.T) {
	for _, tc := range []struct {
		name   string
		hourly float64
		want   string
	}{
		{"missing", 0, "positive number"},
		{"negative", -1, "positive number"},
		{"infinite", math.Inf(1), "positive number"},
		{"worst case above budget", 5, "above budget_usd"}, // ~4.2 USD to the hard deadline > budget 3
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := fixture(t)
			c.HourlyUSD = tc.hourly
			calls := 0
			r := (Controller{Command: func(ctx context.Context, dir, tool string, args ...string) ([]byte, error) {
				calls++
				return nil, nil
			}}).Execute(context.Background(), "run", c)
			if r.Code != "COST_REFUSED" || !strings.Contains(r.Error, tc.want) {
				t.Fatalf("hourly %v: code %s error %q, want COST_REFUSED containing %q", tc.hourly, r.Code, r.Error, tc.want)
			}
			if calls != 0 {
				t.Fatalf("hourly %v: %d tool call(s) before the refusal", tc.hourly, calls)
			}
			if _, e := os.Stat(c.Output); !os.IsNotExist(e) {
				t.Fatalf("hourly %v: output dir exists after the refusal", tc.hourly)
			}
		})
	}
}

// The reserve rule on its own: a cheap enough rate to pass the budget, but an
// abort estimate so small that it is spent within the cleanup reserve.
func TestCostCapRefusesAbortBelowReserve(t *testing.T) {
	c := fixture(t)
	c.HourlyUSD = 2.6 // 2.6 USD/h: ~2.2 USD to the hard deadline <= budget 3
	// abort 2.5 USD lasts 57.7 min > reserve 20 min: fine. Lower the abort estimate
	// by rewriting the declaration so that it lasts less than the reserve.
	setDecl(t, &c, func(run map[string]any) { run["abort_estimate_usd"] = 0.5; run["budget_usd"] = 3 })
	c.EstimatedUSD = 0.4
	r := (Controller{Command: func(ctx context.Context, dir, tool string, args ...string) ([]byte, error) {
		t.Fatalf("tool call %s before the refusal", tool)
		return nil, nil
	}}).Execute(context.Background(), "run", c)
	if r.Code != "COST_REFUSED" || !strings.Contains(r.Error, "does not cover the cleanup reserve") {
		t.Fatalf("code %s error %q, want COST_REFUSED (abort below reserve)", r.Code, r.Error)
	}
}

// A run records its cost admission in cost.json before its first tool call.
func TestRunRecordsCostAdmission(t *testing.T) {
	c := fixture(t)
	f := &fake{}
	ctl := controller(f)
	inner, seen := ctl.Command, false
	ctl.Command = func(ctx context.Context, dir, tool string, args ...string) ([]byte, error) {
		if !seen {
			seen = true
			b, e := os.ReadFile(filepath.Join(c.Output, "cost.json"))
			var rec map[string]any
			if e == nil {
				e = json.Unmarshal(b, &rec)
			}
			if e != nil || rec["schema"] != "seaweed.cloud-cost/v1" || rec["hourly_usd"] != testHourlyUSD || rec["cost_deadline"] == "" {
				t.Errorf("cost.json before the first call: %v %v", rec, e)
			}
		}
		return inner(ctx, dir, tool, args...)
	}
	ctl.Execute(context.Background(), "run", c)
	if !seen {
		t.Fatal("the run made no tool call")
	}
}

// At runtime the cost deadline cuts the pack, the run reports COST_ABORTED and
// destroy still runs. The rate is set so the abort estimate lasts only the
// reserve plus ~2 s: the pack (which blocks until its context ends) is cut by the
// COST deadline, long before the work deadline.
func TestCostDeadlineCutsPackAndDestroys(t *testing.T) {
	c := fixture(t)
	// A short window so the worst case stays under budget: hard ~12 min out,
	// work = hard - reserve.
	hard := time.Now().UTC().Add(12 * time.Minute).Truncate(time.Minute)
	setDecl(t, &c, func(run map[string]any) {
		run["cloud_cleanup_reserve_seconds"] = 600
		run["hard_deadline"] = hard
		run["work_deadline"] = hard.Add(-600 * time.Second)
		run["abort_estimate_usd"] = 1.0
		run["budget_usd"] = 3
	})
	c.EstimatedUSD = 0.5
	// abort 1.0 USD must last reserve (600 s) + 2 s: hourly = 1.0 / (602/3600)
	c.HourlyUSD = 1.0 / (602.0 / 3600.0)
	f := &fake{}
	ctl := controller(f)
	ctl.Pack = func(ctx context.Context, inv []byte, cfg seaweed.CloudConfig) (string, error) {
		<-ctx.Done()
		return "", context.Cause(ctx)
	}
	start := time.Now()
	r := ctl.Execute(context.Background(), "run", c)
	if el := time.Since(start); el > 30*time.Second {
		t.Fatalf("the run took %v; the cost deadline should have cut it after ~2 s", el)
	}
	if r.Code != "COST_ABORTED" {
		t.Fatalf("code %s (%s), want COST_ABORTED", r.Code, r.Error)
	}
	if !strings.Contains(strings.Join(f.events, ","), "terraform destroy") {
		t.Fatalf("destroy did not run after the cost cut: %v", f.events)
	}
}
