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
	// The fault-window handshake (addendum r4): the adapter binds run, M01
	// boot, role and episode into each exclusive file and checks them on the ack.
	Pause(episode, role string, pauseMono float64) error
	Ack(ctx context.Context, episode string) (SoakAck, error)
	Close(episode string, endMono float64) error
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

// SoakAck is the worker's fault-window acknowledgement (addendum r4).
type SoakAck struct {
	PauseMono    float64 `json:"pause_mono"`
	ObservedMono float64 `json:"observed_mono"`
	AckMono      float64 `json:"ack_mono"`
	InflightSets int     `json:"inflight_sets"`
	PendingGETs  []struct {
		Op        string  `json:"op"`
		Submitted float64 `json:"submitted"`
	} `json:"pending_gets"`
}

// pauseAndAck pauses the worker and validates its exact acknowledgement
// within AckWait; anything else means no Kill.
func pauseAndAck(ctx context.Context, ops soakOps, clk *soakClock, f SoakFault) (SoakAck, error) {
	pause, err := clk.now()
	if err == nil {
		err = ops.Pause(f.Episode, f.Role, pause)
	}
	if err != nil {
		return SoakAck{}, err
	}
	actx, cancel := context.WithTimeout(ctx, AckWait)
	defer cancel()
	a, err := ops.Ack(actx, f.Episode)
	now, nerr := clk.now()
	if err = errors.Join(err, nerr, actx.Err()); err != nil {
		return a, err
	}
	if now-pause > AckWait.Seconds() { // a late ack never earns a Kill (codex01 R3)
		return a, errors.New("soak: fault-window ack later than AckWait; no Kill")
	}
	seen := map[string]bool{}
	ok := a.PauseMono == pause && pause <= a.ObservedMono && a.ObservedMono <= a.AckMono && a.AckMono <= now && a.InflightSets == 0
	for _, g := range a.PendingGETs {
		ok = ok && g.Op != "" && !seen[g.Op] && g.Submitted > 0 && g.Submitted <= a.ObservedMono
		seen[g.Op] = true
	}
	if !ok { // NaN fails every comparison above
		return a, errors.New("soak: fault-window ack not exact; no Kill")
	}
	return a, nil
}

// FaultRecord holds actual observations only, never planned times. A failed
// fault keeps its partial receipt (R1).
type FaultRecord struct {
	Episode      string                      `json:"episode"`
	Role         string                      `json:"role"`
	DueMono      float64                     `json:"due_mono"`
	DispatchMono float64                     `json:"dispatch_mono"` // P: the fault start, right before the Kill
	Ack          *SoakAck                    `json:"ack,omitempty"`
	Receipt      tr.GenerationRestartReceipt `json:"receipt"`
	Ready        []ReadyRef                  `json:"ready,omitempty"`
	ReadyMono    float64                     `json:"ready_mono,omitempty"`
	AssignProbe  json.RawMessage             `json:"assign_probe,omitempty"`
	Error        string                      `json:"error,omitempty"`
}

// ReadyRef is the exact receipt reference of one fault READY probe, kept
// even when the probe or its validation fails (codex01 R4).
type ReadyRef struct {
	Job          string `json:"job"`
	Host         string `json:"host"`
	Code         int    `json:"code"`
	StdoutSHA256 string `json:"stdout_sha256,omitempty"`
	Error        string `json:"error,omitempty"`
}

// checkedMono reads the controller clock and refuses a non-finite or
// non-positive value before anything is derived from it.
func checkedMono(ops soakOps) (float64, error) {
	m, err := ops.Mono()
	if err == nil && (math.IsNaN(m) || math.IsInf(m, 0) || m <= 0) {
		err = errors.New("soak: controller clock not a positive finite time")
	}
	return m, err
}

// soakClock refuses a controller clock that steps back from the last
// validated read (codex01: a finite, positive 1001 -> 1000 regression after a
// timer still dispatched faults). It claims no more than that.
type soakClock struct {
	ops  soakOps
	last float64
}

func (c *soakClock) now() (float64, error) {
	m, err := checkedMono(c.ops)
	if err == nil && m < c.last {
		err = errors.New("soak: controller clock went backwards")
	}
	if err == nil {
		c.last = m
	}
	return m, err
}

