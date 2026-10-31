package testrunner

import (
	"context"
	"errors"
	"time"
)

type ProcessPausePlan struct {
	Name            string    `json:"name"`
	Victim          string    `json:"victim"`
	Deadline        time.Time `json:"deadline"`
	AllowWorkResume bool      `json:"allow_work_resume,omitempty"`
}
type ProcessPauseWitness struct {
	Identity      ProcessIdentity
	Before, State string
	ObservedAt    time.Time
}
type ProcessPauseReceipt struct {
	Key, VictimJobID                      string
	Plan                                  ProcessPausePlan
	Identity                              ProcessIdentity
	PreStopMono                           float64
	StoppedMono, PreContMono, ResumedMono float64
	Stopped, Resumed, Final               ProcessPauseWitness
	Error                                 string
}

// Controller and custodian retain separate instances: a lost reply cannot merge
// their failure domains. The custodian uses only the exact grant and CONT latch.
type processPause struct {
	ProcessPauseReceipt
	run       *ApprovedRun
	victim    JobHandle
	resumeErr error
}

func (a *ApprovedRun) ProcessPauseReceipt() *ProcessPauseReceipt {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.pauseReceipt == nil {
		return nil
	}
	r := *a.pauseReceipt
	return &r
}

func (c *ActionContext) PauseApprovedPackGeneration(ctx context.Context, name string, victim JobHandle) (receipt *ProcessPauseReceipt, err error) {
	if err = c.boundPackContext(); err != nil {
		return
	}
	defer func() { c.FailApprovedPack(err) }()
	plan := c.approved.runPolicy.ProcessPause
	if plan == nil || plan.Name != name || !c.approved.approval.AllowMutating {
		return nil, errors.New("pause lacks independent episode/consent")
	}
	identity, e := c.liveApprovedService(ctx, plan.Victim, victim)
	if e != nil {
		return nil, e
	}
	p := &processPause{run: c.approved, victim: victim, ProcessPauseReceipt: ProcessPauseReceipt{Key: "process-pause/" + victim.id, VictimJobID: victim.id, Plan: *plan, Identity: identity}}
	err = c.Safety.applyFault(p.Key, func(work context.Context) error {
		work, cancel := context.WithCancel(work)
		stop := context.AfterFunc(ctx, cancel)
		defer stop()
		defer cancel()
		return p.signal(work, "pause-stop", &p.Stopped)
	}, func(cleanup context.Context) error {
		if p.resumeErr != nil {
			return p.resumeErr
		}
		p.resumeErr = errors.New("CONT pending or failed") // Panic must leave proof ineligible.
		c.Safety.beginCleanup()
		if err := c.Safety.settlePauseIssuers(cleanup); err != nil {
			return err
		}
		return p.resume(cleanup)
	}, func(cleanup context.Context) error {
		if p.resumeErr != nil {
			return p.resumeErr
		}
		return p.signal(cleanup, "pause-observe", &p.Final)
	}, p)
	return c.approved.ProcessPauseReceipt(), err
}

// Caller owns the restoration gate. Both paths consume the same one-shot latch.
func (p *processPause) resume(ctx context.Context) error {
	p.resumeErr = errors.New("CONT pending or failed")
	p.resumeErr = p.signal(ctx, "pause-resume", &p.Resumed)
	return p.resumeErr
}

func (c *ActionContext) ResumeApprovedPackGeneration(ctx context.Context, name string, victim JobHandle) (receipt *ProcessPauseReceipt, err error) {
	if err = c.boundPackContext(); err != nil {
		return
	}
	defer func() { c.FailApprovedPack(err) }()
	s := c.Safety
	s.mu.Lock()
	p := s.pause
	if p == nil || p.Plan.Name != name || !p.Plan.AllowWorkResume || p.victim != victim || len(s.restores) != 1 {
		s.mu.Unlock()
		return nil, errors.New("normal resume not independently bound")
	}
	r := s.restores[p.Key]
	s.mu.Unlock()
	ctx, cancel := context.WithDeadline(ctx, s.admission.WorkDeadline)
	stop := context.AfterFunc(s.ctx, cancel)
	defer cancel()
	defer stop()
	if err = take(ctx, r.ready, r.gate); err != nil {
		return
	}
	defer func() { <-r.gate }()
	if r.applyErr != nil || r.cleared || p.resumeErr != nil {
		return nil, errors.New("STOP unproven or CONT already attempted")
	}
	// Fresh WORK admission follows every blocking STOP/gate join.
	if err = s.backend.(*approvedJobs).work(ctx, victim.id, victim.job.spec); err != nil {
		return
	}
	s.mu.Lock()
	if s.closed || s.ctx.Err() != nil || ctx.Err() != nil {
		s.mu.Unlock()
		return nil, errors.New("WORK closed before CONT arm")
	}
	p.resumeErr = errors.New("CONT armed")
	s.mu.Unlock()
	err = cleanupCall(ctx, p.resume)
	if err == nil {
		r.cleared = true
		p.run.mu.Lock()
		p.ResumedMono, err = generationMonotonic()
		p.run.mu.Unlock()
	} // Physical success survives WORK closure; never a second CONT.
	if err == nil {
		err = errors.Join(s.backend.(*approvedJobs).work(ctx, victim.id, victim.job.spec), ctx.Err(), s.ctx.Err())
	}
	return c.approved.ProcessPauseReceipt(), err
}

func (p *processPause) signal(ctx context.Context, op string, dst *ProcessPauseWitness) (err error) {
	ctx, cancel := context.WithDeadline(ctx, p.Plan.Deadline)
	defer cancel()
	ctx, cancel = context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	j, unlock, err := p.run.scope.lockJob(ctx, p.victim)
	if err != nil {
		return err
	}
	defer unlock()
	if j.settled || j.identity != p.Identity {
		return errors.New("pause generation no longer live")
	}
	if op == "pause-stop" {
		if err = p.run.scope.backend.(*approvedJobs).work(ctx, p.victim.id, j.spec); err != nil {
			p.run.revoke(err)
			return err
		}
		P, e := generationMonotonic()
		if e != nil {
			return e
		}
		p.run.mu.Lock()
		p.PreStopMono = P
		p.run.mu.Unlock()
	} else if err = p.run.readbackNode(ctx, j.identity.Node, "pause node no longer bound"); err != nil {
		return err
	}
	backend, ok := p.run.runtime.Jobs.(interface {
		command(context.Context, string, string, ProcessIdentity, ...time.Time) (custodyReply, error)
	})
	if !ok {
		return errors.New("pause custody unsupported")
	}
	if op == "pause-resume" {
		p.run.mu.Lock()
		p.PreContMono, err = generationMonotonic()
		p.run.mu.Unlock()
		if err != nil {
			return err
		}
	}
	reply, err := backend.command(ctx, p.victim.id, op, j.identity, p.Plan.Deadline)
	w := reply.Pause
	want := "LIVE"
	if op == "pause-stop" {
		want = "STOPPED"
	}
	if err == nil && (ctx.Err() != nil || w.Identity != j.identity || w.State != want || w.ObservedAt.IsZero() || (op == "pause-stop" && w.Before != "LIVE")) {
		err = errors.New("pause identity/state UNKNOWN")
	}
	p.run.mu.Lock()
	*dst = w
	if err != nil {
		p.Error = err.Error()
	}
	if err == nil && op == "pause-stop" {
		p.StoppedMono, err = generationMonotonic()
	}
	p.run.mu.Unlock()
	return err
}
