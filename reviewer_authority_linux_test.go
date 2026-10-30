//go:build linux

package testrunner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pingqiu/sw-test-runner/infra"
)

func TestReviewerClearFaultMustFenceAndSettleOtherIssuers(t *testing.T) {
	f, run, resource, _, plan := approvedFaultFixture(t, nil)
	if err := run.Scope().ApplyWitnessedFault(context.Background(), plan.Key(), plan); err != nil {
		t.Fatal(err)
	}
	if _, err := run.Scope().Start(JobSpec{Node: f.job.Node, Executable: f.job.Executable, SHA256: f.job.SHA256, Argv: f.job.Argv}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(f.dir, "go.json")); err != nil {
		t.Fatal(err)
	}
	err := run.Scope().ClearFault(context.Background(), plan.Key())
	resource.mu.Lock()
	premature := resource.restoredBeforeSettlement
	state := resource.state
	resource.mu.Unlock()
	t.Logf("ClearFault error=%v state=%s restored_before_other_issuer_settlement=%v", err, state, premature)
	if err == nil && premature {
		t.Fatal("restoration authorized while another owned issuer remained live")
	}
}

type reviewerLateRestore struct {
	OwnedJobBackend
	deadline  time.Time
	restoreID string
	late      bool
	envelope  time.Time
	dispatch  time.Time
}

func (b *reviewerLateRestore) Enroll(ctx context.Context, id string, a RunAdmission, s JobSpec) (ProcessIdentity, error) {
	if s.Restoration {
		b.restoreID = id
		b.envelope = a.HardDeadline
		b.dispatch, _ = ctx.Deadline()
	}
	return b.OwnedJobBackend.Enroll(ctx, id, a, s)
}
func (b *reviewerLateRestore) Authorize(ctx context.Context, id string, i ProcessIdentity) error {
	if id == b.restoreID {
		timer := time.NewTimer(time.Until(b.deadline.Add(30 * time.Millisecond)))
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return ctx.Err()
		}
		b.late = time.Now().After(b.deadline)
	}
	return b.OwnedJobBackend.Authorize(ctx, id, i)
}
func TestReviewerRestoreMustCarryGrantDeadlineToDispatch(t *testing.T) {
	var deadline time.Time
	_, run, resource, backend, plan := approvedFaultFixture(t, func(_ *approvedFixture, g *CleanupGrant) {
		deadline = time.Now().Add(700 * time.Millisecond)
		g.Deadline = deadline
	})
	if err := run.Scope().ApplyWitnessedFault(context.Background(), plan.Key(), plan); err != nil {
		t.Fatal(err)
	}
	late := &reviewerLateRestore{OwnedJobBackend: backend, deadline: deadline}
	run.runtime.Jobs = late
	err := run.Close()
	resource.mu.Lock()
	state := resource.state
	resource.mu.Unlock()
	t.Logf("Close error=%v late_authorize=%v state=%s grant=%s dispatch_ctx=%s remote_hard=%s", err, late.late, state, deadline.Format(time.RFC3339Nano), late.dispatch.Format(time.RFC3339Nano), late.envelope.Format(time.RFC3339Nano))
	if err == nil && late.late && state == "UP" {
		t.Fatal("clean restore authorized after independently approved grant deadline")
	}
}
func TestReviewerAlreadyBeforeMustValidateRetainedCustody(t *testing.T) {
	for _, mode := range []string{"expired", "bytes"} {
		t.Run(mode, func(t *testing.T) {
			var deadline time.Time
			_, run, resource, _, plan := approvedFaultFixture(t, func(_ *approvedFixture, g *CleanupGrant) {
				if mode == "expired" {
					deadline = time.Now().Add(350 * time.Millisecond)
					g.Deadline = deadline
				}
			})
			if err := run.Scope().ApplyWitnessedFault(context.Background(), plan.Key(), plan); err != nil {
				t.Fatal(err)
			}
			resource.mu.Lock()
			resource.state = "UP"
			resource.mu.Unlock()
			if mode == "expired" {
				time.Sleep(time.Until(deadline.Add(30 * time.Millisecond)))
			} else {
				old := run.runtime.Readback
				run.runtime.Readback = func(ctx context.Context, p infra.DeploymentPolicy) (infra.DeploymentReceipt, error) {
					r, e := old(ctx, p)
					r.Files[plan.Restore.Executable] = strings.Repeat("0", 64)
					return r, e
				}
			}
			err := run.Close()
			receipt := run.Scope().FaultReceipts()[plan.Key()]
			t.Logf("mode=%s Close error=%v RestoreNotNeeded=%v", mode, err, receipt.RestoreNotNeeded)
			if err == nil {
				t.Fatal(fmt.Sprintf("clean release bypassed %s retained-custody validation via AlreadyBefore", mode))
			}
		})
	}
}
