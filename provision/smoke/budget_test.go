package smoke

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// budgetConfig decodes the fixture config plus extra budget keys through the
// same strict decoder the command uses, so a key the controller cannot express
// fails here.
func budgetConfig(t *testing.T, c Config, extra string) Config {
	t.Helper()
	b, _ := json.Marshal(c)
	if extra != "" {
		b = append(bytes.TrimSuffix(b, []byte("}")), []byte(","+extra+"}")...)
	}
	var out Config
	if e := StrictJSON(b, &out); e != nil {
		t.Fatalf("config cannot express budget keys: %v", e)
	}
	return out
}

// declare rewrites the pinned tfvars run budget and abort estimate.
func declare(t *testing.T, c Config, budget, abort float64) Config {
	t.Helper()
	b, _ := os.ReadFile(c.Variables)
	var v map[string]any
	_ = json.Unmarshal(b, &v)
	r := v["run_declaration"].(map[string]any)
	r["budget_usd"], r["abort_estimate_usd"] = budget, abort
	b, _ = json.Marshal(v)
	if e := os.WriteFile(c.Variables, b, 0600); e != nil {
		t.Fatal(e)
	}
	c.VariablesSHA256 = digest(b)
	return c
}

const capsule = `"project":"capsule","budgets":{"capsule":{"per_run_cap_usd":200,"cumulative_cap_usd":1000}}`
const override = `{"message_id":"chat-6aef0001","amount_usd":250,"approver":"owner","message":"approve one 250 USD run"}`

func planBudget(t *testing.T, r Result) map[string]any {
	t.Helper()
	b, ok := r.Plan["budget"].(map[string]any)
	if r.Code != "PLAN_ONLY" || !ok {
		t.Fatalf("no budget evidence in plan: %+v", r)
	}
	return b
}

// Default project = today's caps exactly: $3 per run, $10 cumulative.
func TestBudgetDefaultProjectIsTodaysCaps(t *testing.T) {
	cases := []struct {
		name          string
		budget, abort float64
		est, prior    float64
		ok            bool
	}{
		{"fixture", 3, 2.5, .12, 0, true},
		{"cumulative-at-10", 3, 2.5, .12, 9.88, true},
		{"cumulative-over-10", 3, 2.5, .12, 9.89, false},
		{"run-over-3", 3.01, 2.5, .12, 0, false},
		{"abort-over-budget", 3, 3.01, .12, 0, false},
		{"estimate-over-abort", 3, 2.5, 2.51, 0, false},
		{"negative-prior", 3, 2.5, .12, -1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := declare(t, budgetConfig(t, fixture(t), ""), tc.budget, tc.abort)
			c.EstimatedUSD, c.PriorUSD = tc.est, tc.prior
			f := &fake{}
			r := controller(f).Execute(context.Background(), "plan", c)
			if len(f.events) != 0 || (r.Code == "PLAN_ONLY") != tc.ok {
				t.Fatalf("%+v", r)
			}
			if tc.ok {
				b := planBudget(t, r)
				if b["project"] != "default" || b["per_run_cap_usd"] != 3.0 || b["cumulative_cap_usd"] != 10.0 || b["override"] != nil {
					t.Fatalf("%+v", b)
				}
			}
		})
	}
}

func TestBudgetProjectCapAdmitsLargerRun(t *testing.T) {
	c := declare(t, budgetConfig(t, fixture(t), capsule), 150, 140)
	c.EstimatedUSD, c.PriorUSD = 120, 500
	f := &fake{}
	r := controller(f).Execute(context.Background(), "plan", c)
	b := planBudget(t, r)
	if b["project"] != "capsule" || b["per_run_cap_usd"] != 200.0 || b["cumulative_cap_usd"] != 1000.0 || r.CumulativeUSD != 620 {
		t.Fatalf("%+v %+v", r, b)
	}
	if r = controller(f).Execute(context.Background(), "run", c); r.Code != "COMPLETE" {
		t.Fatalf("%+v", r)
	}
}

