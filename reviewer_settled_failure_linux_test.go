//go:build linux

package testrunner

import (
	"context"
	"errors"
	"testing"
)

// Original two regression witnesses from codex01 review 294eb392, followed by
// repeated/early cleanup and mixed known-outcome/UNKNOWN custody controls.

type reviewerFailedService struct {
	*faultOwnedBackend
	terminal map[string]ExitInfo
}

func (b reviewerFailedService) Settle(ctx context.Context, id string) error {
	b.fakeOwnedNode.mu.Lock()
	i, spec := b.fakeOwnedNode.ids[id], b.fakeOwnedNode.specs[id]
	b.fakeOwnedNode.mu.Unlock()
	if err := b.faultOwnedBackend.Settle(ctx, id); err != nil {
		return err
	}
	b.terminal[id] = ExitInfo{Identity: i, Code: 9, Signal: 0, CleanupStopped: false, Settled: true, StdoutPath: spec.StdoutPath, StderrPath: spec.StderrPath}
	return nil
}
func (b reviewerFailedService) Wait(ctx context.Context, id string) (ExitInfo, error) {
	if exit, ok := b.terminal[id]; ok {
		return exit, nil
	}
	return b.faultOwnedBackend.Wait(ctx, id)
}
func TestReviewerProvenFailedServiceCannotWithholdFaultRestore(t *testing.T) {
	f, run, resource, backend, plan := approvedFaultFixture(t, nil)
	run.runtime.Jobs = reviewerFailedService{backend, map[string]ExitInfo{}}
	scope := run.Scope()
	if err := scope.ApplyWitnessedFault(context.Background(), plan.Key(), plan); err != nil {
		t.Fatal(err)
	}
	if _, err := scope.start(scope.ctx, JobSpec{Node: f.job.Node, Executable: f.job.Executable, SHA256: f.job.SHA256, Argv: f.job.Argv, retainExit: true}, false); err != nil {
		t.Fatal(err)
	}
	err := run.Close()
	resource.mu.Lock()
	state := resource.state
	resource.mu.Unlock()
	scope.mu.Lock()
	remaining := len(scope.jobs)
	scope.mu.Unlock()
	t.Logf("close=%v state=%s possible_jobs=%d receipts=%+v", err, state, remaining, scope.FaultReceipts())
	if err == nil {
		t.Fatal("service failure lost from final verdict")
	}
	if state != "UP" {
		t.Fatal("all issuers proved settled, but failed service suppressed required fault restoration")
	}
}

func TestReviewerEarlyFailedStopMustRemainInFinalClose(t *testing.T) {
	f, run, resource, backend, plan := approvedFaultFixture(t, nil)
	run.runtime.Jobs = reviewerFailedService{backend, map[string]ExitInfo{}}
	scope := run.Scope()
	if err := scope.ApplyWitnessedFault(context.Background(), plan.Key(), plan); err != nil {
		t.Fatal(err)
	}
	h, err := scope.start(scope.ctx, JobSpec{Node: f.job.Node, Executable: f.job.Executable, SHA256: f.job.SHA256, Argv: f.job.Argv, retainExit: true}, false)
	if err != nil {
		t.Fatal(err)
	}
	first := scope.Stop(context.Background(), h)
	if first == nil {
		t.Fatal("fixture lacks proven failed service")
	}
	final := run.Close()
	resource.mu.Lock()
	state := resource.state
	resource.mu.Unlock()
	t.Logf("early_Stop=%v final_Close=%v restored_state=%s", first, final, state)
	if state != "UP" {
		t.Fatal("settled issuer did not restore fault")
	}
	if final == nil {
		t.Fatal("retired failed service disappeared from final Close")
	}
}

