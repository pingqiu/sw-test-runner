package testrunner

import (
	"context"
	"errors"
	"time"
)

type ProcessPausePlan struct {
	Victim   string    `json:"victim"`
	Deadline time.Time `json:"deadline"`
}
type ProcessPauseWitness struct {
	Identity   ProcessIdentity
	State      string
	ObservedAt time.Time
}
type ProcessPauseReceipt struct {
	VictimJobID             string
	Plan                    ProcessPausePlan
	Identity                ProcessIdentity
	PreStopMono             float64
	Stopped, Resumed, Final ProcessPauseWitness
	Error                   string
}
type pauseBackend interface {
	pauseCommand(context.Context, string, string, ProcessIdentity, time.Time) (ProcessPauseWitness, error)
}
type ownedPause struct {
	run       *ApprovedRun
	victim    JobHandle
	name, key string
	ready     chan struct{}
	receipt   ProcessPauseReceipt // RunScope.mu; execution uses the existing job/restoration gates.
	resumeErr error               // Existing restoration gate, after apply.ready.
}

func (a *ApprovedRun) ProcessPauseReceipts() map[string]ProcessPauseReceipt {
	a.scope.mu.Lock()
	defer a.scope.mu.Unlock()
	if a.scope.pause == nil {
		return nil
	}
	p := a.scope.pause
	return map[string]ProcessPauseReceipt{p.name: p.receipt}
}

func (c *ActionContext) PauseApprovedPackGeneration(ctx context.Context, name string, victim JobHandle) (receipt ProcessPauseReceipt, err error) {
	if err = c.boundPackContext(); err != nil {
		return
	}
	defer func() { c.FailApprovedPack(err) }()
	plan, ok := c.approved.runPolicy.ProcessPauses[name]
	if !ok || !c.approved.approval.AllowMutating {
		return receipt, errors.New("pause lacks independent episode/consent")
	}
	j, unlock, e := c.Safety.lockJob(ctx, victim)
	if e != nil {
		return receipt, e
	}
	valid := j.spec.retainExit && !j.settled && j.spec.packLabel == plan.Victim && sameJob(c.approved.runPolicy.Jobs[plan.Victim], j.spec)
	identity := j.identity
	unlock()
	if !valid {
		return receipt, errors.New("pause victim is not exact owned service")
	}
	p := &ownedPause{run: c.approved, victim: victim, name: name, key: "process-pause/" + victim.id, receipt: ProcessPauseReceipt{VictimJobID: victim.id, Plan: plan, Identity: identity}}
	err = c.Safety.applyFault(p.key, func(work context.Context) error {
		work, cancel := context.WithCancel(work)
		stop := context.AfterFunc(ctx, cancel)
		defer stop()
		defer cancel()
		return p.command(work, "pause-stop", &p.receipt.Stopped)
	}, func(cleanup context.Context) error {
		if p.resumeErr != nil {
			return p.resumeErr
		}
		p.resumeErr = p.command(cleanup, "pause-resume", &p.receipt.Resumed)
		return p.resumeErr
	}, func(cleanup context.Context) error {
		if p.resumeErr != nil {
			return p.resumeErr
		}
		return p.command(cleanup, "pause-observe", &p.receipt.Final)
	}, p)
	return c.approved.ProcessPauseReceipts()[name], err
}
func (c *ActionContext) ResumeApprovedPackGeneration(ctx context.Context, name string) (ProcessPauseReceipt, error) {
	if err := c.boundPackContext(); err != nil {
		return ProcessPauseReceipt{}, err
	}
	c.Safety.mu.Lock()
	p := c.Safety.pause
	c.Safety.mu.Unlock()
	if p == nil || p.name != name {
		return ProcessPauseReceipt{}, errors.New("no registered pause")
	}
	err := c.Safety.ClearFault(ctx, p.key)
	c.FailApprovedPack(err)
	return c.approved.ProcessPauseReceipts()[name], err
}

func (p *ownedPause) command(ctx context.Context, op string, dst *ProcessPauseWitness) (err error) {
	s := p.run.scope
	ctx, cancel := context.WithDeadline(ctx, p.receipt.Plan.Deadline)
	defer cancel()
	ctx, cancel = context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	j, unlock, err := s.lockJob(ctx, p.victim)
	if err != nil {
		return err
	}
	defer unlock()
	if j.settled || j.identity != p.receipt.Identity {
		return errors.New("pause generation no longer live")
	}
	b := s.backend.(*approvedJobs)
	if op == "pause-stop" {
		if err = b.work(ctx, p.victim.id, j.spec); err != nil {
			p.run.revoke(err)
			return err
		}
		P, e := generationMonotonic()
		if e != nil {
			return e
		}
		s.mu.Lock()
		p.receipt.PreStopMono = P
		s.mu.Unlock()
	} else if err = p.run.readbackNode(ctx, j.identity.Node); err != nil {
		return err
	}
	backend, ok := p.run.runtime.Jobs.(pauseBackend)
	if !ok {
		return errors.New("pause backend unsupported")
	}
	w, err := backend.pauseCommand(ctx, p.victim.id, op, j.identity, p.receipt.Plan.Deadline)
	want := "LIVE"
	if op == "pause-stop" {
		want = "STOPPED"
	}
	if err == nil && (w.Identity != j.identity || w.State != want || w.ObservedAt.IsZero()) {
		err = errors.New("pause identity/state UNKNOWN")
	}
	s.mu.Lock()
	*dst = w
	if err != nil {
		p.receipt.Error = err.Error()
	}
	s.mu.Unlock()
	return err
}
func (s *RunScope) joinPauseDispatches(ctx context.Context) error {
	s.mu.Lock()
	p := s.pause
	s.mu.Unlock()
	if p == nil {
		return nil
	}
	select {
	case <-p.ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (b *SSHJobs) pauseCommand(ctx context.Context, id, op string, i ProcessIdentity, deadline time.Time) (ProcessPauseWitness, error) {
	r, err := b.command(ctx, id, op, i, deadline)
	return r.Pause, err
}
