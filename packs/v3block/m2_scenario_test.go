package v3block

import (
	"slices"
	"strings"
	"testing"

	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/actions"
)

const m2Scenario = "../../scenarios/v3block/m2-faults.yaml"

// The M2 scenario is M1's with the fault schedule on: same phases and steps,
// every action registered, v3_gen_run given the schedule's params, and a
// default schedule the generator accepts.
func TestM2ScenarioIsM1WithTheFaultSchedule(t *testing.T) {
	m1, err := tr.ParseFile(m1Scenario)
	if err != nil {
		t.Fatal(err)
	}
	m2, err := tr.ParseFile(m2Scenario)
	if err != nil {
		t.Fatal(err)
	}
	r := tr.NewRegistry()
	actions.RegisterSystemActions(r)
	RegisterPack(r)
	names := func(s *tr.Scenario) (out []string) {
		for _, p := range s.Phases {
			for _, a := range p.Actions {
				if !r.Has(a.Action) {
					t.Errorf("%s phase %s: action %s is not registered", s.Name, p.Name, a.Action)
				}
				out = append(out, p.Name+"/"+a.Action)
			}
		}
		return out
	}
	if a, b := names(m1), names(m2); !slices.Equal(a, b) {
		t.Fatalf("M2 steps %v differ from M1's %v", b, a)
	}
	var gen tr.Action
	for _, p := range m2.Phases {
		for _, a := range p.Actions {
			if a.Action == "v3_gen_run" {
				gen = a
			}
		}
	}
	for _, k := range []string{"faults", "fault_processes", "fault_restart_within", "fault_healthy_wait", "fault_master_outage"} {
		if gen.Params[k] != "{{ "+k+" }}" {
			t.Errorf("v3_gen_run param %s = %q", k, gen.Params[k])
		}
		if m2.Env[k] == "" {
			t.Errorf("env %s has no default", k)
		}
		if _, ok := m1.Env[k]; ok {
			t.Errorf("M1 sets %s: M1 runs without faults", k)
		}
	}
	if !strings.HasPrefix(m2.Env["work_prefix"], "/root/m2-run-") || m2.Env["work_prefix"] == m1.Env["work_prefix"] {
		t.Errorf("work_prefix %q: M1 and M2 runs need distinct work directories", m2.Env["work_prefix"])
	}
	procs := strings.Fields(m2.Env["fault_processes"])
	if _, err := NewFaultSchedule(1, 300, procs, 0.05, 20); err != nil {
		t.Errorf("the default schedule is refused: %v", err)
	}
}
