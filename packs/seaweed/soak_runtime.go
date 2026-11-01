package seaweed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path"
	"time"

	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/inventory"
)

// soakOps is the narrow set of existing ActionContext mechanisms the fault
// loop uses (contract r3: no scheduler, no new authority, no core API). The
// production adapter wraps *tr.ActionContext; CPU tests drive a fake.
type soakOps interface {
	StartTask(ctx context.Context, label string) (tr.JobHandle, error)
	WaitTask(ctx context.Context, h tr.JobHandle) (tr.ExitInfo, error)
	Restart(ctx context.Context, episode string, victim tr.JobHandle) (tr.JobHandle, tr.GenerationRestartReceipt, error)
	RunJob(ctx context.Context, label string) (tr.ExitInfo, error)
	// LoadStart returns the load worker's boot-scoped CLOCK_MONOTONIC start
	// witness, or an error if it is absent or foreign after WitnessWait.
	LoadStart(ctx context.Context) (float64, error)
	Mono() (float64, error) // controller CLOCK_MONOTONIC seconds (same M01 boot)
	Now() time.Time
	After(d time.Duration) <-chan time.Time
	WorkDeadline() time.Time
}

// loadLatch retains the one WaitApprovedPackTask result on the load task. It
// is set once, never consumed, and joined for the final output (R4).
type loadLatch struct {
	done chan struct{}
	exit tr.ExitInfo
	err  error
}

func observeLoad(ctx context.Context, ops soakOps, h tr.JobHandle) *loadLatch {
	l := &loadLatch{done: make(chan struct{})}
	go func() {
		l.exit, l.err = ops.WaitTask(ctx, h)
		close(l.done)
	}()
	return l
}

func (l *loadLatch) completed() bool {
	select {
	case <-l.done:
		return true
	default:
		return false
	}
}

// FaultRecord holds actual observations only, never planned times. A failed
// fault keeps its partial receipt (R1).
type FaultRecord struct {
	Episode      string                      `json:"episode"`
	Role         string                      `json:"role"`
	DueMono      float64                     `json:"due_mono"`
	DispatchMono float64                     `json:"dispatch_mono"`
	Receipt      tr.GenerationRestartReceipt `json:"receipt"`
	ReadyMono    float64                     `json:"ready_mono,omitempty"`
	AssignProbe  json.RawMessage             `json:"assign_probe,omitempty"`
	Error        string                      `json:"error,omitempty"`
}

var errLoadCompleted = errors.New("load completed before the fault schedule ended; no further fault")