func TestBudgetOverrideRaisesRunCapAndIsCopiedVerbatim(t *testing.T) {
	base := declare(t, budgetConfig(t, fixture(t), capsule), 250, 240)
	base.EstimatedUSD = 230
	f := &fake{}
	if r := controller(f).Execute(context.Background(), "plan", base); r.Code != "INPUT_REFUSED" || !strings.Contains(r.Error, "per-run cap") {
		t.Fatalf("over project cap without override: %+v", r)
	}
	c := budgetConfig(t, base, `"budget_override":`+override)
	r := controller(f).Execute(context.Background(), "plan", c)
	b := planBudget(t, r)
	raw, _ := json.Marshal(b["override"])
	if b["per_run_cap_usd"] != 250.0 || b["project_per_run_cap_usd"] != 200.0 || string(raw) != override || len(f.events) != 0 {
		t.Fatalf("%+v", b)
	}
	if r = controller(f).Execute(context.Background(), "run", c); r.Code != "COMPLETE" {
		t.Fatalf("%+v", r)
	}
	saved, e := os.ReadFile(filepath.Join(c.Output, "budget.json"))
	if e != nil || !bytes.Contains(saved, []byte(override)) {
		t.Fatalf("override not saved verbatim: %s %v", saved, e)
	}
}

func TestBudgetRefusalsBeforeAnyCloudCall(t *testing.T) {
	cases := []struct {
		name, extra        string
		budget, abort, est float64
		prior              float64
		want               string
	}{
		{"unknown-project", `"project":"nope","budgets":{"capsule":{"per_run_cap_usd":200,"cumulative_cap_usd":1000}}`, 3, 2.5, .12, 0, "unknown budget project"},
		{"bad-project-name", `"project":"cap_sule","budgets":{"cap_sule":{"per_run_cap_usd":200,"cumulative_cap_usd":1000}}`, 3, 2.5, .12, 0, "budget project name"},
		{"redefine-default", `"project":"default","budgets":{"default":{"per_run_cap_usd":200,"cumulative_cap_usd":1000}}`, 3, 2.5, .12, 0, "default budget is built in"},
		{"zero-run-cap", `"project":"capsule","budgets":{"capsule":{"per_run_cap_usd":0,"cumulative_cap_usd":1000}}`, 3, 2.5, .12, 0, "caps must be positive"},
		{"run-cap-over-cumulative", `"project":"capsule","budgets":{"capsule":{"per_run_cap_usd":200,"cumulative_cap_usd":100}}`, 3, 2.5, .12, 0, "per-run cap exceeds cumulative cap"},
		{"over-project-run-cap", capsule, 201, 200, 150, 0, "budget_usd exceeds per-run cap"},
		{"over-project-cumulative", capsule, 200, 190, 150, 900, "exceeds cumulative cap"},
		{"override-no-id", capsule + `,"budget_override":{"amount_usd":250}`, 250, 240, 230, 0, "override needs message_id"},
		{"override-bad-id", capsule + `,"budget_override":{"message_id":"a b;c","amount_usd":250}`, 250, 240, 230, 0, "override needs message_id"},
		{"override-no-amount", capsule + `,"budget_override":{"message_id":"chat-1"}`, 250, 240, 230, 0, "override amount_usd"},
		{"override-negative", capsule + `,"budget_override":{"message_id":"chat-1","amount_usd":-5}`, 250, 240, 230, 0, "override amount_usd"},
		{"override-not-raising", capsule + `,"budget_override":{"message_id":"chat-1","amount_usd":150}`, 150, 140, 130, 0, "override must raise"},
		{"override-unknown-key", capsule + `,"budget_override":{"message_id":"chat-1","amount_usd":250,"cumulative_usd":5000}`, 250, 240, 230, 0, "malformed budget override"},
		{"override-string-amount", capsule + `,"budget_override":{"message_id":"chat-1","amount_usd":"250"}`, 250, 240, 230, 0, "malformed budget override"},
		{"override-not-object", capsule + `,"budget_override":"yes"`, 250, 240, 230, 0, "malformed budget override"},
		{"over-override-amount", capsule + `,"budget_override":` + override, 260, 240, 230, 0, "budget_usd exceeds per-run cap"},
		{"override-over-cumulative", capsule + `,"budget_override":` + override, 250, 240, 230, 800, "exceeds cumulative cap"},
		{"override-default-over-cumulative", `"budget_override":{"message_id":"chat-1","amount_usd":50}`, 50, 40, 30, 0, "exceeds cumulative cap"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := declare(t, budgetConfig(t, fixture(t), tc.extra), tc.budget, tc.abort)
			c.EstimatedUSD, c.PriorUSD = tc.est, tc.prior
			for _, mode := range []string{"plan", "run"} {
				f := &fake{}
				r := controller(f).Execute(context.Background(), mode, c)
				if r.Code != "INPUT_REFUSED" || len(f.events) != 0 || !strings.Contains(r.Error, tc.want) {
					t.Fatalf("%s: want %q, got %+v events=%v", mode, tc.want, r, f.events)
				}
				if _, e := os.Stat(c.Output); !os.IsNotExist(e) {
					t.Fatal("refused run wrote output")
				}
			}
		})
	}
}

