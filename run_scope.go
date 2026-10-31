package testrunner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"
	"time"
)

// RunAdmission binds the local envelope. ApprovedRun supplies verified S0b data.
type RunAdmission struct {
	RunID, Workload, RequestID, ManifestSHA256 string
	OutputDir                                  string
	WorkDeadline, HardDeadline                 time.Time
	// Optional earlier not-after for effects; HardDeadline still bounds custody.
	EffectDeadline time.Time
	// Optional tighter dispatch bound; unlike EffectDeadline it does not end
	// the lifetime of an already authorized background service.
	AuthorizeDeadline time.Time
	CleanupReserve    time.Duration
}

type ProcessIdentity struct {
	Node, BootID, RunID, ExecutableSHA256 string
	PID, PGID                             int
	StartTime                             uint64
}

type JobSpec struct {
	Node, Executable, SHA256 string
	StdoutPath, StderrPath   string
	Argv                     []string
	Restoration              bool   // Set only by the scope's cleanup path.
	CaptureOutput            bool   // Bounded owned-log export; never execution authority.
	packLabel                string // Private originating approved job label.
	retainExit               bool   // Private pack background ownership, never caller authority.
}

type ExitInfo struct {
	Identity               ProcessIdentity
	Code                   int
	Signal                 int
	StdoutPath, StderrPath string
	Settled                bool
	Output                 CapturedOutput
	// True only when the custodian observed the authorized leader still alive
	// at explicit cleanup and stopped it. Absence/SETTLED alone is not an exit.
	CleanupStopped bool
}

// CapturedOutput is immutable receipt data. An error is evidence loss, not a
// claim that a proven-settled process is still live. Base64 preserves all bytes.
type CapturedOutput struct {
	Complete                                               bool
	StdoutBase64, StderrBase64, StdoutSHA256, StderrSHA256 string
	Error                                                  string
	ReadStart, ReadEnd                                     time.Time
}

const MaxCapturedOutputBytes = 128 << 10 // Per stream, explicit opt-in only.

type KillWitness struct {
	Identity    ProcessIdentity
	AliveBefore bool
	Signal      string
	AppliedAt   time.Time
	// Controller-local CLOCK_MONOTONIC seconds; zero when absent. Compare only
	// within the qualified controller boot/clock domain, not the victim clock.
	PreKillMono float64
}

// Enroll is inert and retains possible issuers before dispatch. Authorize must
// atomically check an owned child/pidfd, not check-then-kill by PID. Settle and
// Wait.Settled prove issuer/descendant absence. Kill witnesses the exact live
// victim, not absence. Calls honor ctx. The concrete adapter independently
// enforces controller loss/deadlines; ApprovedRun supplies independent trust.
type OwnedJobBackend interface {
	Enroll(context.Context, string, RunAdmission, JobSpec) (ProcessIdentity, error)
	Authorize(context.Context, string, ProcessIdentity) error
	Settle(context.Context, string) error
	Wait(context.Context, string) (ExitInfo, error)
	Kill(context.Context, string) (KillWitness, error)
}

// ErrJobNotStarted is only for a proven pre-dispatch failure. All other
// enrollment errors retain POSSIBLE custody until settlement is proven.
var ErrJobNotStarted = errors.New("job NOT_STARTED")

type JobHandle struct {
	scope *RunScope
	id    string
	job   *ownedJob
}

type ownedJob struct {
	ready, gate  chan struct{}
	identity     ProcessIdentity
	spec         JobSpec
	settled      bool
	exit         *ExitInfo
	expectedKill *KillWitness          // Private, bound only by an approved generation restart.
	exitIntent   *GenerationExitIntent // Private pre-dispatch intent; never sufficient alone.
	outcomeErr   error                 // Validated terminal failure, not unsettled custody.
}

type restoration struct {
	ready, gate   chan struct{}
	restore       func(context.Context) error
	verify        func(context.Context) error
	cleared       bool
	finalVerified bool // Set only by Close after fresh restoration qualification; protected by gate.
	applyErr      error
	cleanupErr    error // First approved terminal failure; protected by gate.
}