// waitOrComplete waits d unless the load completes or ctx ends first.
// Observed completion always wins over a simultaneous timer (R4).
func waitOrComplete(ctx context.Context, ops soakOps, l *loadLatch, d time.Duration) error {
	if d > 0 {
		select {
		case <-l.done:
		case <-ops.After(d):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if l.completed() {
		return errLoadCompleted
	}
	return ctx.Err()
}

type readyRow struct {
	Status     string
	ClientMono float64 `json:"client_mono"`
	Processes  map[string]struct {
		Identity tr.ProcessIdentity `json:"scope_identity"`
	}
	AssignProbe json.RawMessage `json:"assign_probe"`
}

// runSoak runs the load and its fault schedule (contract r3). Every failure
// stops further faults; the caller fails the pack and existing owned cleanup
// runs. A replacement is only ever enrolled inside RestartApprovedPackGeneration
// under the live action context, never from cleanup or a detached context.
func runSoak(ctx context.Context, ops soakOps, p Plan, in tr.PackInputs, victims map[string]tr.JobHandle) (records []FaultRecord, exit tr.ExitInfo, err error) {
	x := p.Soak
	duration := secs(x.DurationS)
	// R3: remaining WORK at load admission; cleanup lies after WorkDeadline.
	if ops.WorkDeadline().Sub(ops.Now()) < duration+QuiescenceReserve+CollectReserve {
		return nil, exit, errors.New("soak: not enough work time left to admit the load")
	}
	h, err := ops.StartTask(ctx, p.Workload[0].Job)
	if err != nil {
		return nil, exit, err
	}
	latch := observeLoad(ctx, ops, h)
	wctx, cancel := context.WithTimeout(ctx, WitnessWait)
	start, err := ops.LoadStart(wctx)
	cancel()
	if err == nil && (math.IsNaN(start) || math.IsInf(start, 0) || start <= 0) {
		err = errors.New("soak: load-start witness not a positive finite time")
	}
	if err != nil {
		return nil, exit, fmt.Errorf("soak: load-start witness: %w", err)
	}
	if latch.completed() {
		return nil, exit, errLoadCompleted
	}
	for i, f := range x.Faults {
		due := start + float64(f.AtS)
		now, e := ops.Mono()
		if e != nil {
			return records, exit, e
		}
		if err = waitOrComplete(ctx, ops, latch, time.Duration((due-now)*float64(time.Second))); err != nil {
			return records, exit, err
		}
		if now, err = ops.Mono(); err != nil {
			return records, exit, err
		}
		if now > due+OverdueTolerance.Seconds() {
			return records, exit, errors.New("soak: fault overdue; no catch-up")
		}
		post := start + float64(x.DurationS) + QuiescenceReserve.Seconds() - now
		if i+1 < len(x.Faults) {
			post = start + float64(x.Faults[i+1].AtS) - now
		}
		if ops.WorkDeadline().Sub(ops.Now()) < FaultBudget+time.Duration(post*float64(time.Second))+CollectReserve {
			return records, exit, errors.New("soak: not enough work time left for this fault")
		}
		if latch.completed() { // the last observation before Kill
			return records, exit, errLoadCompleted
		}
		rec := FaultRecord{Episode: f.Episode, Role: f.Role, DueMono: due, DispatchMono: now}
		err = runFault(ctx, ops, p, in, f, victims[in.Restarts[f.Episode].Victim], &rec)
		records = append(records, rec)
		if err != nil {
			return records, exit, err
		}
		if err = waitOrComplete(ctx, ops, latch, RecoveryMargin); err != nil {
			return records, exit, err
		}
	}
	// The load must end by its duration plus the quiescence reserve; an overrun
	// fails instead of waiting without bound on the work deadline.
	now, err := ops.Mono()
	if err != nil {
		return records, exit, err
	}
	end := start + float64(x.DurationS) + QuiescenceReserve.Seconds()
	select {
	case <-latch.done:
	case <-ops.After(time.Duration((end - now) * float64(time.Second))):
		if !latch.completed() {
			return records, exit, errors.New("soak: load overran its duration plus quiescence")
		}
	case <-ctx.Done():
		return records, exit, ctx.Err()
	}
	if latch.err != nil {
		return records, latch.exit, latch.err
	}
	return records, latch.exit, nil
}

// runFault is one bounded restart plus the identity-checked READY probes.
func runFault(ctx context.Context, ops soakOps, p Plan, in tr.PackInputs, f SoakFault, victim tr.JobHandle, rec *FaultRecord) (err error) {
	defer func() {
		if err != nil {
			rec.Error = err.Error()
		}
	}()
	fctx, cancel := context.WithTimeout(ctx, FaultBudget)
	defer cancel()
	_, receipt, err := ops.Restart(fctx, f.Episode, victim)
	rec.Receipt = receipt // partial or complete, as the core recorded it (R1)
	if err != nil {
		return err
	}
	binary := "weed"
	if f.Role == "volume" {
		binary = "weed-volume"
	}
	for _, step := range p.Soak.FaultReady {
		e, err := ops.RunJob(fctx, step.Job)
		if err != nil {
			return err
		}
		raw, _, err := e.Output.Decode()
		if err != nil {
			return err
		}
		var ready readyRow
		if err = json.Unmarshal(raw, &ready); err != nil || ready.Status != "READY" {
			return errors.New("soak: replacement probe not READY")
		}
		if step.Host == "m01" {
			rec.ReadyMono = ready.ClientMono
		}
		if step.Host == "m02" {
			i := ready.Processes[f.Role].Identity
			if i.PID != receipt.Replacement.PID || i.StartTime != receipt.Replacement.StartTime || i.BootID != receipt.Replacement.BootID || i.ExecutableSHA256 != in.Files["m02"][path.Join(p.Smoke.HelperDir, "../bin/"+binary)] {
				return errors.New("soak: replacement readiness identity differs")
			}
		}
		if f.Role == "master" {
			var probe struct {
				Fid string `json:"fid"`
			}
			if len(ready.AssignProbe) == 0 || json.Unmarshal(ready.AssignProbe, &probe) != nil || probe.Fid == "" {
				return errors.New("soak: master READY without a successful Assign probe")
			}
			if step.Host == "m02" {
				rec.AssignProbe = ready.AssignProbe
			}
		}
	}
	if rec.ReadyMono <= 0 {
		return errors.New("soak: client readiness observation missing")
	}
	return nil
}

// soak is the seaweed_kv_soak phase. Until step 3 admits the worker it refuses
// before any effect, in addition to bind() refusing every soak plan.
func (r *runtimePack) soak(ctx context.Context, a *tr.ActionContext, p Plan, d *inventory.Document, in tr.PackInputs, s *session) (map[string]string, error) {
	return refusal("seaweed_kv_soak", "SOAK_WORKER_UNADMITTED", ErrSoakWorkerUnadmitted)
}