// checkedSpan converts seconds to a Duration only inside the soak horizon,
// so no float can wrap the conversion. A past (negative) span waits 0 and is
// judged by the overdue rule as before.
func checkedSpan(s float64) (time.Duration, error) {
	if math.IsNaN(s) || s < -SoakHorizon.Seconds() || s > SoakHorizon.Seconds() {
		return 0, errors.New("soak: derived wait outside the soak horizon")
	}
	return time.Duration(max(s, 0) * float64(time.Second)), nil
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
	ReadyMono  float64 `json:"ready_mono"`
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
	if err = x.checkSchedule(); err != nil { // before any arithmetic or effect
		return nil, exit, err
	}
	duration := secs(x.DurationS)
	// R3: remaining WORK at load admission; cleanup lies after WorkDeadline.
	if ops.WorkDeadline().Sub(ops.Now()) < duration+QuiescenceReserve+CollectReserve {
		return nil, exit, errors.New("soak: not enough work time left to admit the load")
	}
	clk := &soakClock{ops: ops}
	enrolled, err := clk.now()
	if err != nil {
		return nil, exit, err
	}
	h, err := ops.StartTask(ctx, p.Workload[0].Job)
	if err != nil {
		return nil, exit, err
	}
	latch := observeLoad(ctx, ops, h)
	wctx, cancel := context.WithTimeout(ctx, WitnessWait)
	start, err := ops.LoadStart(wctx)
	cancel()
	if err != nil {
		return nil, exit, fmt.Errorf("soak: load-start witness: %w", err)
	}
	// The witness must lie inside this boot's enrollment/acquisition bracket
	// (codex01 R1): written after the task was enrolled, read back no later
	// than now. Anything else, MaxFloat64 included, is refused.
	acquired, err := clk.now()
	if err != nil {
		return nil, exit, err
	}
	if !(start >= enrolled && start <= acquired) {
		return nil, exit, errors.New("soak: load-start witness outside its enrollment bracket")
	}
	if latch.completed() {
		return nil, exit, errLoadCompleted
	}
	for i, f := range x.Faults {
		due := start + float64(f.AtS)
		now, e := clk.now()
		if e != nil {
			return records, exit, e
		}
		// Never dispatch before the due time: a timer that fired early is
		// waited out again, a bounded number of times.
		for tries := 0; now < due; tries++ {
			if tries == 3 {
				return records, exit, errors.New("soak: fault timer fired early repeatedly")
			}
			wait, e := checkedSpan(due - now)
			if e != nil {
				return records, exit, e
			}
			if err = waitOrComplete(ctx, ops, latch, wait); err != nil {
				return records, exit, err
			}
			if now, err = clk.now(); err != nil {
				return records, exit, err
			}
		}
		if now > due+OverdueTolerance.Seconds() {
			return records, exit, errors.New("soak: fault overdue; no catch-up")
		}
		post := start + float64(x.DurationS) + QuiescenceReserve.Seconds() - now
		if i+1 < len(x.Faults) {
			post = start + float64(x.Faults[i+1].AtS) - now
		}
		postSpan, e := checkedSpan(post)
		if e != nil {
			return records, exit, e
		}
		if ops.WorkDeadline().Sub(ops.Now()) < FaultBudget+postSpan+CollectReserve {
			return records, exit, errors.New("soak: not enough work time left for this fault")
		}
		ack, err := pauseAndAck(ctx, ops, clk, f)
		if err != nil { // no valid ack: no Kill
			return records, exit, err
		}
		if latch.completed() { // the last observation before Kill
			return records, exit, errLoadCompleted
		}
		if now, err = clk.now(); err != nil { // P: immediately before the Kill
			return records, exit, err
		}
		rec := FaultRecord{Episode: f.Episode, Role: f.Role, DueMono: due, DispatchMono: now, Ack: &ack}
		err = runFault(ctx, ops, p, in, f, victims[in.Restarts[f.Episode].Victim], &rec)
		records = append(records, rec)
		if err != nil {
			return records, exit, err
		}
		// Close only while WORK is live; cancel, revoke or error is sticky.
		if err = ctx.Err(); err != nil || !ops.Now().Before(ops.WorkDeadline()) {
			return records, exit, errors.Join(err, errors.New("soak: WORK not live; no close"))
		}
		if now, err = clk.now(); err != nil {
			return records, exit, err
		}
		if err = ops.Close(f.Episode, now); err != nil {
			return records, exit, err
		}
		if err = waitOrComplete(ctx, ops, latch, RecoveryMargin); err != nil {
			return records, exit, err
		}
	}
	// The load must end by its duration plus the quiescence reserve; an overrun
	// fails instead of waiting without bound on the work deadline.
	now, err := clk.now()
	if err != nil {
		return records, exit, err
	}
	final, err := checkedSpan(start + float64(x.DurationS) + QuiescenceReserve.Seconds() - now)
	if err != nil {
		return records, exit, err
	}
	select {
	case <-latch.done:
	case <-ops.After(final):
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
	fctx, cancel := context.WithTimeout(ctx, FaultBudget-AckWait)
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
	// One shared 45 s READY stage for all probes of this fault, inside the
	// fault budget (codex01 R4), not the whole fault budget per probe.
	rctx, rcancel := context.WithTimeout(fctx, ReadyBound)
	defer rcancel()
	for _, step := range p.Soak.FaultReady {
		ref := ReadyRef{Job: step.Job, Host: step.Host}
		e, err := ops.RunJob(rctx, step.Job)
		ref.Code, ref.StdoutSHA256 = e.Code, e.Output.StdoutSHA256
		if err != nil {
			ref.Error = err.Error()
			rec.Ready = append(rec.Ready, ref)
			return err
		}
		rec.Ready = append(rec.Ready, ref)
		raw, _, err := e.Output.Decode()
		if err != nil {
			return err
		}
		var ready readyRow
		if err = json.Unmarshal(raw, &ready); err != nil || ready.Status != "READY" {
			return errors.New("soak: replacement probe not READY")
		}
		if step.Host == "m01" {
			// K7 is measured from the actual successful observation, not the
			// probe entry (codex01): the Assign's own time after a master
			// fault, the all-gates-passed time otherwise.
			rec.ReadyMono = ready.ReadyMono
			if f.Role == "master" {
				var probe struct {
					Mono float64 `json:"mono"`
				}
				_ = json.Unmarshal(ready.AssignProbe, &probe)
				rec.ReadyMono = probe.Mono
			}
			if math.IsNaN(rec.ReadyMono) || math.IsInf(rec.ReadyMono, 0) || rec.ReadyMono < ready.ClientMono {
				return errors.New("soak: client READY time not a successful observation after probe entry")
			}
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
