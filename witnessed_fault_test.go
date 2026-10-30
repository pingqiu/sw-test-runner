package testrunner

import (
	"context"
	"errors"
	"sync"
	"testing"
)

type faultFake struct {
	*fakeOwnedNode
	stateMu                                                sync.Mutex
	state                                                  string
	noApply, noRestore, badBoot, probeError, unknownIssuer bool
	applyFailure                                           bool
	jobBoot                                                string
	restores                                               int
}

func (n *faultFake) Enroll(ctx context.Context, id string, a RunAdmission, spec JobSpec) (ProcessIdentity, error) {
	i, err := n.fakeOwnedNode.Enroll(ctx, id, a, spec)
	if spec.Argv[0] == n.jobBoot {
		i.BootID = "other-boot"
		n.mu.Lock()
		n.ids[id] = i
		n.mu.Unlock()
	}
	return i, err
}

func (n *faultFake) Authorize(ctx context.Context, id string, i ProcessIdentity) error {
	if err := n.fakeOwnedNode.Authorize(ctx, id, i); err != nil {
		return err
	}
	n.stateMu.Lock()
	defer n.stateMu.Unlock()
	if n.specs[id].Argv[0] == "apply" && !n.noApply {
		n.state = "on"
	}
	if n.specs[id].Argv[0] == "restore" {
		n.restores++
		if !n.noRestore {
			n.state = "off"
		}
	}
	return nil
}

func (n *faultFake) Wait(ctx context.Context, id string) (ExitInfo, error) {
	if n.unknownIssuer && n.specs[id].Argv[0] == "apply" {
		return ExitInfo{}, errors.New("lost reply")
	}
	x, err := n.fakeOwnedNode.Wait(ctx, id)
	if n.applyFailure && n.specs[id].Argv[0] == "apply" {
		x.Code = 7
	}
	return x, err
}

func (n *faultFake) Settle(ctx context.Context, id string) error {
	if n.unknownIssuer && n.specs[id].Argv[0] == "apply" {
		return errors.New("issuer UNKNOWN")
	}
	return n.fakeOwnedNode.Settle(ctx, id)
}

func faultFixture(t *testing.T) (*RunScope, *faultFake, WitnessedFault) {
	t.Helper()
	n := &faultFake{fakeOwnedNode: &fakeOwnedNode{ids: map[string]ProcessIdentity{}, specs: map[string]JobSpec{}}, state: "off"}
	s, err := NewRunScope(t.Context(), testAdmission(), n, func(context.Context, RunAdmission) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	apply, restore := testJob(), testJob()
	apply.Argv, restore.Argv = []string{"apply"}, []string{"restore"}
	p := WitnessedFault{Kind: "partition", Node: "fake", Resource: "owned-chain", Before: map[string]string{"rule": "off"}, During: map[string]string{"rule": "on"}, Apply: apply, Restore: restore}
	p.Observe = func(context.Context) (FaultObservation, error) {
		n.stateMu.Lock()
		defer n.stateMu.Unlock()
		if n.probeError {
			return FaultObservation{}, errors.New("enumeration failed")
		}
		boot := "boot"
		if n.badBoot {
			boot = "replacement"
		}
		return FaultObservation{Node: "fake", Resource: p.Resource, BootID: boot, State: map[string]string{"rule": n.state}}, nil
	}
	return s, n, p
}

func TestWitnessedFaultRestoresAfterClosedAdmissionAndKeepsBoundReceipts(t *testing.T) {
	s, n, p := faultFixture(t)
	if err := s.ApplyWitnessedFault(t.Context(), p.Key(), p); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if n.restores != 1 || n.state != "off" {
		t.Fatal("resource not restored")
	}
	for _, spec := range n.specs {
		if spec.Argv[0] == "restore" && !spec.Restoration {
			t.Fatal("restore used ordinary work admission")
		}
	}
	p.Apply.Restoration = true
	if _, err := s.Start(p.Apply); err == nil {
		t.Fatal("caller revived work after close with restoration bit")
	}
	r := s.FaultReceipts()[p.Key()]
	if r.Before.State["rule"] != "off" || r.Applied.State["rule"] != "on" || r.Restored.State["rule"] != "off" || r.Restored.ReadStart.Before(r.Applied.ReadEnd) {
		t.Fatalf("unbound evidence %+v", r)
	}
	r.Restored.State["rule"] = "forged"
	if s.FaultReceipts()[p.Key()].Restored.State["rule"] != "off" {
		t.Fatal("receipt mutation escaped snapshot")
	}
}

func TestWitnessedFaultRefusesFalseEffectFalseRestoreAndChangedGeneration(t *testing.T) {
	for _, which := range []string{"effect", "restore", "boot", "probe"} {
		t.Run(which, func(t *testing.T) {
			s, n, p := faultFixture(t)
			n.noApply = which == "effect"
			n.probeError = which == "probe"
			err := s.ApplyWitnessedFault(t.Context(), p.Key(), p)
			if (which == "effect" || which == "probe") && err == nil {
				t.Fatal("false effect/probe accepted")
			}
			n.noRestore = which == "restore"
			n.badBoot = which == "boot"
			closeErr := s.Close()
			if closeErr == nil {
				t.Fatal("cleanup lacked an exact restoration proof")
			}
		})
	}
}

func TestWitnessedFaultPartialApplyStillRestoresAndUnknownIssuerCannotRaceRestore(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(map[bool]string{false: "partial", true: "unknown"}[unknown], func(t *testing.T) {
			s, n, p := faultFixture(t)
			n.applyFailure, n.unknownIssuer = true, unknown
			if err := s.ApplyWitnessedFault(t.Context(), p.Key(), p); err == nil {
				t.Fatal("failed apply accepted")
			}
			err := s.Close()
			if unknown {
				if err == nil || n.restores != 0 {
					t.Fatal("restoration raced a possible issuer")
				}
			} else if err == nil || n.restores != 1 || n.state != "off" {
				t.Fatalf("partial mutation was not restored: %v", err)
			}
		})
	}
}

