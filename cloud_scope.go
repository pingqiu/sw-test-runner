package testrunner

// The fault custody of an ordinary cloud run of the block product (an exec
// pack on the seaweed-cloud controller). It is opened by its OWN constructor,
// bound to the run's pack context; the approved path's admitted workloads are
// not widened (NewRunScope still admits only kv and s3).
//
// One custody mechanism: the faults applied in this scope are restored by the
// scope's existing Close, in reverse order, even when the run fails. The
// scope starts no job: its backend refuses every one.
//
// The restore is the FAULT's custody, not the cost custody: a run that dies
// with a VM powered off is settled by seaweed-cloud's terminal destroy and
// verify, which alone guarantee that no VM is left billing.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"time"
)

// BlockCloudBinding is what the cloud run's pack context and inventory say
// about the run; the CLI reads them and the verify function re-checks them.
type BlockCloudBinding struct {
	RunID        string    // inventory run_id
	ContextPath  string    // the pack context file (SEAWEED_CLOUD_PACK_CONTEXT)
	OutputDir    string    // absolute directory for the run's outputs
	WorkDeadline time.Time // inventory deadlines.work
	HardDeadline time.Time // inventory deadlines.hard
	// RestoreReserve bounds every fault restore of the scope, explicit or at
	// cleanup; it must cover the slowest restore (a VM start).
	RestoreReserve time.Duration
}

// MinBlockRestoreReserve is the least restore reserve a block cloud scope
// accepts: a power fault restore (MinPowerRestoreTimeout) must fit.
const MinBlockRestoreReserve = MinPowerRestoreTimeout

// NewBlockCloudRunScope opens the fault custody of a block cloud run. verify
// re-reads the pack context and its inventory and must find them parseable
// and agreeing with b; any refusal opens nothing.
func NewBlockCloudRunScope(ctx context.Context, b BlockCloudBinding, verify func(context.Context, BlockCloudBinding) error) (*RunScope, error) {
	switch {
	case b.RunID == "" || verify == nil:
		return nil, errors.New("block cloud scope: a run id and a verifier are required")
	case !path.IsAbs(b.ContextPath) || !path.IsAbs(b.OutputDir) || b.OutputDir == "/" || path.Clean(b.OutputDir) != b.OutputDir:
		return nil, errors.New("block cloud scope: context and output paths must be absolute and clean")
	case b.RestoreReserve < MinBlockRestoreReserve:
		return nil, fmt.Errorf("block cloud scope: restore reserve %s is below %s", b.RestoreReserve, MinBlockRestoreReserve)
	case !b.WorkDeadline.After(time.Now()):
		return nil, errors.New("block cloud scope: the run's work deadline has passed")
	case b.WorkDeadline.Add(b.RestoreReserve).After(b.HardDeadline):
		return nil, fmt.Errorf("block cloud scope: work deadline + restore reserve %s passes the hard deadline", b.RestoreReserve)
	}
	raw, err := os.ReadFile(b.ContextPath)
	if err != nil {
		return nil, fmt.Errorf("block cloud scope: pack context: %w", err)
	}
	sum := sha256.Sum256(raw)
	a := RunAdmission{RunID: b.RunID, Workload: "block", RequestID: "cloud-" + b.RunID, ManifestSHA256: hex.EncodeToString(sum[:]),
		OutputDir: b.OutputDir, WorkDeadline: b.WorkDeadline, HardDeadline: b.HardDeadline, CleanupReserve: b.RestoreReserve}
	admitCtx, cancel := context.WithDeadline(ctx, b.WorkDeadline)
	err = verify(admitCtx, b)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("block cloud scope: admission: %w", err)
	}
	if ctx.Err() != nil || !b.WorkDeadline.After(time.Now()) {
		return nil, errors.New("block cloud scope: admission expired during verification")
	}
	work, workCancel := context.WithDeadline(ctx, b.WorkDeadline)
	s := &RunScope{admission: a, backend: refusingJobs{}, ctx: work, cancel: workCancel, jobs: make(map[string]*ownedJob),
		logs: make(map[string]bool), restores: make(map[string]*restoration), done: make(chan struct{})}
	go func() { <-work.Done(); _ = s.Close() }()
	return s, nil
}

// refusingJobs is the job backend of a block cloud scope: it starts nothing.
// Enroll answers ErrJobNotStarted, a proven pre-dispatch refusal, so no
// custody of a job is ever possible.
type refusingJobs struct{}

var errNoJobs = errors.New("a block cloud scope starts no jobs")

func (refusingJobs) Enroll(context.Context, string, RunAdmission, JobSpec) (ProcessIdentity, error) {
	return ProcessIdentity{}, fmt.Errorf("%w: %w", ErrJobNotStarted, errNoJobs)
}
func (refusingJobs) Authorize(context.Context, string, ProcessIdentity) error { return errNoJobs }
func (refusingJobs) Settle(context.Context, string) error                     { return errNoJobs }
func (refusingJobs) Wait(context.Context, string) (ExitInfo, error)           { return ExitInfo{}, errNoJobs }
func (refusingJobs) Kill(context.Context, string) (KillWitness, error) {
	return KillWitness{}, errNoJobs
}
