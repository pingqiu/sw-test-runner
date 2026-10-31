package testrunner

import (
	"context"
	"errors"
	"sync"
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
	VictimJobID                     string
	Plan                            ProcessPausePlan
	Identity                        ProcessIdentity
	BoundAt                         time.Time
	PreStopMono                     float64
	Before, Stopped, Resumed, Final ProcessPauseWitness
	Error                           string
}
type pauseBackend interface {
	pauseCommand(context.Context, string, string, ProcessIdentity, time.Time) (ProcessPauseWitness, error)
}
type ownedPause struct {
	mu        sync.Mutex
	scope     *RunScope
	victim    JobHandle
	name, key string
	ready     chan struct{}
	receipt   ProcessPauseReceipt
	resumeErr error
}

func (a *ApprovedRun) ProcessPauseReceipts() map[string]ProcessPauseReceipt {
	a.scope.mu.Lock()
	rows := make([]*ownedPause, 0, len(a.scope.pauses))
	for _, p := range a.scope.pauses {
		rows = append(rows, p)
	}
	a.scope.mu.Unlock()
	out := map[string]ProcessPauseReceipt{}
	for _, p := range rows {
		p.mu.Lock()
		out[p.name] = p.receipt
		p.mu.Unlock()
	}
	return out
}

// The independently pinned label selects one owned generation, never a PID.
func (c *ActionContext) PauseApprovedPackGeneration(ctx context.Context, name string, victim JobHandle) (receipt ProcessPauseReceipt, err error) {
	if err = c.boundPackContext(); err != nil {
		return
	}
	defer func() {
		if err != nil {
			c.FailApprovedPack(err)
		}
	}()
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
	p := &ownedPause{scope: c.Safety, victim: victim, name: name, key: "process-pause/" + victim.id, receipt: ProcessPauseReceipt{VictimJobID: victim.id, Plan: plan, Identity: identity, BoundAt: time.Now()}}
	err = c.Safety.applyFault(p.key, func(work context.Context) error {
		before, e := p.command(work, "pause-bind")
		p.mu.Lock()
		p.receipt.Before = before
		p.mu.Unlock()
		if e != nil {
			return e
		}
		P, e := generationMonotonic()
		if e != nil {
			return e
		}
		p.mu.Lock()
		p.receipt.PreStopMono = P
		p.mu.Unlock()
		stopped, e := p.command(work, "pause-stop")
		p.mu.Lock()
		p.receipt.Stopped = stopped
		p.mu.Unlock()
		return e
	}, p.resume, p.verify, p)
	p.mu.Lock()
	if err != nil {
		p.receipt.Error = err.Error()
	}
	receipt = p.receipt
	p.mu.Unlock()
	return
}

func (c *ActionContext) ResumeApprovedPackGeneration(ctx context.Context, name string) (receipt ProcessPauseReceipt, err error) {
	if err = c.boundPackContext(); err != nil {
		return
	}
	c.Safety.mu.Lock()
	var p *ownedPause
	for _, v := range c.Safety.pauses {
		if v.name == name {
			p = v
		}
	}
	c.Safety.mu.Unlock()
	if p == nil {
		return receipt, errors.New("no registered pause")
	}
	err = c.Safety.ClearFault(ctx, p.key)
	p.mu.Lock()
	receipt = p.receipt
	p.mu.Unlock()
	if err != nil {
		c.FailApprovedPack(err)
	}
	return
}

func (p *ownedPause) command(ctx context.Context, op string) (ProcessPauseWitness, error) {
	deadline := p.receipt.Plan.Deadline
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	ctx, cancel = context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	j, unlock, err := p.scope.lockJob(ctx, p.victim)
	if err != nil {
		return ProcessPauseWitness{}, err
	}
	defer unlock()
	if j.settled || j.identity != p.receipt.Identity {
		return ProcessPauseWitness{}, errors.New("pause generation no longer live")
	}
	b, ok := p.scope.backend.(pauseBackend)
	if !ok {
		return ProcessPauseWitness{}, errors.New("pause backend unsupported")
	}
	w, err := b.pauseCommand(ctx, p.victim.id, op, j.identity, deadline)
	want := "LIVE"
	if op == "pause-stop" {
		want = "STOPPED"
	}
	if err == nil && (w.Identity != j.identity || w.ObservedAt.IsZero() || (op != "pause-observe" && w.State != want) || (w.State != "LIVE" && w.State != "STOPPED")) {
		err = errors.New("pause identity/state witness UNKNOWN")
	}
	return w, err
}

