//go:build linux

package testrunner

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type reviewerContJoinNode struct {
	*pauseNode
	entered, release, otherSettled chan struct{}
	once                           sync.Once
	victimID                       string
}

func (n *reviewerContJoinNode) command(ctx context.Context, id, op string, i ProcessIdentity, deadline ...time.Time) (custodyReply, error) {
	if op == "pause-resume" {
		close(n.entered)
		<-n.release // A dispatched operation/reply may outlive caller cancellation.
	}
	return n.pauseNode.command(ctx, id, op, i, deadline...)
}
func (n *reviewerContJoinNode) Settle(ctx context.Context, id string) error {
	if id != n.victimID {
		select {
		case <-n.entered:
			n.once.Do(func() { close(n.otherSettled) })
		default:
		}
	}
	return n.generationNode.Settle(ctx, id)
}

func TestReviewerCloseJoinsArmedNormalContBeforeOtherIssuers(t *testing.T) {
	f, base := pauseFixture(t)
	f.policy.ProcessPause.AllowWorkResume = true
	f.publish(t, nil)
	setGenerationConsent(t, f)
	n := &reviewerContJoinNode{pauseNode: base, entered: make(chan struct{}), release: make(chan struct{}), otherSettled: make(chan struct{})}
	f.runtime.Jobs = n
	var settledBeforeContJoin bool
	_, result := runGenerationCase(t, f, func(ctx context.Context, a *ActionContext) error {
		victim, _, err := a.RunApprovedPackJob(ctx, "volume0", true)
		if err != nil {
			return err
		}
		n.victimID = victim.id
		if _, _, err = a.RunApprovedPackJob(ctx, "volume1", true); err != nil {
			return err
		}
		if _, err = a.PauseApprovedPackGeneration(ctx, "freeze", victim); err != nil {
			return err
		}
		resumed := make(chan error, 1)
		go func() { _, e := a.ResumeApprovedPackGeneration(ctx, "freeze", victim); resumed <- e }()
		select {
		case <-n.entered:
		case <-time.After(time.Second):
			t.Fatal("CONT not armed")
		}
		closed := make(chan error, 1)
		go func() { closed <- a.Safety.Close() }()
		select {
		case <-n.otherSettled:
			settledBeforeContJoin = true
		case <-time.After(100 * time.Millisecond):
		}
		close(n.release)
		resumeErr := <-resumed
		closeErr := <-closed
		t.Logf("other_settled_before_cont_join=%v CONT=%d resume=%v close=%v", settledBeforeContJoin, base.resumeCalls, resumeErr, closeErr)
		return errors.Join(resumeErr, closeErr)
	})
	t.Logf("engine_status=%s", result.Status)
	if settledBeforeContJoin {
		t.Error("Close settled an OTHER issuer before joining the already-armed CONT")
	}
}

func TestPauseCloseUnknownNormalJoinWithholdsOtherSettlement(t *testing.T) {
	f, base := pauseFixture(t)
	f.binding.CleanupReserveMS = 50
	f.policy.ProcessPause.AllowWorkResume = true
	f.publish(t, nil)
	setGenerationConsent(t, f)
	n := &reviewerContJoinNode{pauseNode: base, entered: make(chan struct{}), release: make(chan struct{}), otherSettled: make(chan struct{})}
	f.runtime.Jobs = n
	run, result := runGenerationCase(t, f, func(ctx context.Context, a *ActionContext) error {
		victim, _, err := a.RunApprovedPackJob(ctx, "volume0", true)
		if err != nil {
			return err
		}
		n.victimID = victim.id
		if _, _, err = a.RunApprovedPackJob(ctx, "volume1", true); err != nil {
			return err
		}
		if _, err = a.PauseApprovedPackGeneration(ctx, "freeze", victim); err != nil {
			return err
		}
		resumed := make(chan error, 1)
		go func() { _, e := a.ResumeApprovedPackGeneration(ctx, "freeze", victim); resumed <- e }()
		<-n.entered
		closeErr := a.Safety.Close()
		select {
		case <-n.otherSettled:
			t.Error("OTHER issuer settled after UNKNOWN normal join")
		default:
		}
		close(n.release)
		return errors.Join(closeErr, <-resumed)
	})
	if result.Status != StatusFail || run.resourcesSettled() || base.resumeCalls != 1 {
		t.Fatalf("status=%s settled=%v CONT=%d", result.Status, run.resourcesSettled(), base.resumeCalls)
	}
}
