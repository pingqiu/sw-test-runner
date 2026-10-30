//go:build linux

package testrunner

import (
	"context"
	"os"
	"testing"
)

func TestApprovedPolicyCannotLaunchOutsideReadbackSet(t *testing.T) {
	for _, bad := range []string{"custodian-path", "job-path", "job-hash", "nonexecuting-role"} {
		t.Run(bad, func(t *testing.T) {
			f := newApprovedFixture(t)
			switch bad {
			case "custodian-path":
				f.policy.Runner = "/opt/approved/foreign-runner"
			case "job-path":
				j := f.job
				j.Executable = "/tmp/unverified"
				f.policy.Jobs["probe"] = j
			case "job-hash":
				j := f.job
				j.SHA256 = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
				f.policy.Jobs["probe"] = j
			case "nonexecuting-role":
				j := f.job
				j.Executable = "/opt/approved/native"
				j.SHA256 = f.policy.Deployments[0].Files[4].SHA256
				f.policy.Jobs["probe"] = j
			}
			f.publish(t, nil)
			policy, err := ReadTrustedPolicy(f.profile, uint32(os.Getuid()))
			if err != nil {
				t.Fatal(err)
			}
			run, err := OpenApprovedRun(context.Background(), f.request, policy, &f.runtime)
			if err == nil {
				_ = run.Close()
				t.Fatalf("approved bytes with inconsistent %s admitted", bad)
			}
			if f.node.authorized != 0 {
				t.Fatal("unverified entrypoint executed")
			}
		})
	}
}