type faultAuthority interface {
	bindFault(context.Context, string, WitnessedFault, FaultObservation) error
	bindFaultJob(string, string, bool, JobSpec) error
	faultDeadline(string) (time.Time, error)
	qualifyRestoration(context.Context, string) error
}
type faultJobContextKey struct{}
type faultJobContext struct {
	scope *RunScope
	key   string
}

// mu protects admission/indexes only. RPCs and callbacks never hold it.
type RunScope struct {
	mu              sync.Mutex
	admission       RunAdmission
	backend         OwnedJobBackend
	ctx             context.Context
	cancel          context.CancelFunc
	closed          bool
	jobs            map[string]*ownedJob
	logs            map[string]bool
	restores        map[string]*restoration
	order           []string
	once            sync.Once
	done            chan struct{}
	err             error
	outcomeErr      error // Sticky even after a proven-failed job is retired; mu.
	faultReceipts   map[string]FaultReceipt
	cleanupDeadline time.Time
	authority       faultAuthority
}

func NewRunScope(ctx context.Context, a RunAdmission, b OwnedJobBackend, verify func(context.Context, RunAdmission) error) (*RunScope, error) {
	if a.RunID == "" || a.RequestID == "" || (a.Workload != "kv" && a.Workload != "s3") || !validSHA256(a.ManifestSHA256) || b == nil || verify == nil {
		return nil, errors.New("incomplete run admission")
	}
	if a.CleanupReserve <= 0 || a.CleanupReserve > time.Minute || !a.WorkDeadline.After(time.Now()) || a.WorkDeadline.Add(a.CleanupReserve).After(a.HardDeadline) {
		return nil, errors.New("invalid absolute run/cleanup envelope")
	}
	if !path.IsAbs(a.OutputDir) || a.OutputDir == "/" || path.Clean(a.OutputDir) != a.OutputDir || strings.ContainsRune(a.OutputDir, 0) {
		return nil, errors.New("invalid run output directory")
	}
	admitCtx, admitCancel := context.WithDeadline(ctx, a.WorkDeadline)
	defer admitCancel()
	if err := verify(admitCtx, a); err != nil {
		return nil, fmt.Errorf("admission: %w", err)
	}
	if ctx.Err() != nil || !a.WorkDeadline.After(time.Now()) {
		return nil, errors.New("admission expired during verification")
	}
	work, cancel := context.WithDeadline(ctx, a.WorkDeadline)
	s := &RunScope{admission: a, backend: b, ctx: work, cancel: cancel, jobs: make(map[string]*ownedJob), logs: make(map[string]bool), restores: make(map[string]*restoration), done: make(chan struct{})}
	if authority, ok := b.(faultAuthority); ok {
		s.authority = authority
	}
	go func() { <-work.Done(); _ = s.Close() }()
	return s, nil
}

func validSHA256(v string) bool {
	b, err := hex.DecodeString(v)
	return err == nil && len(b) == 32
}

func (s *RunScope) Start(spec JobSpec) (JobHandle, error) {
	return s.start(s.ctx, spec, false)
}

