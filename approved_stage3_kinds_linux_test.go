//go:build linux

package testrunner

import (
	"context"
	"os"
	"testing"
)

// Stage 3's stateful fault kinds are admitted as witnessed cleanup grants; any other kind still refuses.
func TestStage3GrantKindsAdmittedOthersRefused(t *testing.T) {
	for _, kind := range []string{"link_flap", "mr_pressure", "fd_exhaust"} {
		approvedFaultFixture(t, func(_ *approvedFixture, g *CleanupGrant) { g.Kind = kind }) // fails the test if refused
	}
	for _, kind := range []string{"conn_kill", "qp_kill", ""} {
		f := newApprovedFixture(t)
		apply, restore := f.job, f.job
		apply.Argv, restore.Argv = []string{"apply"}, []string{"restore"}
		f.policy.Jobs["apply"], f.policy.Jobs["restore"] = apply, restore
		f.publish(t, []CleanupGrant{{Kind: kind, Node: "n1", Resource: "run1-private-port", Before: map[string]string{"link": "UP"},
			During: map[string]string{"link": "DOWN"}, Apply: apply, Restore: restore, Deadline: f.binding.HardDeadline}})
		policy, err := ReadTrustedPolicy(f.profile, uint32(os.Getuid()))
		if err != nil {
			t.Fatal(err)
		}
		if run, err := OpenApprovedRun(context.Background(), f.request, policy, &f.runtime); err == nil {
			_ = run.Close()
			t.Fatalf("grant kind %q admitted", kind)
		}
	}
}
