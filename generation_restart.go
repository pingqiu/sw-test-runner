package testrunner

import (
	"context"
	"errors"
	"maps"
	"time"
)

// GenerationExitIntent is recorded before signal dispatch; it is not exit proof.
type GenerationExitIntent struct {
	Episode  string
	Identity ProcessIdentity
	BoundAt  time.Time
}

// GenerationRestartReceipt proves dispatch and old-generation settlement, not
// readiness or application recovery. Those remain a pack-level oracle join.
type GenerationRestartReceipt struct {
	VictimJobID, ReplacementJobID string
	Plan                          GenerationRestartPlan
	Intent                        GenerationExitIntent
	State                         string
	Kill                          KillWitness
	OldExit                       ExitInfo
	Replacement                   ProcessIdentity
	Error                         string
}

func (a *ApprovedRun) GenerationRestartReceipts() map[string]GenerationRestartReceipt {
	a.mu.Lock()
	defer a.mu.Unlock()
	return maps.Clone(a.generationRestarts)
}

// RestartApprovedPackGeneration consumes one independently policy-pinned episode.
// It never accepts a PID, signal, executable, argv or expected-exit boolean.
// Replacement enrollment remains ordinary work; revocation never grants it as cleanup.
func (c *ActionContext) RestartApprovedPackGeneration(ctx context.Context, name string, victim JobHandle) (next JobHandle, receipt GenerationRestartReceipt, err error) {
	if _, err = c.ApprovedPackInputs(); err != nil {
		return
	}
	a := c.approved
	defer func() {
		if err != nil {
			receipt.Error = err.Error()
			c.FailApprovedPack(err)
		}
	}()
	plan, ok := a.runPolicy.GenerationRestarts[name]
	if !ok || !a.approval.AllowMutating {
		err = errors.New("generation restart lacks explicit approved episode/consent")
		return
	}
	before, bok := a.runPolicy.Jobs[plan.Victim]
	replacement, rok := a.runPolicy.Jobs[plan.Replacement]
	if !bok || !rok || plan.Victim == plan.Replacement || before.Node != replacement.Node || before.Executable != replacement.Executable || before.SHA256 != replacement.SHA256 {
		err = errors.New("invalid generation plan")
		return
	}
	identity, e := c.liveApprovedService(ctx, plan.Victim, victim)
	if e != nil {
		err = e
		return
	}
	a.mu.Lock()
	if a.generationRestarts == nil {
		a.generationRestarts = map[string]GenerationRestartReceipt{}
	}
	if _, used := a.generationRestarts[name]; used {
		a.mu.Unlock()
		err = errors.New("generation episode already consumed")
		return
	}
	receipt = GenerationRestartReceipt{Plan: plan, VictimJobID: victim.id, State: "ADMITTED", Intent: GenerationExitIntent{Episode: name, Identity: identity, BoundAt: time.Now()}}
	a.generationRestarts[name] = receipt
	a.mu.Unlock()
	defer func() {
		if err != nil {
			receipt.State = "FAILED"
			receipt.Error = err.Error()
		}
		a.mu.Lock()
		a.generationRestarts[name] = receipt
		a.mu.Unlock()
	}()
	receipt.Kill, err = c.Safety.kill(ctx, victim, &receipt.Intent)
	if err != nil {
		return
	}
	receipt.State = "KILL_WITNESSED"
	receipt.OldExit, err = c.Safety.Wait(ctx, victim)
	if err != nil {
		return
	}
	if receipt.OldExit.Identity != receipt.Kill.Identity || receipt.OldExit.Code != -1 || receipt.OldExit.Signal != 9 {
		err = errors.New("old generation lacks matching KILL exit")
		return
	}
	receipt.State = "OLD_SETTLED"
	next, _, err = c.RunApprovedPackJob(ctx, plan.Replacement, true)
	if err != nil {
		return
	}
	receipt.ReplacementJobID = next.id
	receipt.Replacement = next.job.identity
	if receipt.Replacement.BootID != receipt.OldExit.Identity.BootID || (receipt.Replacement.PID == receipt.OldExit.Identity.PID && receipt.Replacement.StartTime == receipt.OldExit.Identity.StartTime) {
		err = errors.New("replacement generation identity did not change")
		return
	}
	receipt.State = "REPLACEMENT_STARTED"
	return
}

func (c *ActionContext) liveApprovedService(ctx context.Context, label string, h JobHandle) (ProcessIdentity, error) {
	j, unlock, err := c.Safety.lockJob(ctx, h)
	if err != nil {
		return ProcessIdentity{}, err
	}
	defer unlock()
	if !j.spec.retainExit || j.settled || j.spec.packLabel != label || !sameJob(c.approved.runPolicy.Jobs[label], j.spec) {
		return ProcessIdentity{}, errors.New("victim is not the exact live approved service generation")
	}
	return j.identity, nil
}
