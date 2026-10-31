package testrunner

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"
)

type approvedJob struct {
	spec     JobSpec
	identity ProcessIdentity
	faultKey string
	restore  bool
}

func (a *ApprovedRun) jobReceipts() map[string]ApprovedJobReceipt {
	b := a.scope.backend.(*approvedJobs)
	b.mu.Lock()
	defer b.mu.Unlock()
	out := maps.Clone(b.receipts)
	for id, r := range out {
		r.Spec.Argv = append([]string(nil), r.Spec.Argv...)
		if r.Exit != nil {
			x := *r.Exit
			r.Exit = &x
		}
		out[id] = r
	}
	return out
}

type approvedJobs struct {
	mu       sync.Mutex
	run      *ApprovedRun
	jobs     map[string]approvedJob
	faults   map[string]registeredFault
	receipts map[string]ApprovedJobReceipt
}

// Final custody observations survive pruning live indexes. No evidence row is
// a substitute for the underlying backend's settlement proof.
type ApprovedJobReceipt struct {
	ID        string
	PackLabel string
	Spec      JobSpec
	Identity  ProcessIdentity
	State     string
	Updated   time.Time
	Exit      *ExitInfo
	Error     string
}

func (b *approvedJobs) observe(id, state string, i ProcessIdentity, exit *ExitInfo, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	r := b.receipts[id]
	r.ID, r.State, r.Updated = id, state, time.Now()
	if i.PID > 0 {
		r.Identity = i
	}
	if exit != nil {
		x := *exit
		r.Exit = &x
	}
	if err != nil {
		r.Error = err.Error()
	}
	b.receipts[id] = r
}

func sameJob(pin JobPin, s JobSpec) bool {
	return pin.Node == s.Node && pin.Executable == s.Executable && pin.SHA256 == s.SHA256 && slices.Equal(pin.Argv, s.Argv)
}

func (b *approvedJobs) work(ctx context.Context, id string, s JobSpec) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b.mu.Lock()
	row := b.jobs[id]
	fault, registered := b.faults[row.faultKey]
	b.mu.Unlock()
	if row.faultKey != "" {
		if !registered || !equalJSON(row.spec, s) {
			return errors.New("private fault job binding changed")
		}
		return b.permitFault(ctx, row, fault, false)
	}
	for _, grant := range b.run.approval.Cleanup {
		if sameJob(grant.Apply, s) || sameJob(grant.Restore, s) {
			return errors.New("fault command requires private obligation custody")
		}
	}
	allowed := false
	for _, pin := range b.run.runPolicy.Jobs {
		if sameJob(pin, s) {
			allowed = true
			break
		}
	}
	if !allowed || s.Restoration {
		return errors.New("job is not independently approved work")
	}
	if err := b.run.authority(ctx, 2*time.Second); err != nil {
		return err
	}
	for _, p := range b.run.runPolicy.Deployments {
		if p.NodeID == s.Node {
			return b.run.readback(ctx, p)
		}
	}
	return errors.New("job node is not approved")
}

func (b *approvedJobs) Enroll(ctx context.Context, id string, a RunAdmission, s JobSpec) (ProcessIdentity, error) {
	if err := b.work(ctx, id, s); err != nil {
		b.run.revoke(err)
		return ProcessIdentity{}, errors.Join(ErrJobNotStarted, err)
	}
	s.Argv = append([]string(nil), s.Argv...)
	b.mu.Lock()
	if b.receipts == nil {
		b.receipts = map[string]ApprovedJobReceipt{}
	}
	row := b.jobs[id]
	// Ordinary work cannot consume the evidence slots reserved for the at
	// most 128 independently registered cleanup obligations.
	if len(b.receipts) >= 10000 && (!row.restore || len(b.receipts) >= 10128) {
		b.mu.Unlock()
		return ProcessIdentity{}, errors.Join(ErrJobNotStarted, errors.New("custody evidence capacity exceeded"))
	}
	row.spec = s
	b.jobs[id] = row
	b.receipts[id] = ApprovedJobReceipt{ID: id, PackLabel: s.packLabel, Spec: s, State: "POSSIBLE", Updated: time.Now()}
	b.mu.Unlock()
	i, err := b.run.runtime.Jobs.Enroll(ctx, id, a, s)
	state := "ENROLLED"
	if err != nil {
		state = "UNKNOWN"
	}
	if errors.Is(err, ErrJobNotStarted) {
		state = "NOT_STARTED"
	}
	b.observe(id, state, i, nil, err)
	b.mu.Lock()
	row = b.jobs[id]
	row.identity = i
	b.jobs[id] = row
	if errors.Is(err, ErrJobNotStarted) {
		delete(b.jobs, id)
	}
	b.mu.Unlock()
	if err != nil {
		return i, err
	}
	b.run.mu.Lock()
	boot := b.run.readbacks[s.Node].BootID
	b.run.mu.Unlock()
	if i.BootID != boot {
		err = errors.New("custodian boot changed after readback")
		b.run.revoke(err)
		return i, err
	}
	return i, nil
}

