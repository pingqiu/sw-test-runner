package testrunner

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

const powerVM = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg-run/providers/Microsoft.Compute/virtualMachines/run-n2"

func powerScope(t *testing.T) *RunScope {
	t.Helper()
	a := RunAdmission{RunID: "power-test", Workload: "kv", RequestID: "request", ManifestSHA256: strings.Repeat("a", 64), OutputDir: "/run/power-test",
		WorkDeadline: time.Now().Add(time.Minute), HardDeadline: time.Now().Add(2 * time.Minute), CleanupReserve: 30 * time.Second}
	s, err := NewRunScope(t.Context(), a, &SSHJobs{}, func(context.Context, RunAdmission) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// fakeVM is a VM whose power, identity and boot the test steers.
type fakeVM struct {
	mu                sync.Mutex
	power, vmID, boot string
	reachable         bool
	offs, ons         int
	// hooks run after a power call to model what the cloud did.
	afterOff, afterOn func(*fakeVM)
}

func (v *fakeVM) plan() WitnessedPowerFault {
	return WitnessedPowerFault{
		Node: "n2", Resource: powerVM, Owned: []string{strings.ToLower(powerVM)}, RestoreTimeout: MinPowerRestoreTimeout,
		PowerOff: func(context.Context) error {
			v.mu.Lock()
			defer v.mu.Unlock()
			v.offs++
			v.power, v.reachable = "stopped", false
			if v.afterOff != nil {
				v.afterOff(v)
			}
			return nil
		},
		PowerOn: func(context.Context) error {
			v.mu.Lock()
			defer v.mu.Unlock()
			v.ons++
			v.power, v.reachable, v.boot = "running", true, "boot-2"
			if v.afterOn != nil {
				v.afterOn(v)
			}
			return nil
		},
		View: func(context.Context) (VMPowerView, error) {
			v.mu.Lock()
			defer v.mu.Unlock()
			return VMPowerView{VMID: v.vmID, PowerState: v.power}, nil
		},
		BootID: func(context.Context) (string, error) {
			v.mu.Lock()
			defer v.mu.Unlock()
			if !v.reachable {
				return "", errors.New("unreachable")
			}
			return v.boot, nil
		},
	}
}

func newFakeVM() *fakeVM {
	return &fakeVM{power: "running", vmID: "vmid-1", boot: "boot-1", reachable: true}
}

// The power fault: off with the node unreachable, then at close powered on
// and proven by a new boot read from the node.
func TestPowerFaultOffThenRestoredWithANewBoot(t *testing.T) {
	s := powerScope(t)
	v := newFakeVM()
	p := v.plan()
	if err := s.ApplyWitnessedPowerFault(t.Context(), p.Key(), p); err != nil {
		t.Fatalf("apply: %v", err)
	}
	r := s.FaultReceipts()[p.Key()]
	if r.Kind != "vm_power" || r.Before.BootID != "boot-1" || r.Applied.BootID != "" || r.Applied.State["power"] != "stopped" || r.Applied.State["node"] != "unreachable" {
		t.Fatalf("applied receipt %+v", r)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	r = s.FaultReceipts()[p.Key()]
	if v.ons != 1 || r.Restored.State["power"] != "running" || r.Restored.BootID != "boot-2" || r.RestoreNotNeeded {
		t.Fatalf("restored receipt %+v (ons=%d)", r, v.ons)
	}
}

// Every refusal comes before any call: a VM the run does not own, a
// resource that is not a VM, a restore bound below a real start, an
// incomplete plan, a key that is not the plan's.
func TestPowerFaultRefusesBeforeAnyCall(t *testing.T) {
	for name, mutate := range map[string]func(*WitnessedPowerFault){
		"not owned": func(p *WitnessedPowerFault) {
			p.Owned = []string{"/subscriptions/x/resourceGroups/other/providers/Microsoft.Compute/virtualMachines/run-n2"}
		},
		"not a VM": func(p *WitnessedPowerFault) {
			p.Resource = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg-run/providers/Microsoft.Network/publicIPAddresses/run-n2-ip"
			p.Owned = []string{p.Resource}
		},
		"short restore":  func(p *WitnessedPowerFault) { p.RestoreTimeout = time.Minute },
		"no power-on":    func(p *WitnessedPowerFault) { p.PowerOn = nil },
		"no boot reader": func(p *WitnessedPowerFault) { p.BootID = nil },
		"no node":        func(p *WitnessedPowerFault) { p.Node = "" },
	} {
		t.Run(name, func(t *testing.T) {
			s := powerScope(t)
			t.Cleanup(func() { _ = s.Close() })
			v := newFakeVM()
			p := v.plan()
			mutate(&p)
			if err := s.ApplyWitnessedPowerFault(t.Context(), p.Key(), p); err == nil {
				t.Fatal("refused plan was applied")
			}
			if v.offs != 0 || len(s.FaultReceipts()) != 0 {
				t.Fatalf("a refused plan touched the VM (offs=%d receipts=%d)", v.offs, len(s.FaultReceipts()))
			}
		})
	}
	s := powerScope(t)
	t.Cleanup(func() { _ = s.Close() })
	v := newFakeVM()
	p := v.plan()
	if err := s.ApplyWitnessedPowerFault(t.Context(), "n2/another", p); err == nil || v.offs != 0 {
		t.Fatal("a key that is not the plan's was applied")
	}
}

// A VM whose vmId changes (re-created under the same name) is refused: the
// observation identity is the ARM id and the vmId.
func TestPowerFaultRefusesAChangedVMID(t *testing.T) {
	s := powerScope(t)
	t.Cleanup(func() { _ = s.Close() })
	v := newFakeVM()
	v.afterOff = func(v *fakeVM) { v.vmID = "vmid-recreated" }
	p := v.plan()
	if err := s.ApplyWitnessedPowerFault(t.Context(), p.Key(), p); err == nil || !strings.Contains(err.Error(), "vmId") {
		t.Fatalf("err=%v; want a refusal naming the vmId", err)
	}
}

// Off but the node still answers: the effect is NOT_PROVEN.
func TestPowerFaultNotProvenWhileTheNodeAnswers(t *testing.T) {
	s := powerScope(t)
	t.Cleanup(func() { _ = s.Close() })
	v := newFakeVM()
	v.afterOff = func(v *fakeVM) { v.reachable = true }
	p := v.plan()
	if err := s.ApplyWitnessedPowerFault(t.Context(), p.Key(), p); err == nil || !strings.Contains(err.Error(), "NOT_PROVEN") {
		t.Fatalf("err=%v; want NOT_PROVEN", err)
	}
}

// Restored with the OLD boot (the VM never really went down, or the read is
// stale) is NOT_PROVEN at close.
func TestPowerFaultRestoredWithTheOldBootIsNotProven(t *testing.T) {
	s := powerScope(t)
	v := newFakeVM()
	v.afterOn = func(v *fakeVM) { v.boot = "boot-1" }
	p := v.plan()
	if err := s.ApplyWitnessedPowerFault(t.Context(), p.Key(), p); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if err := s.Close(); err == nil || !strings.Contains(err.Error(), "NOT_PROVEN") {
		t.Fatalf("close err=%v; want restoration NOT_PROVEN", err)
	}
}

// At cleanup a VM already running with a new boot needs no power-on.
func TestPowerFaultAlreadyBackNeedsNoRestore(t *testing.T) {
	s := powerScope(t)
	v := newFakeVM()
	p := v.plan()
	if err := s.ApplyWitnessedPowerFault(t.Context(), p.Key(), p); err != nil {
		t.Fatalf("apply: %v", err)
	}
	v.mu.Lock()
	v.power, v.reachable, v.boot = "running", true, "boot-3" // started by someone else
	v.mu.Unlock()
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if r := s.FaultReceipts()[p.Key()]; v.ons != 0 || !r.RestoreNotNeeded || r.Restored.BootID != "boot-3" {
		t.Fatalf("ons=%d receipt=%+v; want no power-on, restore not needed, new boot", v.ons, r)
	}
}
