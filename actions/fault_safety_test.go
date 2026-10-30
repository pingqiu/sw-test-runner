package actions

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tr "github.com/pingqiu/sw-test-runner"
)

func TestTierChaosNeverFallsBackToUnwitnessedCleanupStrings(t *testing.T) {
	r := tr.NewRegistry()
	RegisterFaultActions(r)
	for _, name := range []string{"inject_partition", "inject_netem", "fill_disk", "clear_fault"} {
		h, err := r.Get(name)
		if err != nil {
			t.Fatal(err)
		}
		_, err = h.Execute(context.Background(), &tr.ActionContext{Vars: map[string]string{"__cleanup_fill_disk": "true"}}, tr.Action{Action: name, Node: "fake", Params: map[string]string{"target_ip": "127.0.0.1", "ports": "80", "dir": "/tmp"}})
		if err == nil {
			t.Fatalf("%s ran without owned witnessed plan", name)
		}
	}
}

func TestTierChaosRefusesAnotherFaultKindBeforeObservation(t *testing.T) {
	r := tr.NewRegistry()
	RegisterFaultActions(r)
	for _, name := range []string{"inject_partition", "inject_netem", "fill_disk"} {
		t.Run(name, func(t *testing.T) {
			a := tr.RunAdmission{RunID: "kind-test", Workload: "kv", RequestID: "request", ManifestSHA256: strings.Repeat("a", 64), OutputDir: "/run/kind-test", WorkDeadline: time.Now().Add(time.Minute), HardDeadline: time.Now().Add(2 * time.Minute), CleanupReserve: time.Second}
			scope, err := tr.NewRunScope(t.Context(), a, &tr.SSHJobs{}, func(context.Context, tr.RunAdmission) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = scope.Close() })
			kind := "fill_disk"
			if name == "fill_disk" {
				kind = "partition"
			}
			observations := 0
			plan := tr.WitnessedFault{Kind: kind, Node: "fake", Resource: "owned", Before: map[string]string{"state": "before"}, During: map[string]string{"state": "during"}, Apply: tr.JobSpec{Node: "fake"}, Restore: tr.JobSpec{Node: "fake"}, Observe: func(context.Context) (tr.FaultObservation, error) {
				observations++
				return tr.FaultObservation{}, errors.New("mismatched plan must not be observed")
			}}
			h, err := r.Get(name)
			if err != nil {
				t.Fatal(err)
			}
			_, err = h.Execute(t.Context(), &tr.ActionContext{Safety: scope, FaultPlans: map[string]tr.WitnessedFault{"wrong-kind": plan}}, tr.Action{Action: name, Node: "fake", Params: map[string]string{"plan": "wrong-kind"}})
			if err == nil || observations != 0 {
				t.Fatal("action admitted another kind's fault plan")
			}
		})
	}
}
