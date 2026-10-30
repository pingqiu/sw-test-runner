//go:build linux

package testrunner

import (
	"context"
	"errors"
	"github.com/pingqiu/sw-test-runner/infra"
	"os"
	"strings"
	"testing"
	"time"
)

func approvedFaultFixture(t *testing.T, alter func(*approvedFixture, *CleanupGrant)) (*approvedFixture, *ApprovedRun, *virtualFault, *faultOwnedBackend, WitnessedFault) {
	t.Helper()
	f := newApprovedFixture(t)
	apply, restore := f.job, f.job
	apply.Argv = []string{"apply"}
	restore.Argv = []string{"restore"}
	grant := CleanupGrant{Kind: "partition", Node: "n1", Resource: "run1-private-port", Before: map[string]string{"owner": "run1", "link": "UP"}, During: map[string]string{"owner": "run1", "link": "DOWN"}, Apply: apply, Restore: restore, Deadline: f.binding.HardDeadline}
	f.policy.Jobs["apply"], f.policy.Jobs["restore"] = apply, restore
	if alter != nil {
		alter(f, &grant)
	}
	f.publish(t, []CleanupGrant{grant})
	resource := &virtualFault{state: "UP"}
	backend := &faultOwnedBackend{fakeOwnedNode: f.node, resource: resource, apply: apply, restore: restore}
	f.runtime.Jobs = backend
	policy, err := ReadTrustedPolicy(f.profile, uint32(os.Getuid()))
	if err != nil {
		t.Fatal(err)
	}
	run, err := OpenApprovedRun(context.Background(), f.request, policy, &f.runtime)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Close() })
	plan := WitnessedFault{Kind: grant.Kind, Node: grant.Node, Resource: grant.Resource, Before: grant.Before, During: grant.During, Apply: JobSpec{Node: apply.Node, Executable: apply.Executable, SHA256: apply.SHA256, Argv: apply.Argv}, Restore: JobSpec{Node: restore.Node, Executable: restore.Executable, SHA256: restore.SHA256, Argv: restore.Argv}, Observe: func(context.Context) (FaultObservation, error) {
		resource.mu.Lock()
		defer resource.mu.Unlock()
		return FaultObservation{Node: "n1", BootID: "boot", Resource: grant.Resource, State: map[string]string{"owner": "run1", "link": resource.state}}, nil
	}}
	return f, run, resource, backend, plan
}

func TestApprovedRestoreCannotBeForgedByPublicData(t *testing.T) {
	for _, forge := range []string{"public-bit", "copied-command", "different-obligation", "absent-grant"} {
		t.Run(forge, func(t *testing.T) {
			f, run, _, _, plan := approvedFaultFixture(t, nil)
			if forge == "public-bit" || forge == "copied-command" {
				spec := plan.Restore
				spec.Restoration = forge == "public-bit"
				if _, err := run.Scope().Start(spec); err == nil {
					t.Fatal("public command created restore authority")
				}
			} else {
				if forge == "different-obligation" {
					plan.Resource = "foreign"
				} else {
					plan.Restore.Argv = []string{"foreign-restore"}
				}
				if err := run.Scope().ApplyWitnessedFault(context.Background(), plan.Key(), plan); err == nil {
					t.Fatal("unregistered fault applied")
				}
			}
			_ = run.Close()
			if f.node.authorized != 0 {
				t.Fatalf("forgery dispatched %d jobs", f.node.authorized)
			}
		})
	}
}

func TestApprovedRestorationRefusesChangedCustody(t *testing.T) {
	for _, change := range []string{"boot", "resource", "owner", "bytes", "expired", "already-before"} {
		t.Run(change, func(t *testing.T) {
			var alter func(*approvedFixture, *CleanupGrant)
			if change == "expired" {
				alter = func(_ *approvedFixture, g *CleanupGrant) { g.Deadline = time.Now().Add(300 * time.Millisecond) }
			}
			f, run, resource, _, plan := approvedFaultFixture(t, alter)
			observation := plan.Observe
			mutated := false
			plan.Observe = func(ctx context.Context) (FaultObservation, error) {
				o, err := observation(ctx)
				if mutated {
					switch change {
					case "boot":
						o.BootID = "foreign"
					case "resource":
						o.Resource = "foreign"
					case "owner":
						o.State["owner"] = "foreign"
					}
				}
				return o, err
			}
			if err := run.Scope().ApplyWitnessedFault(context.Background(), plan.Key(), plan); err != nil {
				t.Fatal(err)
			}
			mutated = true
			switch change {
			case "bytes":
				old := run.runtime.Readback
				run.runtime.Readback = func(ctx context.Context, p infra.DeploymentPolicy) (infra.DeploymentReceipt, error) {
					r, e := old(ctx, p)
					r.Files[plan.Restore.Executable] = strings.Repeat("0", 64)
					return r, e
				}
			case "expired":
				time.Sleep(310 * time.Millisecond)
			case "already-before":
				resource.mu.Lock()
				resource.state = "UP"
				resource.mu.Unlock()
			}
			err := run.Close()
			if change == "already-before" {
				if err != nil || !run.Scope().FaultReceipts()[plan.Key()].RestoreNotNeeded {
					t.Fatalf("already-Before not witnessed: %v", err)
				}
			} else if err == nil {
				t.Fatal("foreign/expired cleanup claimed clean release")
			}
			if f.node.authorized != 1 {
				t.Fatalf("unqualified restore dispatched: %d", f.node.authorized)
			}
		})
	}
}

type delayedPossibleIssuer struct {
	OwnedJobBackend
	entered, release chan struct{}
}

func (b delayedPossibleIssuer) Enroll(ctx context.Context, id string, a RunAdmission, s JobSpec) (ProcessIdentity, error) {
	i, e := b.OwnedJobBackend.Enroll(ctx, id, a, s)
	if len(s.Argv) > 0 && s.Argv[0] == "" {
		close(b.entered)
		<-b.release
	}
	return i, e
}

func TestApprovedDelayedPossibleIssuerWithholdsRestoration(t *testing.T) {
	f, run, resource, backend, plan := approvedFaultFixture(t, func(f *approvedFixture, _ *CleanupGrant) { f.binding.CleanupReserveMS = 150 })
	if err := run.Scope().ApplyWitnessedFault(context.Background(), plan.Key(), plan); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	run.runtime.Jobs = delayedPossibleIssuer{backend, entered, release}
	returned := make(chan error, 1)
	go func() {
		_, err := run.Scope().Start(JobSpec{Node: f.job.Node, Executable: f.job.Executable, SHA256: f.job.SHA256, Argv: f.job.Argv})
		returned <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("no enrollment intent")
	}
	err := run.Close()
	if err == nil || !strings.Contains(err.Error(), "possible mutation issuer") {
		t.Fatalf("UNKNOWN issuer claimed release: %v", err)
	}
	close(release)
	select {
	case err = <-returned:
		if err == nil {
			t.Fatal("late dispatch authorized")
		}
	case <-time.After(time.Second):
		t.Fatal("late enrollment did not return")
	}
	resource.mu.Lock()
	defer resource.mu.Unlock()
	if resource.state != "DOWN" || f.node.authorized != 1 {
		t.Fatalf("late issuer/restoration admitted: state=%s jobs=%d", resource.state, f.node.authorized)
	}
	// The fake has no real process; discard only its retained test identities.
	if errors.Is(err, ErrJobNotStarted) {
		t.Fatal("possibly dispatched job was downgraded to NOT_STARTED")
	}
}
