//go:build linux

package testrunner

import (
	"context"
	"github.com/pingqiu/sw-test-runner/infra"
	"os"
	"testing"
	"time"
)

func TestApprovedRunRejectsUnboundReadback(t *testing.T) {
	for _, bad := range []string{"policy", "clock", "empty-native", "foreign-bytes", "boot"} {
		t.Run(bad, func(t *testing.T) {
			f := newApprovedFixture(t)
			read := f.runtime.Readback
			f.runtime.Readback = func(ctx context.Context, p infra.DeploymentPolicy) (infra.DeploymentReceipt, error) {
				r, err := read(ctx, p)
				switch bad {
				case "policy":
					r.PolicySHA256 = "foreign"
				case "clock":
					r.ReadStart = time.Now().Add(time.Minute)
				case "empty-native":
					delete(r.Files, "/opt/approved/native")
				case "foreign-bytes":
					r.Files["/opt/approved/product"] = "foreign"
				case "boot":
					r.BootID = "replacement"
				}
				return r, err
			}
			policy, err := ReadTrustedPolicy(f.profile, uint32(os.Getuid()))
			if err != nil {
				t.Fatal(err)
			}
			run, err := OpenApprovedRun(context.Background(), f.request, policy, &f.runtime)
			if err == nil {
				_ = run.Close()
				t.Fatalf("unbound %s readback admitted", bad)
			}
			if f.node.authorized != 0 {
				t.Fatal("bad readback executed a job")
			}
		})
	}
}

func TestApprovedCustodianRechecksBootAndBytes(t *testing.T) {
	for _, when := range []string{"before-enroll", "before-authorize"} {
		for _, changed := range []string{"boot", "bytes"} {
			t.Run(when+"/"+changed, func(t *testing.T) {
				f := newApprovedFixture(t)
				drift := false
				read := f.runtime.Readback
				f.runtime.Readback = func(ctx context.Context, p infra.DeploymentPolicy) (infra.DeploymentReceipt, error) {
					r, e := read(ctx, p)
					if drift {
						if changed == "boot" {
							r.BootID = "foreign"
						} else {
							r.Files[f.job.Executable] = "foreign"
						}
					}
					return r, e
				}
				if when == "before-authorize" {
					f.runtime.Jobs = changedAfterEnroll{f.node, func() { drift = true }}
				}
				p, err := ReadTrustedPolicy(f.profile, uint32(os.Getuid()))
				if err != nil {
					t.Fatal(err)
				}
				run, err := OpenApprovedRun(context.Background(), f.request, p, &f.runtime)
				if err != nil {
					t.Fatal(err)
				}
				if when == "before-enroll" {
					drift = true
				}
				_, err = run.Scope().Start(JobSpec{Node: f.job.Node, Executable: f.job.Executable, SHA256: f.job.SHA256, Argv: f.job.Argv})
				if err == nil || f.node.authorized != 0 {
					t.Fatalf("changed deployment executed: %v %d", err, f.node.authorized)
				}
				_ = run.Close()
				if len(f.node.ids) != 0 {
					t.Fatal("inert child abandoned")
				}
			})
		}
	}
}