func (s *RunScope) start(ctx context.Context, spec JobSpec, restoration bool) (JobHandle, error) {
	spec.Restoration = restoration
	if spec.Node == "" || !path.IsAbs(spec.Executable) || !validSHA256(spec.SHA256) {
		return JobHandle{}, errors.New("unbound job")
	}
	for _, arg := range append([]string{spec.Executable}, spec.Argv...) {
		if strings.ContainsRune(arg, 0) {
			return JobHandle{}, errors.New("NUL in argv")
		}
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return JobHandle{}, err
	}
	name := hex.EncodeToString(nonce[:])
	id := s.admission.RunID + "/" + name
	if spec.StdoutPath == "" {
		spec.StdoutPath = s.admission.OutputDir + "/" + name + ".stdout"
	}
	if spec.StderrPath == "" {
		spec.StderrPath = s.admission.OutputDir + "/" + name + ".stderr"
	}
	for _, p := range []string{spec.StdoutPath, spec.StderrPath} {
		if !strings.HasPrefix(p, s.admission.OutputDir+"/") || path.Clean(p) != p || strings.ContainsRune(p, 0) {
			return JobHandle{}, errors.New("log path outside run")
		}
	}
	if spec.StdoutPath == spec.StderrPath {
		return JobHandle{}, errors.New("shared stdout/stderr path")
	}
	spec.Argv = append([]string(nil), spec.Argv...)
	j := &ownedJob{ready: make(chan struct{}), gate: make(chan struct{}, 1), spec: spec}
	s.mu.Lock()
	if ((s.closed || s.ctx.Err() != nil) && !restoration) || ctx.Err() != nil {
		s.mu.Unlock()
		return JobHandle{}, errors.New("run admission closed")
	}
	if s.logs[spec.StdoutPath] || s.logs[spec.StderrPath] {
		s.mu.Unlock()
		return JobHandle{}, errors.New("log path already claimed")
	}
	s.logs[spec.StdoutPath], s.logs[spec.StderrPath] = true, true
	s.jobs[id] = j // POSSIBLE before the first backend call.
	s.mu.Unlock()
	defer close(j.ready)
	h := JobHandle{s, id, j}
	admission := s.admission
	if deadline, ok := ctx.Deadline(); ok && (admission.AuthorizeDeadline.IsZero() || deadline.Before(admission.AuthorizeDeadline)) {
		admission.AuthorizeDeadline = deadline
	}
	if s.authority != nil {
		binding, ok := ctx.Value(faultJobContextKey{}).(faultJobContext)
		if restoration && !ok {
			s.retired(h)
			return h, errors.Join(ErrJobNotStarted, errors.New("missing private restoration custody"))
		}
		if ok {
			if binding.scope != s {
				s.retired(h)
				return h, errors.Join(ErrJobNotStarted, errors.New("foreign fault custody"))
			}
			if err := s.authority.bindFaultJob(id, binding.key, restoration, spec); err != nil {
				s.retired(h)
				return h, errors.Join(ErrJobNotStarted, err)
			}
			if restoration {
				deadline, err := s.authority.faultDeadline(binding.key)
				if err != nil {
					s.retired(h)
					return h, errors.Join(ErrJobNotStarted, err)
				}
				var cancel context.CancelFunc
				ctx, cancel = context.WithDeadline(ctx, deadline)
				defer cancel()
				admission.EffectDeadline, _ = ctx.Deadline()
			}
		}
	}
	i, err := s.backend.Enroll(ctx, id, admission, spec)
	if err != nil {
		if errors.Is(err, ErrJobNotStarted) {
			s.retired(h)
			return h, err
		}
		return h, fmt.Errorf("enrollment UNKNOWN: %w", err)
	}
	if i.Node != spec.Node || i.RunID != s.admission.RunID || i.BootID == "" || i.PID <= 0 || i.PGID <= 0 || i.StartTime == 0 || i.ExecutableSHA256 != spec.SHA256 {
		return h, errors.New("enrolled identity does not match job")
	}
	j.identity = i
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if ((closed || s.ctx.Err() != nil) && !restoration) || ctx.Err() != nil {
		return h, errors.New("run closed before authorization")
	}
	return h, s.backend.Authorize(ctx, id, i)
}

