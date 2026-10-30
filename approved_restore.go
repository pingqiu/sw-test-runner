package testrunner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"strings"
	"time"
)

type registeredFault struct {
	grant            CleanupGrant
	plan             WitnessedFault
	boot, planSHA256 string
}

func (b *approvedJobs) faultDeadline(key string) (time.Time, error) {
	b.mu.Lock()
	f, ok := b.faults[key]
	b.mu.Unlock()
	if !ok || !f.grant.Deadline.After(time.Now()) || f.grant.Deadline.After(b.run.request.Binding.HardDeadline) {
		return time.Time{}, errors.New("missing/expired original cleanup grant")
	}
	return f.grant.Deadline, nil
}

func (b *approvedJobs) bindFault(ctx context.Context, key string, p WitnessedFault, before FaultObservation) error {
	if !b.run.approval.AllowMutating {
		return errors.New("independent GO does not consent to mutation")
	}
	if err := b.run.authority(ctx, 2*time.Second); err != nil {
		b.run.revoke(err)
		return err
	}
	var grant CleanupGrant
	found := false
	for _, g := range b.run.approval.Cleanup {
		if g.Node+"/"+g.Resource != key {
			continue
		}
		if found {
			return errors.New("duplicate retained cleanup grant")
		}
		grant = g
		found = true
	}
	if !found || grant.Kind != p.Kind || grant.Node != p.Node || grant.Resource != p.Resource || !maps.Equal(grant.Before, p.Before) || !maps.Equal(grant.During, p.During) || !sameJob(grant.Apply, p.Apply) || !sameJob(grant.Restore, p.Restore) || !grant.Deadline.After(time.Now()) || grant.Deadline.After(b.run.request.Binding.HardDeadline) {
		return errors.New("fault lacks exact independent retained cleanup grant")
	}
	b.run.mu.Lock()
	boot := b.run.readbacks[p.Node].BootID
	b.run.mu.Unlock()
	if before.BootID != boot || before.Node != p.Node || before.Resource != p.Resource || !maps.Equal(before.State, grant.Before) {
		return errors.New("qualified fault baseline differs from deployment")
	}
	p.Before, p.During = maps.Clone(p.Before), maps.Clone(p.During)
	p.Apply.Argv, p.Restore.Argv = append([]string(nil), p.Apply.Argv...), append([]string(nil), p.Restore.Argv...)
	encoded, _ := json.Marshal(struct {
		Grant                         CleanupGrant
		RunID, RequestSHA, Root, Boot string
	}{grant, b.run.request.Binding.RunID, b.run.request.SHA256, b.run.request.Binding.OutputDir, boot})
	sum := sha256.Sum256(encoded)
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, exists := b.faults[key]; exists {
		return errors.New("fault obligation already registered")
	}
	b.faults[key] = registeredFault{grant: grant, plan: p, boot: boot, planSHA256: hex.EncodeToString(sum[:])}
	return nil
}

func (b *approvedJobs) bindFaultJob(id, key string, restore bool, s JobSpec) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	fault, ok := b.faults[key]
	if !ok || !strings.HasPrefix(id, b.run.request.Binding.RunID+"/") {
		return errors.New("unregistered/foreign fault custody")
	}
	pin := fault.grant.Apply
	if restore {
		pin = fault.grant.Restore
	}
	if !sameJob(pin, s) || s.Restoration != restore || !fault.grant.Deadline.After(time.Now()) {
		return errors.New("fault job differs from retained grant or expired")
	}
	if _, exists := b.jobs[id]; exists {
		return errors.New("duplicate custody job ID")
	}
	b.jobs[id] = approvedJob{spec: s, faultKey: key, restore: restore}
	return nil
}

func (b *approvedJobs) qualifyRestoration(ctx context.Context, key string) error {
	b.mu.Lock()
	f, ok := b.faults[key]
	b.mu.Unlock()
	if !ok {
		return errors.New("missing retained cleanup custody")
	}
	return b.permitFault(ctx, approvedJob{spec: f.plan.Restore, restore: true}, f, true)
}

func (b *approvedJobs) permitFault(ctx context.Context, row approvedJob, f registeredFault, observationOnly bool) error {
	if !f.grant.Deadline.After(time.Now()) || f.grant.Deadline.After(b.run.request.Binding.HardDeadline) {
		return errors.New("original cleanup grant expired")
	}
	ctx, cancel := context.WithDeadline(ctx, f.grant.Deadline)
	defer cancel()
	if !row.restore {
		if err := b.run.authority(ctx, 2*time.Second); err != nil {
			return err
		}
	}
	// Restoration deliberately does NOT re-read revoked/unreadable WORK data.
	// It still rechecks protected bytes, SSH/boot and exact resource ownership.
	found := false
	for _, p := range b.run.runPolicy.Deployments {
		if p.NodeID == row.spec.Node {
			found = true
			if err := b.run.readback(ctx, p); err != nil {
				return err
			}
			break
		}
	}
	if !found {
		return errors.New("fault node no longer bound")
	}
	observation, err := f.plan.Observe(ctx)
	if err != nil {
		return err
	}
	if ctx.Err() != nil || observation.Node != f.grant.Node || observation.Resource != f.grant.Resource || observation.BootID != f.boot || len(observation.State) != len(f.grant.Before) {
		return errors.New("fault resource identity UNKNOWN")
	}
	for key, value := range observation.State {
		before, ok := f.grant.Before[key]
		if !ok || (value != before && value != f.grant.During[key]) {
			return errors.New("foreign fault state/ownership")
		}
	}
	if !row.restore && !maps.Equal(observation.State, f.grant.Before) {
		return errors.New("apply no longer has the exact baseline")
	}
	if row.restore && !observationOnly && maps.Equal(observation.State, f.grant.Before) {
		return errors.New("resource already restored; no mutation authorized")
	}
	return nil
}
