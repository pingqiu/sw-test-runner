//go:build linux

package testrunner

import (
	"context"
	"fmt"
	"testing"
)

func TestReviewerPauseCloseOwnsBarrierForGenericOlderRestore(t *testing.T) {
	for _, paused := range []bool{false, true} {
		t.Run(fmt.Sprint(paused), func(t *testing.T) {
			f, n := pauseFixture(t)
			_, _ = runGenerationCase(t, f, func(ctx context.Context, a *ActionContext) error {
				victim, _, err := a.RunApprovedPackJob(ctx, "volume0", true)
				if err != nil {
					return err
				}
				err = a.Safety.ApplyFault("reviewer-generic-older", func(context.Context) error { return nil }, func(context.Context) error {
					// Ordinary ApplyFault relies on Close's issuer barrier, as on the parent.
					n.mu.Lock()
					_, live := n.ids[victim.id]
					state := n.state
					n.mu.Unlock()
					t.Logf("paused=%v generic_restore victim_still_owned=%v pause_state=%s", paused, live, state)
					if live {
						t.Error("Close ran older generic restoration before the resumed victim settled")
					}
					return nil
				}, func(context.Context) error { return nil })
				if err != nil {
					return err
				}
				if paused {
					_, err = a.PauseApprovedPackGeneration(ctx, "freeze", victim)
				}
				return err
			})
		})
	}
}

type pauseUnknownSettlement struct {
	*pauseNode
	victim   string
	failures int
}

func (n *pauseUnknownSettlement) Settle(ctx context.Context, id string) error {
	n.mu.Lock()
	fail := id == n.victim && n.resumeCalls > 0
	if fail {
		n.failures++
	}
	n.mu.Unlock()
	if fail {
		return fmt.Errorf("victim settlement UNKNOWN")
	}
	return n.pauseNode.Settle(ctx, id)
}
func TestPausePostBarrierUnknownWithholdsGenericRestore(t *testing.T) {
	f, n := pauseFixture(t)
	backend := &pauseUnknownSettlement{pauseNode: n}
	f.runtime.Jobs = backend
	restored := false
	run, result := runGenerationCase(t, f, func(ctx context.Context, a *ActionContext) error {
		victim, _, err := a.RunApprovedPackJob(ctx, "volume0", true)
		if err != nil {
			return err
		}
		backend.victim = victim.id
		err = a.Safety.ApplyFault("older-generic", func(context.Context) error { return nil }, func(context.Context) error { restored = true; return nil }, func(context.Context) error { return nil })
		if err != nil {
			return err
		}
		_, err = a.PauseApprovedPackGeneration(ctx, "freeze", victim)
		return err
	})
	if restored || backend.failures == 0 || result.Status != StatusFail || run.resourcesSettled() {
		t.Fatalf("UNKNOWN victim admitted older restore: restored=%v failures=%d status=%s settled=%v", restored, backend.failures, result.Status, run.resourcesSettled())
	}
	if n.resumeCalls != 1 {
		t.Fatalf("CONT calls=%d", n.resumeCalls)
	}
}
