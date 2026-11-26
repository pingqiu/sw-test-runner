package testrunner_test

import (
	"context"
	"fmt"
	tr "github.com/pingqiu/sw-test-runner"
	"strings"
	"sync"
	"testing"
	"time"
)

// Execute the committed YAML through the real engine with off-host handlers.
// It proves phase failure/cleanup wiring, not NVMe or systemd behavior.
func TestKnownZeroScenarioAlwaysSettlesBothHosts(t *testing.T) {
	for _, fault := range []string{"", "controller:contract", "source:start", "source:handoff", "controller:judge", "source:cleanup", "source:collect-reports"} {
		t.Run(fault, func(t *testing.T) {
			s, err := tr.ParseFile("scenarios/public/known-zero-1tib-acceptance.yaml")
			if err != nil {
				t.Fatal(err)
			}
			if s.AlwaysTimeout.Duration <= 0 {
				t.Fatal("missing cleanup budget")
			}
			s.Env["source_root"] = "/scratch/source"
			s.Env["target_root"] = "/scratch/target"
			var mu sync.Mutex
			counts := map[string]int{}
			handler := func(_ context.Context, _ *tr.ActionContext, a tr.Action) (map[string]string, error) {
				key := a.Action
				if a.Action == "collect_path" {
					key = "collect-reports"
					if strings.Contains(a.Params["name"], "closeout") {
						key = "collect-closeout"
					}
				}
				for _, verb := range []string{"prepare", "start", "handoff", "accept", "poll", "done", "cleanup"} {
					if strings.Contains(a.Params["cmd"], "host.py' "+verb+" ") || strings.HasSuffix(a.Params["cmd"], "host.py' "+verb) {
						key = verb
					}
				}
				if strings.Contains(a.Params["cmd"], "judge.py'") {
					key = "judge"
				}
				if strings.Contains(a.Params["cmd"], "contract.py'") {
					key = "contract"
				}
				key = a.Node + ":" + key
				mu.Lock()
				counts[key]++
				mu.Unlock()
				if key == fault {
					return nil, fmt.Errorf("injected %s", fault)
				}
				return map[string]string{"value": "HANDOFF e30="}, nil
			}
			r := tr.NewRegistry()
			for _, name := range []string{"exec", "upload_path", "wait_exec_match", "collect_path"} {
				r.RegisterFunc(name, tr.TierCore, handler)
			}
			a := &tr.ActionContext{Scenario: s, Vars: map[string]string{}, Log: func(string, ...interface{}) {}}
			result := tr.NewEngine(r, nil).Run(context.Background(), s, a)
			if (result.Status == tr.StatusPass) != (fault == "") {
				t.Fatalf("fault=%s status=%s error=%s", fault, result.Status, result.Error)
			}
			if fault == "controller:contract" && (counts["source:prepare"] != 0 || counts["source:start"] != 0 || counts["target:start"] != 0) {
				t.Fatalf("host preparation/launch ran after contract refusal: %v", counts)
			}
			for _, role := range []string{"source", "target"} {
				for _, verb := range []string{"cleanup", "collect-closeout"} {
					if counts[role+":"+verb] != 1 {
						t.Fatalf("missing always action: %s %s counts=%v", role, verb, counts)
					}
				}
			}
		})
	}
}

