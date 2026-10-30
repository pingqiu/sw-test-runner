//go:build linux

package testrunner

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
)

// Publish independent fixture records exactly as an operator would. Tests can
// approve a deliberately inconsistent policy to exercise semantic binding too.
func (f *approvedFixture) publish(t *testing.T, grants []CleanupGrant) {
	t.Helper()
	write := func(name string, v any) FilePin {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(f.dir, name)
		if err = os.WriteFile(p, raw, 0600); err != nil {
			t.Fatal(err)
		}
		return FilePin{p, fmt.Sprintf("%x", sha256.Sum256(raw))}
	}
	f.binding.Policy = write("run-policy.json", f.policy)
	f.binding.Artifacts = write("artifacts.json", ApprovedArtifacts{Version: "s0b-artifacts-v1", Deployments: f.policy.Deployments})
	request := write("running/r1.env", ClaimedRequest{Version: "s0b-request-v1", Binding: f.binding})
	approval := write("go.json", ApprovedGO{Version: "s0b-go-v1", SourceID: "0123456789abcdef01234567", RequestSHA256: request.SHA256, Binding: f.binding, Cleanup: grants, AllowMutating: len(grants) > 0})
	raw, err := os.ReadFile(f.profile)
	if err != nil {
		t.Fatal(err)
	}
	var profile PolicyBootstrap
	if err = json.Unmarshal(raw, &profile); err != nil {
		t.Fatal(err)
	}
	profile.Policy, profile.Approval = f.binding.Policy, approval
	write("profile.json", profile)
}

type virtualFault struct {
	mu                       sync.Mutex
	state                    string
	restoredBeforeSettlement bool
	ordinaryRestore          int
}
type faultOwnedBackend struct {
	*fakeOwnedNode
	resource       *virtualFault
	apply, restore JobPin
	next           int
}

func (b *faultOwnedBackend) Enroll(ctx context.Context, id string, a RunAdmission, s JobSpec) (ProcessIdentity, error) {
	i, err := b.fakeOwnedNode.Enroll(ctx, id, a, s)
	if err != nil {
		return i, err
	}
	b.mu.Lock()
	b.next++
	i.PID = 100 + b.next
	i.PGID = i.PID
	b.ids[id] = i
	b.mu.Unlock()
	return i, nil
}
func (b *faultOwnedBackend) Authorize(ctx context.Context, id string, i ProcessIdentity) error {
	if err := b.fakeOwnedNode.Authorize(ctx, id, i); err != nil {
		return err
	}
	b.mu.Lock()
	spec := b.specs[id]
	liveLoad := false
	for job := range b.ids {
		if len(b.specs[job].Argv) > 0 && b.specs[job].Argv[0] == "" {
			liveLoad = true
		}
	}
	b.mu.Unlock()
	b.resource.mu.Lock()
	defer b.resource.mu.Unlock()
	if slices.Equal(spec.Argv, b.apply.Argv) {
		b.resource.state = "DOWN"
	}
	if slices.Equal(spec.Argv, b.restore.Argv) {
		b.resource.state = "UP"
		b.resource.restoredBeforeSettlement = liveLoad
		if !spec.Restoration {
			b.resource.ordinaryRestore++
		}
	}
	return nil
}

func TestApprovedRestorationSurvivesLostWorkAuthority(t *testing.T) {
	for _, loss := range []string{"revoked", "unreadable"} {
		t.Run(loss, func(t *testing.T) {
			f := newApprovedFixture(t)
			apply, restore := f.job, f.job
			apply.Argv = []string{"apply"}
			restore.Argv = []string{"restore"}
			// Even if an executable also has an ordinary permission, the independent
			// fault registration must be what authorizes retained restoration.
			f.policy.Jobs["apply"], f.policy.Jobs["restore"] = apply, restore
			before, during := map[string]string{"owner": "run1", "link": "UP"}, map[string]string{"owner": "run1", "link": "DOWN"}
			grant := CleanupGrant{Kind: "partition", Node: "n1", Resource: "run1-private-port", Before: before, During: during, Apply: apply, Restore: restore, Deadline: f.binding.HardDeadline}
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
			defer run.Close()
			if _, err = run.Scope().Start(JobSpec{Node: f.job.Node, Executable: f.job.Executable, SHA256: f.job.SHA256, Argv: f.job.Argv}); err != nil {
				t.Fatal(err)
			} // outstanding load issuer
			plan := WitnessedFault{Kind: grant.Kind, Node: grant.Node, Resource: grant.Resource, Before: before, During: during, Apply: JobSpec{Node: apply.Node, Executable: apply.Executable, SHA256: apply.SHA256, Argv: apply.Argv}, Restore: JobSpec{Node: restore.Node, Executable: restore.Executable, SHA256: restore.SHA256, Argv: restore.Argv}, Observe: func(context.Context) (FaultObservation, error) {
				resource.mu.Lock()
				defer resource.mu.Unlock()
				return FaultObservation{Node: "n1", BootID: "boot", Resource: grant.Resource, State: map[string]string{"owner": "run1", "link": resource.state}}, nil
			}}
			if err = run.Scope().ApplyWitnessedFault(context.Background(), plan.Key(), plan); err != nil {
				t.Fatal(err)
			}
			resource.mu.Lock()
			active := resource.state == "DOWN"
			resource.mu.Unlock()
			if !active {
				t.Fatal("fault never became ACTIVE")
			}
			if loss == "revoked" {
				err = os.WriteFile(filepath.Join(f.dir, "cancel.json"), []byte("cancel"), 0600)
			} else {
				err = os.Remove(filepath.Join(f.dir, "go.json"))
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = run.Scope().Start(JobSpec{Node: f.job.Node, Executable: f.job.Executable, SHA256: f.job.SHA256, Argv: f.job.Argv}); err == nil {
				t.Fatal("ordinary work survived authority loss")
			}
			_ = run.Close() // the work verdict is failed; cleanup must still succeed
			resource.mu.Lock()
			defer resource.mu.Unlock()
			if resource.state != "UP" || resource.restoredBeforeSettlement || resource.ordinaryRestore != 0 {
				t.Fatalf("retained cleanup failed: state=%s restore-before-settle=%v ordinary=%d", resource.state, resource.restoredBeforeSettlement, resource.ordinaryRestore)
			}
			receipt := run.Scope().FaultReceipts()[plan.Key()]
			if receipt.Restored.State["link"] != "UP" || receipt.RestoreExit.Identity.BootID != "boot" {
				t.Fatalf("no exact restoration proof: %#v", receipt)
			}
		})
	}
}