func (b *approvedJobs) Authorize(ctx context.Context, id string, i ProcessIdentity) error {
	b.mu.Lock()
	row, ok := b.jobs[id]
	b.mu.Unlock()
	if !ok || row.identity != i {
		return errors.New("foreign enrolled identity")
	}
	if err := b.work(ctx, id, row.spec); err != nil {
		b.run.revoke(err)
		return err
	}
	err := b.run.runtime.Jobs.Authorize(ctx, id, i)
	state := "AUTHORIZED"
	if err != nil {
		state = "UNKNOWN"
	}
	b.observe(id, state, i, nil, err)
	return err
}
func (b *approvedJobs) Settle(ctx context.Context, id string) error {
	b.mu.Lock()
	_, ok := b.jobs[id]
	b.mu.Unlock()
	if !ok {
		return errors.New("unknown owned job")
	}
	if err := b.run.runtime.Jobs.Settle(ctx, id); err != nil {
		b.observe(id, "UNKNOWN", ProcessIdentity{}, nil, err)
		return err
	}
	b.observe(id, "SETTLED", ProcessIdentity{}, nil, nil)
	b.mu.Lock()
	delete(b.jobs, id)
	b.mu.Unlock()
	return nil
}

// Pack services retain their actual terminal reply through the same bounded
// settlement. Wait validates/records it before either live index is pruned.
func (b *approvedJobs) settleExit(ctx context.Context, id string) (ExitInfo, error) {
	if err := b.run.runtime.Jobs.Settle(ctx, id); err != nil {
		b.observe(id, "UNKNOWN", ProcessIdentity{}, nil, err)
		return ExitInfo{}, err
	}
	return b.Wait(ctx, id)
}
func (b *approvedJobs) Wait(ctx context.Context, id string) (ExitInfo, error) {
	b.mu.Lock()
	_, ok := b.jobs[id]
	b.mu.Unlock()
	if !ok {
		return ExitInfo{}, fmt.Errorf("unknown owned wait")
	}
	exit, err := b.run.runtime.Jobs.Wait(ctx, id)
	b.mu.Lock()
	row := b.jobs[id]
	validStatus := (exit.Code >= 0 && exit.Code <= 255 && exit.Signal == 0) || (exit.Code == -1 && exit.Signal > 0 && exit.Signal <= 64)
	if err == nil && exit.Settled && validStatus && exit.Identity == row.identity && exit.StdoutPath == row.spec.StdoutPath && exit.StderrPath == row.spec.StderrPath {
		delete(b.jobs, id)
	}
	b.mu.Unlock()
	state := "UNKNOWN"
	if err == nil && exit.Settled && validStatus && exit.Identity == row.identity && exit.StdoutPath == row.spec.StdoutPath && exit.StderrPath == row.spec.StderrPath {
		state = "EXIT_SETTLED"
	}
	b.observe(id, state, exit.Identity, &exit, err)
	return exit, err
}
func (b *approvedJobs) Kill(ctx context.Context, id string) (KillWitness, error) {
	b.mu.Lock()
	row, ok := b.jobs[id]
	b.mu.Unlock()
	if !ok {
		return KillWitness{}, errors.New("unknown owned kill")
	}
	if err := b.work(ctx, id, row.spec); err != nil {
		b.run.revoke(err)
		return KillWitness{}, err
	}
	return b.run.runtime.Jobs.Kill(ctx, id)
}