func TestKnownZeroScenarioResolvesLaunchInputs(t *testing.T) {
	s, err := tr.ParseFile("scenarios/public/known-zero-1tib-acceptance.yaml")
	if err != nil {
		t.Fatal(err)
	}
	const run = "kz-20261126t050000z"
	s.Env["source_root"] = "/scratch/source"
	s.Env["target_root"] = "/scratch/target"
	s.Env["kz_run"] = run
	s.Env["run_id"] = "runner-internal-bundle-id"
	s.Env["product_src"] = "/product"
	s.Env["rc_sha"] = strings.Repeat("a", 40)
	if s.Phases[0].Name != "product-contract" {
		t.Fatal("product contract must precede every host action")
	}
	var mu sync.Mutex
	seen := map[string]bool{}
	handler := func(_ context.Context, _ *tr.ActionContext, a tr.Action) (map[string]string, error) {
		key := ""
		want := ""
		got := ""
		if a.Node == "controller" && strings.Contains(a.Params["cmd"], "contract.py'") {
			if a.Params["env.KZ_PRODUCT_SRC"] != "/product" || a.Params["env.KZ_RC"] != strings.Repeat("a", 40) {
				return nil, fmt.Errorf("product contract inputs not resolved: %v", a.Params)
			}
		}
		if a.Node == "source" && strings.HasSuffix(a.Params["cmd"], "host.py' prepare") {
			key = "root"
			want = "/scratch/source"
			got = a.Params["env.KZ_ROOT"]
		}
		if a.Node == "target" && strings.HasSuffix(a.Params["cmd"], "host.py' accept") {
			key = "handoff"
			want = "HANDOFF e30="
			got = a.Params["env.KZ_HANDOFF_B64"]
		}
		if a.SaveAs == "cleanup_at" {
			return map[string]string{"value": "2099-01-01T00:00:00Z"}, nil
		}
		if a.Node == "source" && key == "root" && a.Params["env.KZ_DEADLINE"] != "2099-01-01T00:00:00Z" {
			return nil, fmt.Errorf("absolute cleanup deadline not propagated: %q", a.Params["env.KZ_DEADLINE"])
		}
		if a.Node == "controller" && strings.Contains(a.Params["cmd"], "judge.py'") {
			role := a.Params["env.KZ_ROLE"]
			key = "judge-" + role
			want = "/bundle/" + role + ".tgz"
			got = a.Params["env.KZ_ARCHIVE"]
			if a.Params["env.KZ_ACCEPTANCE_ID"] != run {
				return nil, fmt.Errorf("controller acceptance id=%q", a.Params["env.KZ_ACCEPTANCE_ID"])
			}
		}
		if key != "" {
			if got != want {
				return nil, fmt.Errorf("%s=%q want %q", key, got, want)
			}
			if a.Node != "controller" && a.Params["env.KZ_RUN"] != run {
				return nil, fmt.Errorf("KZ_RUN=%q; bundle id overwrote acceptance id", a.Params["env.KZ_RUN"])
			}
			mu.Lock()
			seen[key] = true
			mu.Unlock()
		}
		if a.Action == "collect_path" {
			return map[string]string{"value": "/bundle/" + a.Node + ".tgz"}, nil
		}
		return map[string]string{"value": "HANDOFF e30="}, nil
	}
	r := tr.NewRegistry()
	for _, name := range []string{"exec", "upload_path", "wait_exec_match", "collect_path"} {
		r.RegisterFunc(name, tr.TierCore, handler)
	}
	result := tr.NewEngine(r, nil).Run(context.Background(), s, &tr.ActionContext{Scenario: s, Vars: map[string]string{}, Log: func(string, ...interface{}) {}})
	if result.Status != tr.StatusPass || !seen["root"] || !seen["handoff"] || !seen["judge-source"] || !seen["judge-target"] {
		t.Fatalf("status=%s error=%s seen=%v", result.Status, result.Error, seen)
	}
}

func TestKnownZeroOrdinaryPhaseBudgetsFit(t *testing.T) {
	s, err := tr.ParseFile("scenarios/public/known-zero-1tib-acceptance.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var total time.Duration
	for _, p := range s.Phases {
		if p.Always {
			continue
		}
		// Deliberately sum even parallel actions: a conservative staging budget.
		for _, a := range p.Actions {
			d, err := time.ParseDuration(a.Timeout)
			if err != nil {
				t.Fatalf("%s action timeout %q: %v", p.Name, a.Timeout, err)
			}
			total += d
		}
	}
	if total > s.Timeout.Duration {
		t.Fatalf("ordinary phases %s exceed work budget %s", total, s.Timeout.Duration)
	}
}