func TestWitnessedFaultSameResourceCannotAcquireAnotherKindAndFailedRestoreIsProbed(t *testing.T) {
	s, n, p := faultFixture(t)
	if err := s.ApplyWitnessedFault(t.Context(), p.Key(), p); err != nil {
		t.Fatal(err)
	}
	p.Kind = "netem"
	if err := s.ApplyWitnessedFault(t.Context(), p.Key(), p); err == nil {
		t.Fatal("resource alias bypassed custody")
	}
	key := p.Key()
	s.restores[key].restore = func(context.Context) error { return errors.New("restore transport failed") }
	if err := s.Close(); err == nil {
		t.Fatal("failed restore qualified")
	}
	if s.FaultReceipts()[key].Restored.ReadEnd.IsZero() || n.state != "on" {
		t.Fatal("failed restore was not followed by an observation")
	}
}

func TestWitnessedFaultIgnoredApplyFailureStillFailsEngine(t *testing.T) {
	s, n, p := faultFixture(t)
	n.noApply = true
	r := NewRegistry()
	r.RegisterFunc("fake_fault", TierChaos, func(ctx context.Context, _ *ActionContext, _ Action) (map[string]string, error) {
		return nil, s.ApplyWitnessedFault(ctx, p.Key(), p)
	})
	scenario := &Scenario{Name: "ignored-fault", Phases: []Phase{{Name: "body", Actions: []Action{{Action: "fake_fault", IgnoreError: true}}}}}
	result := NewEngine(r, nil).Run(t.Context(), scenario, &ActionContext{Safety: s})
	if result.Status != StatusFail {
		t.Fatalf("unwitnessed ignored fault qualified: %+v", result)
	}
}

func TestWitnessedFaultCancelledActionNeverAuthorizes(t *testing.T) {
	s, n, p := faultFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := s.ApplyWitnessedFault(ctx, p.Key(), p); err == nil {
		t.Fatal("cancelled action admitted")
	}
	if n.authorized != 0 {
		t.Fatal("cancelled fault sent a mutation")
	}
}

func TestWitnessedFaultRefusesUnreviewedBaselineBeforeDispatch(t *testing.T) {
	s, n, p := faultFixture(t)
	n.state = "on"
	if err := s.ApplyWitnessedFault(t.Context(), p.Key(), p); err == nil || n.authorized != 0 {
		t.Fatal("pre-faulted resource admitted a mutation")
	}
	if err := s.Close(); err == nil || n.restores != 0 {
		t.Fatal("unreviewed baseline qualified or caused a restore")
	}
}

func TestWitnessedFaultRefusesUnreviewedRestoreValueBeforeDispatch(t *testing.T) {
	s, n, p := faultFixture(t)
	if err := s.ApplyWitnessedFault(t.Context(), p.Key(), p); err != nil {
		t.Fatal(err)
	}
	n.state = "unreviewed"
	if err := s.Close(); err == nil || n.restores != 0 {
		t.Fatal("restore dispatched against an unreviewed value")
	}
	if s.FaultReceipts()[p.Key()].Restored.State["rule"] != "unreviewed" {
		t.Fatal("refused restore lacked a final observation")
	}
}

func TestWitnessedFaultJoinsEachCommandBootToTheObservation(t *testing.T) {
	for _, command := range []string{"apply", "restore"} {
		t.Run(command, func(t *testing.T) {
			s, n, p := faultFixture(t)
			n.jobBoot = command
			err := s.ApplyWitnessedFault(t.Context(), p.Key(), p)
			if (command == "apply") != (err != nil) {
				t.Fatalf("apply boot join: %v", err)
			}
			if err := s.Close(); err == nil {
				t.Fatal("a command from another boot qualified restoration")
			}
		})
	}
}