// ready fences creation; gate serializes only this entry, with bounded waiting.
func take(ctx context.Context, ready, gate chan struct{}) error {
	select {
	case <-ready:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case gate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *RunScope) lockJob(ctx context.Context, h JobHandle) (*ownedJob, func(), error) {
	if h.scope != s || h.id == "" || h.job == nil {
		return nil, nil, errors.New("foreign job handle")
	}
	if err := take(ctx, h.job.ready, h.job.gate); err != nil {
		return nil, nil, err
	}
	return h.job, func() { <-h.job.gate }, nil
}

func (s *RunScope) retired(h JobHandle) {
	h.job.settled = true
	s.mu.Lock()
	delete(s.jobs, h.id)
	s.mu.Unlock()
}

func (s *RunScope) Stop(ctx context.Context, h JobHandle) error {
	custody, outcome := s.stop(ctx, h)
	return errors.Join(custody, outcome)
}

// Keep settlement uncertainty separate from an unacceptable proven outcome.
// Restoration barriers consume only custody; callers/final Close retain both.
func (s *RunScope) stop(ctx context.Context, h JobHandle) (custody, outcome error) {
	ctx, cancel := s.cleanupContext(ctx)
	defer cancel()
	j, unlock, err := s.lockJob(ctx, h)
	if err != nil {
		return err, nil
	}
	defer unlock()
	if j.settled {
		return nil, j.outcomeErr
	}
	if j.spec.retainExit {
		b, ok := s.backend.(interface {
			settleExit(context.Context, string) (ExitInfo, error)
		})
		if !ok {
			return errors.New("background terminal receipt unavailable"), nil
		}
		i, err := b.settleExit(ctx, h.id)
		if err != nil {
			return err, nil
		}
		if !validJobExit(i, j) {
			return errors.New("unproven background exit receipt"), nil
		}
		s.recordExit(h, j, i)
		return nil, j.outcomeErr
	}
	if err := s.backend.Settle(ctx, h.id); err != nil {
		return err, nil
	}
	if j.spec.CaptureOutput && j.spec.packLabel != "" {
		j.outcomeErr = errors.New("approved task terminal/output not observed before cleanup")
		s.mu.Lock()
		s.outcomeErr = errors.Join(s.outcomeErr, j.outcomeErr)
		s.mu.Unlock()
	}
	s.retired(h)
	return nil, j.outcomeErr
}

func (s *RunScope) Wait(ctx context.Context, h JobHandle) (ExitInfo, error) {
	ctx, cancel := context.WithDeadline(ctx, s.admission.HardDeadline)
	defer cancel()
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	j, unlock, err := s.lockJob(ctx, h)
	if err != nil {
		return ExitInfo{}, err
	}
	defer unlock()
	if j.exit != nil {
		return *j.exit, j.outcomeErr
	}
	if j.settled {
		return ExitInfo{}, errors.New("settled without exit receipt")
	}
	i, err := s.backend.Wait(ctx, h.id)
	if err != nil {
		return i, err
	}
	if !validJobExit(i, j) {
		return i, errors.New("unproven exit receipt")
	}
	s.recordExit(h, j, i)
	return i, j.outcomeErr
}

// Called under this job's gate. Persist the failed outcome before retirement,
// so a concurrent Close cannot miss it after taking its live-job snapshot.
func (s *RunScope) recordExit(h JobHandle, j *ownedJob, i ExitInfo) {
	j.exit = &i
	expected := j.exitIntent != nil && j.exitIntent.Identity == i.Identity && j.expectedKill != nil && j.expectedKill.Identity == i.Identity && i.Code == -1 && i.Signal == 9
	if j.spec.retainExit && !expected && (!i.CleanupStopped || i.Code != -1 || i.Signal != 9) {
		j.outcomeErr = errors.New("background service exited before intentional cleanup")
		s.mu.Lock()
		s.outcomeErr = errors.Join(s.outcomeErr, j.outcomeErr)
		s.mu.Unlock()
	}
	if j.spec.CaptureOutput && j.spec.packLabel != "" {
		_, _, err := i.Output.Decode()
		if i.Code != 0 || i.Signal != 0 || i.Output.ReadEnd.After(s.admission.HardDeadline) {
			err = errors.Join(err, errors.New("approved task terminal/output failed"))
		}
		j.outcomeErr = errors.Join(j.outcomeErr, err)
		s.mu.Lock()
		s.outcomeErr = errors.Join(s.outcomeErr, j.outcomeErr)
		s.mu.Unlock()
	}
	s.retired(h)
}

func (s *RunScope) failedOutcome() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.outcomeErr
}

