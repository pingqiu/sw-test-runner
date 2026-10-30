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

// RunAdmission binds the local envelope. Queue/GO integration remains S0b.
type RunAdmission struct {
	RunID, Workload, RequestID, ManifestSHA256 string
	OutputDir                                  string
	WorkDeadline, HardDeadline                 time.Time
	CleanupReserve                             time.Duration
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
	Restoration              bool // Set only by the scope's cleanup path.
}

type ExitInfo struct {
	Identity               ProcessIdentity
	Code                   int
	Signal                 int
	StdoutPath, StderrPath string
	Settled                bool
}

type KillWitness struct {
	Identity    ProcessIdentity
	AliveBefore bool
	Signal      string
	AppliedAt   time.Time
}

// Enroll is inert and retains possible issuers before dispatch. Authorize must
// atomically check an owned child/pidfd, not check-then-kill by PID. Settle and
// Wait.Settled prove issuer/descendant absence. Kill witnesses the exact live
// victim, not absence. Calls honor ctx. The concrete adapter independently
// enforces controller loss/deadlines; no production adapter is enabled yet.
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
	ready, gate chan struct{}
	identity    ProcessIdentity
	spec        JobSpec
	settled     bool
	exit        *ExitInfo
}

type restoration struct {
	ready, gate chan struct{}
	restore     func(context.Context) error
	verify      func(context.Context) error
	cleared     bool
	applyErr    error
}

// mu protects admission/indexes only. RPCs and callbacks never hold it.
type RunScope struct {
	mu            sync.Mutex
	admission     RunAdmission
	backend       OwnedJobBackend
	ctx           context.Context
	cancel        context.CancelFunc
	closed        bool
	jobs          map[string]*ownedJob
	logs          map[string]bool
	restores      map[string]*restoration
	order         []string
	once          sync.Once
	done          chan struct{}
	err           error
	faultReceipts map[string]FaultReceipt
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
	if (s.closed && !restoration) || ctx.Err() != nil {
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
	i, err := s.backend.Enroll(ctx, id, s.admission, spec)
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
	if (closed && !restoration) || ctx.Err() != nil {
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
	ctx, cancel := s.cleanupContext(ctx)
	defer cancel()
	j, unlock, err := s.lockJob(ctx, h)
	if err != nil {
		return err
	}
	defer unlock()
	if j.settled {
		return nil
	}
	if err := s.backend.Settle(ctx, h.id); err != nil {
		return err
	}
	s.retired(h)
	return nil
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
		return *j.exit, nil
	}
	if j.settled {
		return ExitInfo{}, errors.New("settled without exit receipt")
	}
	i, err := s.backend.Wait(ctx, h.id)
	if err != nil {
		return i, err
	}
	validExit := (i.Code >= 0 && i.Code <= 255 && i.Signal == 0) || (i.Code == -1 && i.Signal > 0 && i.Signal <= 64)
	if !i.Settled || !validExit || j.identity.PID <= 0 || i.Identity != j.identity || i.StdoutPath != j.spec.StdoutPath || i.StderrPath != j.spec.StderrPath {
		return i, errors.New("unproven exit receipt")
	}
	j.exit = &i
	s.retired(h)
	return i, nil
}

func (s *RunScope) Kill(ctx context.Context, h JobHandle) (KillWitness, error) {
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
	w, err := s.backend.Kill(ctx, h.id)
	if err != nil {
		return w, err
	}
	if w.Identity != j.identity || !w.AliveBefore || w.Signal != "KILL" || w.AppliedAt.IsZero() {
		return w, errors.New("unproven kill witness")
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
	if deadline.After(s.admission.HardDeadline) {
		deadline = s.admission.HardDeadline
	}
	return context.WithDeadline(parent, deadline)
}

func (s *RunScope) ClearFault(ctx context.Context, key string) error {
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
	var restoreErr error
	if !r.cleared {
		restoreErr = cleanupCall(ctx, r.restore)
	}
	if err := errors.Join(restoreErr, cleanupCall(ctx, r.verify)); err != nil {
		return err
	}
	r.cleared = true
	return nil
}

func cleanupCall(ctx context.Context, fn func(context.Context) error) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("cleanup panic: %v", p)
		}
	}()
	return fn(ctx)
}

// Close closes admission before waiting for bounded per-entry creation/settlement.
func (s *RunScope) Close() error {
	s.once.Do(func() {
		defer close(s.done)
		s.mu.Lock()
		s.closed = true
		order := append([]string(nil), s.order...)
		jobs := make([]JobHandle, 0, len(s.jobs))
		for id, j := range s.jobs {
			jobs = append(jobs, JobHandle{s, id, j})
		}
		s.mu.Unlock()
		s.cancel()
		ctx, cancel := s.cleanupContext(context.Background())
		defer cancel()
		var errs []error
		for n := len(order) - 1; n >= 0; n-- {
			if err := s.ClearFault(ctx, order[n]); err != nil {
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
			if err := cleanupCall(ctx, func(ctx context.Context) error { return s.Stop(ctx, h) }); err != nil {
				errs = append(errs, fmt.Errorf("settle %s: %w", h.id, err))
			}
		}
		s.err = errors.Join(errs...)
	})
	<-s.done
	return s.err
}
