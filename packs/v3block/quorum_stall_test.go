package v3block

import (
	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/actions"
	"github.com/pingqiu/sw-test-runner/packs/cloud"
	"reflect"
	"strings"
	"testing"
)

func TestQuorumStallUsesTheSameStandaloneStartAndFaultsOnlyAReplica(t *testing.T) {
	base, err := tr.ParseFile("../../scenarios/v3block/r4-node-loss.yaml")
	if err != nil {
		t.Fatal(err)
	}
	gate, err := tr.ParseFile("../../scenarios/v3block/quorum-stall.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"preflight", "build", "start"} {
		var a, b *tr.Phase
		for i := range base.Phases {
			if base.Phases[i].Name == name {
				a = &base.Phases[i]
			}
		}
		for i := range gate.Phases {
			if gate.Phases[i].Name == name {
				b = &gate.Phases[i]
			}
		}
		if a == nil || b == nil || !reflect.DeepEqual(a, b) {
			t.Fatalf("%s is not the shared R4 lifecycle", name)
		}
	}
	if gate.Env["write_budget_ms"] != "5000" || gate.Env["resume_budget_ms"] != "10000" || gate.Env["budget_ref"] != "ruling-6b0a1756d82cd3d6137887b9" {
		t.Fatal("approved gate policy drifted")
	}
	off, on := 0, 0
	for _, phase := range gate.Phases {
		for _, a := range phase.Actions {
			if a.Action == "cloud_vm_power" {
				off++
				if a.Params["vm"] != "node-3" {
					t.Fatal("fault must hit the replica, not the guest/primary")
				}
			}
			if a.Action == "cloud_vm_power_restore" {
				on++
			}
			if strings.Contains(a.Params["cmd"], "--allow-external-nvme-bind") {
				t.Fatal("standalone guest must stay loopback")
			}
		}
	}
	if off != 1 || on != 2 {
		t.Fatalf("fault/restore count %d/%d", off, on)
	}
}

func TestQuorumStallRegistersEveryActionAndVerifiesBeforeTimingPass(t *testing.T) {
	s, err := tr.ParseFile("../../scenarios/v3block/quorum-stall.yaml")
	if err != nil {
		t.Fatal(err)
	}
	r := tr.NewRegistry()
	actions.RegisterCore(r)
	actions.RegisterBlockActions(r)
	actions.RegisterNVMeActions(r)
	RegisterPack(r)
	cloud.RegisterPack(r)
	r.AllowMutating = true
	found := false
	restoredBeforeCollect := false
	for _, p := range s.Phases {
		for _, a := range p.Actions {
			if _, err := r.Get(a.Action); err != nil {
				t.Fatal(err)
			}
			if p.Name == "stop-and-verify-every-ack" {
				cmd := a.Params["cmd"]
				stop := strings.Index(cmd, " stop --work")
				verify := strings.Index(cmd, "r4.sh verify")
				drop := strings.Index(cmd, "r4.sh drop-control")
				judge := strings.Index(cmd, " judge --work")
				if stop < 0 || verify < stop || drop < verify || judge < drop || !strings.Contains(cmd, "--require-block-device") {
					t.Fatal("timing pass can bypass stopped writer or full data oracle")
				}
				found = true
			}
			if p.Name == "restore-before-evidence" {
				if !p.Always {
					t.Fatal("restoration must run on failure")
				}
				if a.Action == "cloud_vm_power_restore" {
					restoredBeforeCollect = true
				}
				if a.Action == "collect_path" && !restoredBeforeCollect {
					t.Fatal("evidence collected before replica restoration")
				}
			}
		}
	}
	if !found || !restoredBeforeCollect {
		t.Fatal("missing data verification or restoration")
	}
}
