//go:build linux

package testrunner

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"testing"
)

func TestReviewerPauseUnknownBarrierMustCoverActualAlwaysClear(t *testing.T) {
	f, n := pauseFixture(t)
	backend := &pauseUnknownSettlement{pauseNode: n}
	f.runtime.Jobs = backend
	raw := []byte("name: pause-always\nphases:\n  - name: work\n    actions:\n      - action: apply\n  - name: cleanup\n    always: true\n    actions:\n      - action: clear\n")
	if err := os.WriteFile(f.binding.Scenario.Path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	f.binding.Scenario.SHA256 = fmt.Sprintf("%x", sha256.Sum256(raw))
	f.publish(t, nil)
	setGenerationConsent(t, f)
	policy, err := ReadTrustedPolicy(f.profile, uint32(os.Getuid()))
	if err != nil {
		t.Fatal(err)
	}
	run, err := OpenApprovedRun(context.Background(), f.request, policy, &f.runtime)
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close()
	restored, alwaysRan := false, false
	r := NewRegistry()
	r.RegisterMutatingFunc("apply", TierChaos, func(ctx context.Context, a *ActionContext, _ Action) (map[string]string, error) {
		victim, _, e := a.RunApprovedPackJob(ctx, "volume0", true)
		if e != nil {
			return nil, e
		}
		backend.victim = victim.id
		e = a.Safety.ApplyFault("older-generic", func(context.Context) error { return nil }, func(context.Context) error { restored = true; return nil }, func(context.Context) error { return nil })
		if e != nil {
			return nil, e
		}
		_, e = a.PauseApprovedPackGeneration(ctx, "freeze", victim)
		return nil, e
	})
	r.RegisterMutatingFunc("clear", TierChaos, func(ctx context.Context, a *ActionContext, _ Action) (map[string]string, error) {
		alwaysRan = true
		e := a.Safety.ClearFault(ctx, "older-generic")
		t.Logf("always_clear_error=%v older_restored=%v unknown_settlements=%d", e, restored, backend.failures)
		return nil, e
	})
	result := run.Execute(context.Background(), NewEngine(r, nil), &ActionContext{})
	t.Logf("actual_always_ran=%v older_restored=%v final_status=%s resources_settled=%v", alwaysRan, restored, result.Status, run.resourcesSettled())
	if !alwaysRan || backend.failures == 0 {
		t.Fatal("control did not reach actual always after UNKNOWN victim settlement")
	}
	if restored {
		t.Error("public always ClearFault bypassed framework UNKNOWN withholding and restored beside possibly live issuer")
	}
	if result.Status != StatusFail || run.resourcesSettled() {
		t.Error("UNKNOWN settlement incorrectly qualified")
	}
}

func TestPauseLatePublicClearRefusesBeforeChangingProof(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(fmt.Sprint(failed), func(t *testing.T) {
			f, n := pauseFixture(t)
			if failed {
				n.mode = "cont-failure"
			}
			run, _ := runGenerationCase(t, f, func(ctx context.Context, a *ActionContext) error {
				victim, _, err := a.RunApprovedPackJob(ctx, "volume0", true)
				if err != nil {
					return err
				}
				_, err = a.PauseApprovedPackGeneration(ctx, "freeze", victim)
				return err
			})
			before := run.resourcesSettled()
			receipt := run.ProcessPauseReceipt()
			if before == failed {
				t.Fatalf("setup proof mismatch: failed=%v settled=%v", failed, before)
			}
			calls := n.resumeCalls
			if err := run.Scope().ClearFault(context.Background(), receipt.Key); err == nil {
				t.Error("late public pause clear accepted")
			}
			if run.resourcesSettled() != before || n.resumeCalls != calls {
				t.Fatalf("late clear changed proof or reissued CONT: before=%v after=%v calls=%d/%d", before, run.resourcesSettled(), calls, n.resumeCalls)
			}
		})
	}
}

func TestPauseAlwaysOlderClearAfterProvenSettlement(t *testing.T) {
	f, n := pauseFixture(t)
	backend := &pauseUnknownSettlement{pauseNode: n}
	f.runtime.Jobs = n
	raw := []byte("name: pause-always\nphases:\n  - name: work\n    actions:\n      - action: apply\n  - name: cleanup\n    always: true\n    actions:\n      - action: clear\n")
	if err := os.WriteFile(f.binding.Scenario.Path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	f.binding.Scenario.SHA256 = fmt.Sprintf("%x", sha256.Sum256(raw))
	f.publish(t, nil)
	setGenerationConsent(t, f)
	policy, err := ReadTrustedPolicy(f.profile, uint32(os.Getuid()))
	if err != nil {
		t.Fatal(err)
	}
	run, err := OpenApprovedRun(context.Background(), f.request, policy, &f.runtime)
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close()
	restored, alwaysRan := 0, false
	r := NewRegistry()
	r.RegisterMutatingFunc("apply", TierChaos, func(ctx context.Context, a *ActionContext, _ Action) (map[string]string, error) {
		victim, _, e := a.RunApprovedPackJob(ctx, "volume0", true)
		if e != nil {
			return nil, e
		}
		backend.victim = victim.id
		e = a.Safety.ApplyFault("older-generic", func(context.Context) error { return nil }, func(context.Context) error { restored++; return nil }, func(context.Context) error { return nil })
		if e != nil {
			return nil, e
		}
		_, e = a.PauseApprovedPackGeneration(ctx, "freeze", victim)
		return nil, e
	})
	r.RegisterMutatingFunc("clear", TierChaos, func(ctx context.Context, a *ActionContext, _ Action) (map[string]string, error) {
		alwaysRan = true
		e := a.Safety.ClearFault(ctx, "older-generic")
		t.Logf("always_clear_error=%v older_restored=%v unknown_settlements=%d", e, restored, backend.failures)
		return nil, e
	})
	result := run.Execute(context.Background(), NewEngine(r, nil), &ActionContext{})
	t.Logf("actual_always_ran=%v older_restored=%v final_status=%s resources_settled=%v", alwaysRan, restored, result.Status, run.resourcesSettled())
	if !alwaysRan || restored != 1 || result.Status != StatusPass || !run.resourcesSettled() {
		t.Fatalf("proven settlement failed positive: ran=%v callbacks=%d status=%s settled=%v", alwaysRan, restored, result.Status, run.resourcesSettled())
	}
}

func TestPauseActiveOlderClearRefusesThenFrameworkCompletes(t *testing.T) {
	f, _ := pauseFixture(t)
	called := 0
	attempted := false
	run, result := runGenerationCase(t, f, func(ctx context.Context, a *ActionContext) error {
		victim, _, err := a.RunApprovedPackJob(ctx, "volume0", true)
		if err != nil {
			return err
		}
		err = a.Safety.ApplyFault("older-active", func(context.Context) error { return nil }, func(context.Context) error { called++; return nil }, func(context.Context) error { return nil })
		if err != nil {
			return err
		}
		if _, err = a.PauseApprovedPackGeneration(ctx, "freeze", victim); err != nil {
			return err
		}
		attempted = true
		if err = a.Safety.ClearFault(ctx, "older-active"); err == nil || called != 0 {
			t.Errorf("active pause admitted older restore: err=%v callbacks=%d", err, called)
		}
		return nil
	})
	if !attempted || called != 1 || result.Status != StatusPass || !run.resourcesSettled() {
		t.Fatalf("framework did not finish proven positive: attempted=%v callbacks=%d status=%s settled=%v", attempted, called, result.Status, run.resourcesSettled())
	}
}
