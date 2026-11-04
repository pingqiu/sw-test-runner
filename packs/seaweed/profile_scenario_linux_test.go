//go:build linux

package seaweed

import (
	"fmt"
	tr "github.com/pingqiu/sw-test-runner"
	"gopkg.in/yaml.v3"
	"testing"
	"time"
)

// Exercise the producer's actual ScenarioYAML unconditionally. The separate
// bind-negative fixture intentionally constructs phase shapes to mutate them.
func TestBuildProfileEmittedSoakScenarioBinds(t *testing.T) {
	f := soakProfile(t, 45*time.Minute, nil, nil)
	if f.err != nil {
		t.Fatal(f.err)
	}
	bundle, err := BuildProfile(f.input)
	if err != nil {
		t.Fatal(err)
	}
	check := func(raw string) error {
		var s tr.Scenario
		if err := yaml.Unmarshal([]byte(raw), &s); err != nil {
			return err
		}
		want := []string{"seaweed_preflight", "seaweed_deploy", "seaweed_kv_soak", "seaweed_collect", "seaweed_cleanup"}
		if len(s.Phases) != len(want) {
			return fmt.Errorf("emitted phase count %d", len(s.Phases))
		}
		for i, p := range s.Phases {
			if p.Name != phases[i] || len(p.Actions) != 1 || p.Actions[0].Action != want[i] || p.Always != (i == 4) {
				return fmt.Errorf("emitted phase %d action/cleanup shape changed", i)
			}
		}
		if s.Phases[2].Actions[0].Timeout != "1990s" {
			return fmt.Errorf("emitted soak timeout %q, want 1990s", s.Phases[2].Actions[0].Timeout)
		}
		plan, d, err := decodePlan(s.Metadata["seaweed_plan"])
		if err != nil {
			return err
		}
		in := f.in
		in.Scenario = []byte(raw)
		in.Jobs = bundle.Jobs
		in.Restarts = bundle.Restarts
		return bind(plan, d, in, time.Now())
	}
	if err := check(bundle.ScenarioYAML); err != nil {
		t.Fatalf("actual emitted bundle: %v", err)
	}
	for name, mutate := range map[string]func(*tr.Scenario){
		"wrong-action":    func(s *tr.Scenario) { s.Phases[2].Actions[0].Action = "seaweed_workload" },
		"wrong-timeout":   func(s *tr.Scenario) { s.Phases[2].Actions[0].Timeout = "1989s" },
		"missing-cleanup": func(s *tr.Scenario) { s.Phases[4].Always = false },
	} {
		t.Run(name, func(t *testing.T) {
			var s tr.Scenario
			if err := yaml.Unmarshal([]byte(bundle.ScenarioYAML), &s); err != nil {
				t.Fatal(err)
			}
			mutate(&s)
			raw, err := yaml.Marshal(s)
			if err != nil {
				t.Fatal(err)
			}
			if err := check(string(raw)); err == nil {
				t.Fatal("mutated emitted bundle accepted")
			}
		})
	}
}
