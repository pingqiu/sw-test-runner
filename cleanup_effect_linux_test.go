//go:build linux

package testrunner

import (
	"context"
	"github.com/pingqiu/sw-test-runner/infra"
	"golang.org/x/sys/unix"
	"testing"
	"time"
)

// Actual native SSHJobs -> custodian -> gated child. The parent deliberately
// sends no stop at the effect deadline; the custodian must enforce it itself.
func TestCleanupEffectDeadlineStopsChildButRetainsSettlement(t *testing.T) {
	scope, backend, spec := nativeCustodyScope(t)
	admission := scope.admission
	admission.EffectDeadline = time.Now().Add(3 * time.Second)
	spec.Restoration = true
	spec.Argv = []string{"s0a-empty-workload"}
	spec.StdoutPath = admission.OutputDir + "/effect.stdout"
	spec.StderrPath = admission.OutputDir + "/effect.stderr"
	id := admission.RunID + "/effect"
	ctx, cancel := context.WithDeadline(context.Background(), admission.EffectDeadline)
	defer cancel()
	identity, err := backend.Enroll(ctx, id, admission, spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = backend.Settle(ctx, id)
	})
	fd, err := unix.PidfdOpen(identity.PID, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if err = backend.Authorize(ctx, id, identity); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Until(admission.EffectDeadline.Add(30 * time.Millisecond)))
	if count, err := unix.Poll([]unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}, 1500); err != nil || count != 1 {
		t.Fatalf("custodian did not enforce the shorter effect deadline: count=%d error=%v", count, err)
	}
	// Settlement uses retained original custody, not the already expired effect.
	settlement, cancelSettlement := context.WithDeadline(context.Background(), admission.HardDeadline)
	defer cancelSettlement()
	if err = backend.Authorize(settlement, id, identity); err == nil {
		t.Fatal("expired effect was reauthorized")
	}
	if err = backend.Settle(settlement, id); err != nil {
		t.Fatalf("effect expiry discarded settlement custody: %v", err)
	}
	if !time.Now().Before(admission.HardDeadline) {
		t.Fatal("settlement extended original envelope")
	}
}

func TestCleanupEffectDeadlineRefusesUnacknowledgedCustodian(t *testing.T) {
	t.Setenv("S0A_TEST_CUSTODIAN", "fake") // old-shaped reply omits effect bound
	backend := &SSHJobs{Nodes: map[string]*infra.Node{"fake": {IsNative: true}}, Runner: custodyFixtureRunner(t)}
	admission := testAdmission()
	admission.EffectDeadline = time.Now().Add(3 * time.Second)
	spec := testJob()
	spec.Restoration = true
	id := admission.RunID + "/old-effect-peer"
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = backend.Settle(ctx, id)
	})
	_, err := backend.Enroll(context.Background(), id, admission, spec)
	if err == nil {
		t.Fatal("old custodian with no deadline echo could be authorized")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err = backend.Settle(ctx, id); err != nil {
		t.Fatalf("unacknowledged inert peer was discarded: %v", err)
	}
}

func TestCleanupClearFaultIsTerminalForApprovedScope(t *testing.T) {
	f, run, _, _, plan := approvedFaultFixture(t, nil)
	if err := run.Scope().ApplyWitnessedFault(context.Background(), plan.Key(), plan); err != nil {
		t.Fatal(err)
	}
	if err := run.Scope().ClearFault(context.Background(), plan.Key()); err != nil {
		t.Fatal(err)
	}
	if _, err := run.Scope().Start(JobSpec{Node: f.job.Node, Executable: f.job.Executable, SHA256: f.job.SHA256, Argv: f.job.Argv}); err == nil {
		t.Fatal("ClearFault reopened ordinary admission after the issuer barrier")
	}
}

type cleanupAdmissionCapture struct {
	OwnedJobBackend
	effect, hard, dispatch time.Time
}

func (b *cleanupAdmissionCapture) Enroll(ctx context.Context, id string, a RunAdmission, s JobSpec) (ProcessIdentity, error) {
	if s.Restoration {
		b.effect, b.hard = a.EffectDeadline, a.HardDeadline
		b.dispatch, _ = ctx.Deadline()
	}
	return b.OwnedJobBackend.Enroll(ctx, id, a, s)
}
func TestCleanupGrantReachesCustodianAdmission(t *testing.T) {
	var grant time.Time
	_, run, _, backend, plan := approvedFaultFixture(t, func(_ *approvedFixture, g *CleanupGrant) { grant = time.Now().Add(2 * time.Second); g.Deadline = grant })
	if err := run.Scope().ApplyWitnessedFault(context.Background(), plan.Key(), plan); err != nil {
		t.Fatal(err)
	}
	capture := &cleanupAdmissionCapture{OwnedJobBackend: backend}
	run.runtime.Jobs = capture
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	if capture.effect.IsZero() || capture.effect.After(grant) || capture.dispatch.IsZero() || capture.dispatch.After(grant) || !capture.hard.Equal(run.scope.admission.HardDeadline) {
		t.Fatalf("effect/dispatch/settlement bounds not separated: grant=%s effect=%s dispatch=%s settlement=%s", grant, capture.effect, capture.dispatch, capture.hard)
	}
}
