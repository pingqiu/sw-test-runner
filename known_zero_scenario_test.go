package testrunner_test

import (
	"context"
	"fmt"
	tr "github.com/pingqiu/sw-test-runner"
	"strings"
	"sync"
	"testing"
)

// Execute the committed YAML through the real engine with off-host handlers.
// It proves phase failure/cleanup wiring, not NVMe or systemd behavior.
func TestKnownZeroScenarioAlwaysSettlesBothHosts(t *testing.T) {
	for _, fault := range []string{"", "source:start", "source:handoff", "target:judge", "source:cleanup", "source:collect_path"} {
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
				for _, verb := range []string{"prepare", "start", "handoff", "accept", "poll", "done", "cleanup"} {
					if strings.Contains(a.Params["cmd"], "host.py' "+verb+" ") || strings.HasSuffix(a.Params["cmd"], "host.py' "+verb) {
						key = verb
					}
				}
				if strings.Contains(a.Params["cmd"], "judge.py'") {
					key = "judge"
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
			for _, role := range []string{"source", "target"} {
				for _, verb := range []string{"cleanup", "collect_path"} {
					if counts[role+":"+verb] != 1 {
						t.Fatalf("missing always action: %s %s counts=%v", role, verb, counts)
					}
				}
			}
		})
	}
}