func TestProvenServiceOutcomeSurvivesRepeatedEarlyCleanup(t *testing.T) {
	for _, first := range []string{"clear", "stop", "wait"} {
		t.Run(first, func(t *testing.T) {
			f, run, resource, backend, plan := approvedFaultFixture(t, nil)
			run.runtime.Jobs = reviewerFailedService{backend, map[string]ExitInfo{}}
			scope := run.Scope()
			if err := scope.ApplyWitnessedFault(t.Context(), plan.Key(), plan); err != nil {
				t.Fatal(err)
			}
			h, err := scope.start(scope.ctx, JobSpec{Node: f.job.Node, Executable: f.job.Executable, SHA256: f.job.SHA256, Argv: f.job.Argv, retainExit: true}, false)
			if err != nil {
				t.Fatal(err)
			}
			if first == "stop" {
				for n := 0; n < 2; n++ {
					if scope.Stop(t.Context(), h) == nil {
						t.Fatal("repeated Stop lost outcome")
					}
				}
			}
			if first == "wait" {
				for n := 0; n < 2; n++ {
					if _, err := scope.Wait(t.Context(), h); err == nil {
						t.Fatal("repeated Wait lost early-exit outcome")
					}
				}
			}
			for n := 0; n < 2; n++ {
				if scope.ClearFault(t.Context(), plan.Key()) == nil {
					t.Fatal("early ClearFault lost service outcome")
				}
				resource.mu.Lock()
				state := resource.state
				resource.mu.Unlock()
				if state != "UP" {
					t.Fatalf("proven settlement withheld restore: %s", state)
				}
			}
			for n := 0; n < 2; n++ {
				if run.Close() == nil {
					t.Fatal("repeated final Close lost outcome")
				}
			}
			f.node.mu.Lock()
			restores := 0
			for _, spec := range f.node.specs {
				if spec.Restoration {
					restores++
				}
			}
			f.node.mu.Unlock()
			if restores != 1 {
				t.Fatalf("restoration commands=%d", restores)
			}
		})
	}
}

type mixedServiceCustody struct {
	reviewerFailedService
	unknown string
}

func (b *mixedServiceCustody) Settle(ctx context.Context, id string) error {
	if id == b.unknown {
		return errors.New("test descendant absence UNKNOWN")
	}
	return b.reviewerFailedService.Settle(ctx, id)
}
func TestKnownServiceFailureDoesNotHideUnknownIssuer(t *testing.T) {
	f, run, resource, backend, plan := approvedFaultFixture(t, nil)
	mixed := &mixedServiceCustody{reviewerFailedService: reviewerFailedService{backend, map[string]ExitInfo{}}}
	run.runtime.Jobs = mixed
	scope := run.Scope()
	if err := scope.ApplyWitnessedFault(t.Context(), plan.Key(), plan); err != nil {
		t.Fatal(err)
	}
	spec := JobSpec{Node: f.job.Node, Executable: f.job.Executable, SHA256: f.job.SHA256, Argv: f.job.Argv, retainExit: true}
	known, err := scope.start(scope.ctx, spec, false)
	if err != nil {
		t.Fatal(err)
	}
	unknown, err := scope.start(scope.ctx, spec, false)
	if err != nil {
		t.Fatal(err)
	}
	mixed.unknown = unknown.id
	if run.Close() == nil {
		t.Fatal("mixed UNKNOWN passed")
	}
	resource.mu.Lock()
	state := resource.state
	resource.mu.Unlock()
	if state != "DOWN" {
		t.Fatal("UNKNOWN issuer allowed restore")
	}
	if len(scope.jobs) != 1 || scope.jobs[unknown.id] == nil {
		t.Fatal("UNKNOWN custody lost or known exit retained as possible")
	}
	receipt := run.jobReceipts()[known.id]
	if receipt.Exit == nil || receipt.Exit.Code != 9 || !receipt.Exit.Settled {
		t.Fatal("known terminal evidence lost")
	}
}

func TestKnownServiceFailureStillFailsEngineAfterSwallowedStop(t *testing.T) {
	f, run, resource, backend, plan := approvedFaultFixture(t, nil)
	run.runtime.Jobs = reviewerFailedService{backend, map[string]ExitInfo{}}
	r := NewRegistry()
	r.RegisterFunc("probe", TierCore, func(ctx context.Context, a *ActionContext, _ Action) (map[string]string, error) {
		if err := a.Safety.ApplyWitnessedFault(ctx, plan.Key(), plan); err != nil {
			return nil, err
		}
		h, err := a.Safety.start(ctx, JobSpec{Node: f.job.Node, Executable: f.job.Executable, SHA256: f.job.SHA256, Argv: f.job.Argv, retainExit: true}, false)
		if err != nil {
			return nil, err
		}
		if a.Safety.Stop(ctx, h) == nil {
			t.Fatal("fixture lacks a failed service")
		}
		return nil, nil // A caller swallowing Stop cannot turn final closure into PASS.
	})
	result := run.Execute(t.Context(), NewEngine(r, nil), &ActionContext{})
	resource.mu.Lock()
	state := resource.state
	resource.mu.Unlock()
	if result.Status != StatusFail || state != "UP" || run.Close() == nil {
		t.Fatalf("result=%s state=%s", result.Status, state)
	}
}