func (p *ownedPause) resume(ctx context.Context) (err error) {
	p.mu.Lock()
	prior := p.resumeErr
	p.mu.Unlock()
	if prior != nil {
		return prior
	}
	defer func() {
		if err != nil {
			p.mu.Lock()
			p.resumeErr = err
			p.receipt.Error = err.Error()
			p.mu.Unlock()
		}
	}()
	// Same serialized custody stream: this joins any prior STOP before CONT.
	if _, err = p.command(ctx, "pause-observe"); err != nil {
		return
	}
	w, err := p.command(ctx, "pause-resume")
	p.mu.Lock()
	p.receipt.Resumed = w
	p.mu.Unlock()
	return
}

func (p *ownedPause) verify(ctx context.Context) error {
	p.mu.Lock()
	prior := p.resumeErr
	resumed := p.receipt.Resumed
	p.mu.Unlock()
	if prior != nil {
		return prior
	}
	if resumed.State != "LIVE" {
		return errors.New("no proven CONT")
	}
	w, err := p.command(ctx, "pause-observe")
	if err == nil && w.State != "LIVE" {
		err = errors.New("final pause restoration UNKNOWN")
	}
	p.mu.Lock()
	p.receipt.Final = w
	p.mu.Unlock()
	return err
}

func (s *RunScope) joinPauseDispatches(ctx context.Context) error {
	s.mu.Lock()
	var ready []chan struct{}
	for _, p := range s.pauses {
		ready = append(ready, p.ready)
	}
	s.mu.Unlock()
	for _, r := range ready {
		select {
		case <-r:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (b *approvedJobs) pauseCommand(ctx context.Context, id, op string, i ProcessIdentity, deadline time.Time) (ProcessPauseWitness, error) {
	s := b.run.scope
	s.mu.Lock()
	p := s.pauses[id]
	s.mu.Unlock()
	if p == nil || p.receipt.Identity != i || !deadline.Equal(p.receipt.Plan.Deadline) || !deadline.After(time.Now()) || deadline.After(b.run.request.Binding.HardDeadline) {
		return ProcessPauseWitness{}, errors.New("missing/expired retained pause grant")
	}
	b.mu.Lock()
	row, ok := b.jobs[id]
	b.mu.Unlock()
	if !ok || row.identity != i || !sameJob(b.run.runPolicy.Jobs[p.receipt.Plan.Victim], row.spec) {
		return ProcessPauseWitness{}, errors.New("foreign pause target")
	}
	if op == "pause-bind" || op == "pause-stop" {
		if err := b.work(ctx, id, row.spec); err != nil {
			b.run.revoke(err)
			return ProcessPauseWitness{}, err
		}
	} else {
		found := false
		for _, d := range b.run.runPolicy.Deployments {
			if d.NodeID == i.Node {
				found = true
				if err := b.run.readback(ctx, d); err != nil {
					return ProcessPauseWitness{}, err
				}
			}
		}
		if !found {
			return ProcessPauseWitness{}, errors.New("pause node unbound")
		}
	}
	backend, ok := b.run.runtime.Jobs.(pauseBackend)
	if !ok {
		return ProcessPauseWitness{}, errors.New("pause backend unsupported")
	}
	return backend.pauseCommand(ctx, id, op, i, deadline)
}

func (b *SSHJobs) pauseCommand(ctx context.Context, id, op string, i ProcessIdentity, deadline time.Time) (ProcessPauseWitness, error) {
	b.mu.Lock()
	c := b.jobs[id]
	b.mu.Unlock()
	if c == nil {
		return ProcessPauseWitness{}, errors.New("unknown pause custody")
	}
	r, err := c.call(ctx, custodyRequest{Operation: op, Identity: i, PauseDeadline: deadline})
	return r.Pause, err
}