// Region, size and the ten-node layout are unchanged under a project budget.
func TestBudgetProjectKeepsRegionSizeAndCluster(t *testing.T) {
	c := declare(t, budgetConfig(t, clusterFixture(t), capsule), 150, 140)
	c.EstimatedUSD = 100
	f := &fake{}
	if r := controller(f).Execute(context.Background(), "plan", c); r.Code != "PLAN_ONLY" || r.Plan["vm_count"] != 10 {
		t.Fatalf("%+v", r)
	}
	for _, kv := range [][2]string{{"location", "eastus"}, {"vm_size", "Standard_D4s_v5"}} {
		b, _ := os.ReadFile(c.Variables)
		var v map[string]any
		_ = json.Unmarshal(b, &v)
		v[kv[0]] = kv[1]
		b, _ = json.Marshal(v)
		p := filepath.Join(t.TempDir(), "vars.json")
		_ = os.WriteFile(p, b, 0600)
		d := c
		d.Variables, d.VariablesSHA256 = p, digest(b)
		if r := controller(f).Execute(context.Background(), "plan", d); r.Code != "INPUT_REFUSED" || len(f.events) != 0 {
			t.Fatalf("%s=%s admitted: %+v", kv[0], kv[1], r)
		}
	}
}

// Review P2 on 6769ba0: the approval message must reach the evidence byte for
// byte; json.Marshal and a default Encoder rewrite <, > and & as < etc.
func TestBudgetOverrideHTMLCharsVerbatim(t *testing.T) {
	const ov = `{"message_id":"chat-6aef0002","amount_usd":250,"approver":"owner","message":"ok <b>250</b> & no more"}`
	base := declare(t, budgetConfig(t, fixture(t), capsule), 250, 240)
	base.EstimatedUSD = 230
	c := budgetConfig(t, base, `"budget_override":`+ov)
	f := &fake{}
	r := controller(f).Execute(context.Background(), "plan", c)
	var out bytes.Buffer
	if e := EncodeVerbatim(&out, r); e != nil || !bytes.Contains(out.Bytes(), []byte(ov)) {
		t.Fatalf("plan output not verbatim: %s %v", out.Bytes(), e)
	}
	if r = controller(f).Execute(context.Background(), "run", c); r.Code != "COMPLETE" {
		t.Fatalf("%+v", r)
	}
	saved, e := os.ReadFile(filepath.Join(c.Output, "budget.json"))
	if e != nil || !bytes.Contains(saved, []byte(ov)) || bytes.Contains(saved, []byte("\\u003c")) {
		t.Fatalf("budget.json not verbatim: %s %v", saved, e)
	}
}