func validJobExit(i ExitInfo, j *ownedJob) bool {
	valid := (i.Code >= 0 && i.Code <= 255 && i.Signal == 0) || (i.Code == -1 && i.Signal > 0 && i.Signal <= 64)
	return i.Settled && valid && j.identity.PID > 0 && i.Identity == j.identity && i.StdoutPath == j.spec.StdoutPath && i.StderrPath == j.spec.StderrPath
}

func (s *RunScope) Kill(ctx context.Context, h JobHandle) (KillWitness, error) {
	return s.kill(ctx, h, nil)
}

func (s *RunScope) kill(ctx context.Context, h JobHandle, intent *GenerationExitIntent) (KillWitness, error) {
	ctx, cancel := s.cleanupContext(ctx)
	defer cancel()
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	j, unlock, err := s.lockJob(ctx, h)
	if err != nil {
		return KillWitness{}, err
	}
	defer unlock()
	if j.settled {
		return KillWitness{}, errors.New("job already settled")
	}
	if j.identity.PID <= 0 {
		return KillWitness{}, errors.New("job identity UNKNOWN")
	}
	if s.ctx.Err() != nil {
		return KillWitness{}, s.ctx.Err()
	}
	preKill := 0.
	if intent != nil {
		if j.exitIntent != nil || intent.Identity != j.identity || intent.Episode == "" || intent.BoundAt.IsZero() {
			return KillWitness{}, errors.New("invalid or consumed generation exit intent")
		}
		bound := *intent
		j.exitIntent = &bound
		preKill, err = generationMonotonic()
		if err != nil {
			return KillWitness{}, err
		}
	}
	w, err := s.backend.Kill(ctx, h.id)
	w.PreKillMono = preKill
	if err != nil {
		return w, err
	}
	if w.Identity != j.identity || !w.AliveBefore || w.Signal != "KILL" || w.AppliedAt.IsZero() {
		return w, errors.New("unproven kill witness")
	}
	if intent != nil {
		j.expectedKill = &w
	}
	return w, nil // A signal alone never retires custody.
}

// Callbacks may Start/Stop other entries, but must not clear their own apply.
func (s *RunScope) ApplyFault(key string, apply, restore, verify func(context.Context) error) error {
	s.mu.Lock()
	if s.closed || s.ctx.Err() != nil {
		s.mu.Unlock()
		return errors.New("run admission closed")
	}
	if key == "" || apply == nil || restore == nil || verify == nil {
		s.mu.Unlock()
		return errors.New("incomplete fault obligation")
	}
	if _, exists := s.restores[key]; exists {
		s.mu.Unlock()
		return errors.New("conflicting fault resource")
	}
	r := &restoration{restore: restore, verify: verify, ready: make(chan struct{}), gate: make(chan struct{}, 1)}
	s.restores[key] = r
	s.order = append(s.order, key)
	s.mu.Unlock()
	defer close(r.ready)
	r.applyErr = cleanupCall(s.ctx, apply)
	return r.applyErr
}

func (s *RunScope) cleanupContext(parent context.Context) (context.Context, context.CancelFunc) {
	deadline := time.Now().Add(s.admission.CleanupReserve)
	s.mu.Lock()
	if !s.cleanupDeadline.IsZero() && deadline.After(s.cleanupDeadline) {
		deadline = s.cleanupDeadline
	}
	s.mu.Unlock()
	if deadline.After(s.admission.HardDeadline) {
		deadline = s.admission.HardDeadline
	}
	return context.WithDeadline(parent, deadline)
}

func (s *RunScope) ClearFault(ctx context.Context, key string) error {
	return errors.Join(s.clearFault(ctx, key, false), s.failedOutcome())
}

