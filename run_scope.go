package testrunner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// RunAdmission comes from the trusted queue adapter, never from YAML vars.
// Verify must bind the current request, workload, manifest and absolute deadline.
type RunAdmission struct {
	RunID, Workload, RequestID, ManifestSHA256 string
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
	Argv                     []string
}

// OwnedJobBackend belongs to the existing node/agent, not a new service.
// Enroll is inert: no workload executes until Authorize. The backend must retain
// id BEFORE dispatch, including lost replies, and independently settle on the
// hard deadline/controller loss. Authorize must atomically check the identity
// (pidfd/owned child, not check-then-kill by PID). Settle covers POSSIBLE issuers,
// descendants and changed generations; nil means independently proven absent.
// All calls must honor ctx. No production backend is enabled by this draft.
type OwnedJobBackend interface {
	Enroll(context.Context, string, RunAdmission, JobSpec) (ProcessIdentity, error)
	Authorize(context.Context, string, ProcessIdentity) error
	Settle(context.Context, string) error
}

// JobHandle cannot be forged from a PID or moved between runs.
type JobHandle struct {
	scope *RunScope
	id    string
}

type restoration struct {
	restore func(context.Context) error
	verify  func(context.Context) error
	cleared bool
}

// RunScope holds typed ownership outside ActionContext.Vars. The transport
// adapter remains responsible for durable/agent-side custody if this process dies.
type RunScope struct {
	mu        sync.Mutex
	admission RunAdmission
	backend   OwnedJobBackend
	ctx       context.Context
	cancel    context.CancelFunc
	closed    bool
	jobs      []string
	restores  map[string]restoration
	order     []string
	once      sync.Once
	done      chan struct{}
	err       error
}

func NewRunScope(ctx context.Context, a RunAdmission, b OwnedJobBackend, verify func(context.Context, RunAdmission) error) (*RunScope, error) {
	if a.RunID == "" || a.RequestID == "" || (a.Workload != "kv" && a.Workload != "s3") || !validSHA256(a.ManifestSHA256) || b == nil || verify == nil {
		return nil, errors.New("incomplete run admission")
	}
	if a.CleanupReserve <= 0 || a.CleanupReserve > time.Minute || !a.WorkDeadline.After(time.Now()) || a.WorkDeadline.Add(a.CleanupReserve).After(a.HardDeadline) {
		return nil, errors.New("invalid absolute run/cleanup envelope")
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
	s := &RunScope{admission: a, backend: b, ctx: work, cancel: cancel, restores: make(map[string]restoration), done: make(chan struct{})}
	go func() { <-work.Done(); _ = s.Close() }()
	return s, nil
}

func validSHA256(v string) bool {
	b, err := hex.DecodeString(v)
	return err == nil && len(b) == 32
}

func (s *RunScope) Start(spec JobSpec) (JobHandle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.ctx.Err() != nil {
		return JobHandle{}, errors.New("run admission closed")
	}
	if spec.Node == "" || !strings.HasPrefix(spec.Executable, "/") || !validSHA256(spec.SHA256) {
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
	id := s.admission.RunID + "/" + hex.EncodeToString(nonce[:])
	s.jobs = append(s.jobs, id) // POSSIBLE before the first backend call.
	h := JobHandle{s, id}
	spec.Argv = append([]string(nil), spec.Argv...)
	i, err := s.backend.Enroll(s.ctx, id, s.admission, spec)
	if err != nil {
		return h, fmt.Errorf("enrollment UNKNOWN: %w", err)
	}
	if i.Node != spec.Node || i.RunID != s.admission.RunID || i.BootID == "" || i.PID <= 0 || i.PGID <= 0 || i.StartTime == 0 || i.ExecutableSHA256 != spec.SHA256 {
		return h, errors.New("enrolled identity does not match job")
	}
	if s.ctx.Err() != nil {
		return h, s.ctx.Err()
	}
	return h, s.backend.Authorize(s.ctx, id, i)
}

func (s *RunScope) Stop(ctx context.Context, h JobHandle) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if h.scope != s || h.id == "" {
		return errors.New("foreign job handle")
	}
	// Keep the obligation: final Close independently settles even a stopped job.
	ctx, cancel := s.cleanupContext(ctx)
	defer cancel()
	return s.backend.Settle(ctx, h.id)
}

// ApplyFault enrolls the full restore obligation before applying any mutation.
// Keys name an exclusive resource (e.g. node/qdisc), not just the action type.
// A failed/partial apply remains pending; restore must be identity-safe and
// verify must observe state, not just trust a successful command exit.
func (s *RunScope) ApplyFault(key string, apply, restore, verify func(context.Context) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.ctx.Err() != nil {
		return errors.New("run admission closed")
	}
	if key == "" || apply == nil || restore == nil || verify == nil {
		return errors.New("incomplete fault obligation")
	}
	if _, exists := s.restores[key]; exists {
		return errors.New("conflicting fault resource")
	}
	s.restores[key] = restoration{restore: restore, verify: verify}
	s.order = append(s.order, key)
	return apply(s.ctx)
}

func (s *RunScope) ClearFault(ctx context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx, cancel := s.cleanupContext(ctx)
	defer cancel()
	return s.clearFault(ctx, key)
}

func (s *RunScope) cleanupContext(parent context.Context) (context.Context, context.CancelFunc) {
	deadline := time.Now().Add(s.admission.CleanupReserve)
	if deadline.After(s.admission.HardDeadline) {
		deadline = s.admission.HardDeadline
	}
	return context.WithDeadline(parent, deadline)
}

func (s *RunScope) clearFault(ctx context.Context, key string) error {
	r, ok := s.restores[key]
	if !ok {
		return fmt.Errorf("unknown fault resource %q", key)
	}
	if !r.cleared {
		if err := cleanupCall(ctx, r.restore); err != nil {
			return err
		}
	}
	if err := cleanupCall(ctx, r.verify); err != nil {
		return err
	}
	r.cleared = true
	s.restores[key] = r
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

// Close is independent of admission/queue health, closes admission first, and
// attempts every restoration and job even when one fails. UNKNOWN is an error.
func (s *RunScope) Close() error {
	s.once.Do(func() {
		defer close(s.done)
		s.cancel()
		s.mu.Lock()
		defer s.mu.Unlock()
		s.closed = true
		ctx, cancel := s.cleanupContext(context.Background())
		defer cancel()
		var errs []error
		for n := len(s.order) - 1; n >= 0; n-- {
			key := s.order[n]
			if _, ok := s.restores[key]; ok {
				if err := s.clearFault(ctx, key); err != nil {
					errs = append(errs, fmt.Errorf("restore %s: %w", key, err))
				}
			}
		}
		for _, id := range s.jobs {
			if err := cleanupCall(ctx, func(ctx context.Context) error { return s.backend.Settle(ctx, id) }); err != nil {
				errs = append(errs, fmt.Errorf("settle %s: %w", id, err))
			}
		}
		s.err = errors.Join(errs...)
	})
	<-s.done
	return s.err
}
