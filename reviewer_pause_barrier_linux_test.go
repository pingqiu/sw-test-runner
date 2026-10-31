//go:build linux

package testrunner

import (
	"context"
	"testing"
	"time"
)

func TestReviewerPauseEarlyClearCannotBypassIssuerBarrier(t *testing.T) {
	f, n := pauseFixture(t)
	n.mode = "barrier" // Assert exact one-live at CONT dispatch, not after Close can finish.
	run, _ := runGenerationCase(t, f, func(ctx context.Context, a *ActionContext) error {
		victim, _, err := a.RunApprovedPackJob(ctx, "volume0", true)
		if err != nil {
			return err
		}
		if _, _, err = a.RunApprovedPackJob(ctx, "volume1", true); err != nil {
			return err
		}
		receipt, err := a.PauseApprovedPackGeneration(ctx, "freeze", victim)
		if err != nil {
			return err
		}
		clearErr := a.Safety.ClearFault(ctx, receipt.Key)
		n.mu.Lock()
		resumes, live := n.resumeCalls, len(n.ids)
		n.mu.Unlock()
		a.Safety.mu.Lock()
		closed := a.Safety.closed
		a.Safety.mu.Unlock()
		t.Logf("early_clear_error=%v resume_calls=%d live_issuers_including_victim=%d admission_closed=%v", clearErr, resumes, live, closed)
		if clearErr != nil || resumes != 1 || !closed || live > 1 {
			t.Error("CONT dispatched before admission closed and other issuers settled")
		}
		return nil
	})
	if !run.resourcesSettled() {
		t.Fatal("successful terminal clear did not retain final settlement proof")
	}
}

type reviewerPanicPauseNode struct {
	*pauseNode
	contAttempts int
}

func (n *reviewerPanicPauseNode) command(ctx context.Context, id, op string, i ProcessIdentity, deadline ...time.Time) (custodyReply, error) {
	if op == "pause-resume" {
		n.contAttempts++
		if n.contAttempts == 1 {
			panic("reviewer CONT dispatch panic")
		}
	}
	return n.pauseNode.command(ctx, id, op, i, deadline...)
}

func TestReviewerPausePanicCannotReviveFinalProof(t *testing.T) {
	f, n := pauseFixture(t)
	backend := &reviewerPanicPauseNode{pauseNode: n}
	f.runtime.Jobs = backend
	run, result := runGenerationCase(t, f, func(ctx context.Context, a *ActionContext) error {
		victim, _, err := a.RunApprovedPackJob(ctx, "volume0", true)
		if err != nil {
			return err
		}
		receipt, err := a.PauseApprovedPackGeneration(ctx, "freeze", victim)
		if err != nil {
			return err
		}
		if err = a.Safety.ClearFault(ctx, receipt.Key); err == nil {
			t.Error("cleanup panic disappeared")
		}
		return nil
	})
	settled := run.resourcesSettled()
	t.Logf("CONT_attempts=%d result=%s resources_settled=%v", backend.contAttempts, result.Status, settled)
	if settled || backend.contAttempts != 1 {
		t.Error("panicked CONT retried and revived final release proof")
	}
}

func TestReviewerPauseExemptionCannotLeakIntoOlderRestore(t *testing.T) {
	f, n := pauseFixture(t)
	_, _ = runGenerationCase(t, f, func(ctx context.Context, a *ActionContext) error {
		victim, _, err := a.RunApprovedPackJob(ctx, "volume0", true)
		if err != nil {
			return err
		}
		err = a.Safety.ApplyFault("reviewer-older-persistent", func(context.Context) error { return nil }, func(cleanup context.Context) error {
			if err := a.Safety.settleWork(cleanup); err != nil {
				return err
			}
			n.mu.Lock()
			_, live := n.ids[victim.id]
			state := n.state
			n.mu.Unlock()
			t.Logf("older_restore victim_still_owned=%v pause_state=%s", live, state)
			if live {
				t.Error("pause victim exemption survived CONT into unrelated restoration")
			}
			return nil
		}, func(context.Context) error { return nil })
		if err != nil {
			return err
		}
		_, err = a.PauseApprovedPackGeneration(ctx, "freeze", victim)
		return err
	})
}