func (s *RunScope) clearFault(ctx context.Context, key string, final bool) error {
	ctx, cancel := s.cleanupContext(ctx)
	defer cancel()
	s.mu.Lock()
	r, ok := s.restores[key]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("unknown fault resource %q", key)
	}
	if err := take(ctx, r.ready, r.gate); err != nil {
		return err
	}
	defer func() { <-r.gate }()
	priorFinal := r.finalVerified
	r.finalVerified = false
	var restoreErr error
	if !r.cleared {
		restoreErr = cleanupCall(ctx, r.restore)
	}
	if err := errors.Join(restoreErr, cleanupCall(ctx, r.verify)); err != nil {
		if s.authority != nil && r.cleanupErr == nil {
			r.cleanupErr = err
		}
		return err
	}
	r.cleared = true
	// A successful always-phase recheck preserves an already final proof.
	// Failure above invalidates it; public clear cannot revive failed proof.
	r.finalVerified = final || priorFinal
	return r.cleanupErr
}

// Closing fences admission before this snapshot. A failed settlement prevents
// restoration: an UNKNOWN issuer could otherwise apply a late fault after UP.
func (s *RunScope) settleWork(ctx context.Context) error {
	s.mu.Lock()
	var jobs []JobHandle
	for id, j := range s.jobs {
		if !j.spec.Restoration {
			jobs = append(jobs, JobHandle{s, id, j})
		}
	}
	s.mu.Unlock()
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].id < jobs[j].id })
	var errs []error
	for _, h := range jobs {
		if err, _ := s.stop(ctx, h); err != nil {
			errs = append(errs, fmt.Errorf("settle issuer %s: %w", h.id, err))
		}
	}
	return errors.Join(errs...)
}

func cleanupCall(ctx context.Context, fn func(context.Context) error) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("cleanup panic: %v", p)
		}
	}()
	return fn(ctx)
}

// Early S0b ClearFault is a terminal cleanup transition, just like Close.
// Fence admission before taking the issuer snapshot; never reset its bound.
func (s *RunScope) beginCleanup() {
	s.mu.Lock()
	s.closed = true
	if s.cleanupDeadline.IsZero() {
		s.cleanupDeadline = time.Now().Add(s.admission.CleanupReserve)
		if s.cleanupDeadline.After(s.admission.HardDeadline) {
			s.cleanupDeadline = s.admission.HardDeadline
		}
	}
	s.mu.Unlock()
	s.cancel()
}

// Close closes admission before waiting for bounded per-entry creation/settlement.
func (s *RunScope) Close() error {
	s.once.Do(func() {
		defer close(s.done)
		s.beginCleanup()
		s.mu.Lock()
		order := append([]string(nil), s.order...)
		jobs := make([]JobHandle, 0, len(s.jobs))
		for id, j := range s.jobs {
			jobs = append(jobs, JobHandle{s, id, j})
		}
		s.mu.Unlock()
		ctx, cancel := s.cleanupContext(context.Background())
		defer cancel()
		var errs []error
		var issuerErr error
		if s.authority != nil {
			issuerErr = s.settleWork(ctx)
		}
		if issuerErr != nil {
			errs = append(errs, issuerErr, errors.New("restoration withheld: possible mutation issuer"))
		}
		for n := len(order) - 1; issuerErr == nil && n >= 0; n-- {
			if err := s.clearFault(ctx, order[n], true); err != nil {
				errs = append(errs, fmt.Errorf("restore %s: %w", order[n], err))
			}
			s.mu.Lock()
			r := s.restores[order[n]]
			s.mu.Unlock()
			select {
			case <-r.ready:
				if r.applyErr != nil {
					errs = append(errs, fmt.Errorf("apply %s: %w", order[n], r.applyErr))
				}
			default:
			}
		}
		sort.Slice(jobs, func(i, j int) bool { return jobs[i].id < jobs[j].id })
		for _, h := range jobs {
			if err := cleanupCall(ctx, func(ctx context.Context) error {
				custody, _ := s.stop(ctx, h)
				return custody
			}); err != nil {
				errs = append(errs, fmt.Errorf("settle %s: %w", h.id, err))
			}
		}
		s.err = errors.Join(errors.Join(errs...), s.failedOutcome())
	})
	<-s.done
	return s.err
}
