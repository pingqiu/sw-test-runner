//go:build linux

package testrunner

import (
	"context"
	"fmt"
	"github.com/pingqiu/sw-test-runner/infra"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestPauseNormalWorkResume(t *testing.T) {
	f, n := pauseFixture(t)
	f.policy.ProcessPause.AllowWorkResume = true
	f.publish(t, nil)
	setGenerationConsent(t, f)
	run, result := runGenerationCase(t, f, func(ctx context.Context, a *ActionContext) error {
		victim, _, err := a.RunApprovedPackJob(ctx, "volume0", true)
		if err != nil {
			return err
		}
		if _, err = a.PauseApprovedPackGeneration(ctx, "freeze", victim); err != nil {
			return err
		}
		receipt, err := a.ResumeApprovedPackGeneration(ctx, "freeze", victim)
		if err != nil {
			return err
		}
		if receipt.StoppedMono <= 0 || receipt.PreContMono < receipt.StoppedMono || receipt.ResumedMono < receipt.PreContMono {
			return fmt.Errorf("monotonic custody boundary missing")
		}
		if _, _, err = a.RunApprovedPackJob(ctx, "volume1", true); err != nil {
			return fmt.Errorf("normal resume closed WORK: %w", err)
		}
		return nil
	})
	if result.Status != StatusPass || !run.resourcesSettled() || n.resumeCalls != 1 {
		t.Fatalf("status=%s settled=%v CONT=%d receipt=%+v", result.Status, run.resourcesSettled(), n.resumeCalls, run.ProcessPauseReceipt())
	}
}

// The actual Engine owns the scope and cleanup; only the remote reply is faked.
type workResumeNode struct {
	*pauseNode
	afterCont func(context.Context)
	normal    atomic.Int64
	workOpen  func() bool
}

func (n *workResumeNode) command(ctx context.Context, id, op string, i ProcessIdentity, deadline ...time.Time) (custodyReply, error) {
	if op == "pause-resume" && n.workOpen != nil && n.workOpen() {
		n.normal.Add(1)
	}
	r, e := n.pauseNode.command(ctx, id, op, i, deadline...)
	if op == "pause-resume" && n.afterCont != nil {
		n.afterCont(ctx)
	}
	return r, e
}
func TestPauseNormalWorkRefusals(t *testing.T) {
	for _, mode := range []string{"no-optin", "foreign-episode", "mixed-obligation", "replay", "revoked-before", "lost-stop", "cancel-after-arm", "close-after-arm", "cancel-during-readback", "lost-cont", "panic-cont"} {
		t.Run(mode, func(t *testing.T) {
			f, base := pauseFixture(t)
			f.policy.ProcessPause.AllowWorkResume = mode != "no-optin"
			f.publish(t, nil)
			setGenerationConsent(t, f)
			n := &workResumeNode{pauseNode: base}
			f.runtime.Jobs = n
			var before int
			var sent atomic.Bool
			var cancelAfterReadback context.CancelFunc
			priorReadback := f.runtime.Readback
			f.runtime.Readback = func(c context.Context, p infra.DeploymentPolicy) (infra.DeploymentReceipt, error) {
				r, e := priorReadback(c, p)
				if sent.Load() {
					cancelAfterReadback()
				}
				return r, e
			}
			run, result := runGenerationCase(t, f, func(ctx context.Context, a *ActionContext) error {
				n.workOpen = func() bool {
					a.Safety.mu.Lock()
					defer a.Safety.mu.Unlock()
					return !a.Safety.closed && a.Safety.ctx.Err() == nil
				}
				victim, _, err := a.RunApprovedPackJob(ctx, "volume0", true)
				if err != nil {
					return err
				}
				if mode == "mixed-obligation" {
					if err = a.Safety.ApplyFault("older", func(context.Context) error { return nil }, func(context.Context) error { return nil }, func(context.Context) error { return nil }); err != nil {
						return err
					}
				}
				if mode == "lost-stop" {
					base.mode = "lost-stop"
				}
				_, pauseErr := a.PauseApprovedPackGeneration(ctx, "freeze", victim)
				if pauseErr != nil && mode != "lost-stop" {
					return pauseErr
				}
				work, cancel := context.WithCancel(ctx)
				defer cancel()
				name := "freeze"
				if mode == "foreign-episode" {
					name = "other"
				}
				if mode == "revoked-before" {
					if err = os.WriteFile(filepath.Join(f.dir, "go.json"), []byte("{}"), 0600); err != nil {
						return err
					}
				}
				if mode == "cancel-during-readback" {
					cancelAfterReadback = cancel
					n.afterCont = func(context.Context) { sent.Store(true) }
				}
				if mode == "cancel-after-arm" {
					n.afterCont = func(context.Context) { cancel() }
				}
				if mode == "close-after-arm" {
					n.afterCont = func(context.Context) { a.Safety.beginCleanup() }
				}
				if mode == "lost-cont" {
					n.afterCont = func(context.Context) { cancel() }
				}
				if mode == "panic-cont" {
					n.afterCont = func(context.Context) { panic("lost CONT reply") }
				}
				_, err = a.ResumeApprovedPackGeneration(work, name, victim)
				before = int(n.normal.Load())
				if mode == "replay" && err == nil {
					_, err = a.ResumeApprovedPackGeneration(ctx, name, victim)
				}
				if err == nil {
					t.Error("invalid normal resume accepted")
					return fmt.Errorf("invalid normal resume accepted")
				}

				return err
			})
			if result.Status != StatusFail {
				t.Fatalf("%s status=%s", mode, result.Status)
			}
			afterArm := mode == "cancel-after-arm" || mode == "cancel-during-readback" || mode == "close-after-arm" || mode == "lost-cont" || mode == "panic-cont"
			if mode != "replay" && !afterArm && before != 0 {
				t.Fatalf("normal CONT before admission: %d", before)
			}
			if base.resumeCalls != 1 {
				t.Fatalf("CONT calls=%d", base.resumeCalls)
			}
			if afterArm && mode != "close-after-arm" && mode != "cancel-during-readback" && run.resourcesSettled() {
				t.Fatal("lost/failed armed attempt revived by cleanup")
			}
			if !afterArm && !run.resourcesSettled() {
				t.Fatalf("retained cleanup unavailable: %v", run.Close())
			}
		})
	}
}

func TestPauseNormalJoinsStopBeforeFreshAdmission(t *testing.T) {
	f, n := pauseFixture(t)
	f.policy.ProcessPause.AllowWorkResume = true
	f.publish(t, nil)
	setGenerationConsent(t, f)
	n.stopEntered, n.stopRelease = make(chan struct{}), make(chan struct{})
	wrapper := &workResumeNode{pauseNode: n}
	f.runtime.Jobs = wrapper
	var normalCalls int
	run, result := runGenerationCase(t, f, func(ctx context.Context, a *ActionContext) error {
		wrapper.workOpen = func() bool {
			a.Safety.mu.Lock()
			defer a.Safety.mu.Unlock()
			return !a.Safety.closed && a.Safety.ctx.Err() == nil
		}
		victim, _, err := a.RunApprovedPackJob(ctx, "volume0", true)
		if err != nil {
			return err
		}
		stopped := make(chan error, 1)
		go func() { _, e := a.PauseApprovedPackGeneration(ctx, "freeze", victim); stopped <- e }()
		<-n.stopEntered
		resumed := make(chan error, 1)
		go func() { _, e := a.ResumeApprovedPackGeneration(ctx, "freeze", victim); resumed <- e }()
		select {
		case e := <-resumed:
			t.Fatalf("normal CONT passed STOP join: %v", e)
		case <-time.After(10 * time.Millisecond):
		}
		if err = os.WriteFile(filepath.Join(f.dir, "go.json"), []byte("{}"), 0600); err != nil {
			return err
		}
		close(n.stopRelease)
		<-stopped
		err = <-resumed
		normalCalls = int(wrapper.normal.Load())
		if err == nil {
			return fmt.Errorf("fresh authority after STOP join ignored")
		}
		return err
	})
	if result.Status != StatusFail || normalCalls != 0 || n.resumeCalls != 1 || !run.resourcesSettled() {
		t.Fatalf("status=%s normal=%d total=%d settled=%v", result.Status, normalCalls, n.resumeCalls, run.resourcesSettled())
	}
}
