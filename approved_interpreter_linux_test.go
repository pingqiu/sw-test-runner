//go:build linux

package testrunner

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/pingqiu/sw-test-runner/infra"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestApprovedInterpreterRequiresQualifiedBody(t *testing.T) {
	for _, mode := range []string{"pinned-body", "unlisted-body", "no-body-pin", "inline-eval", "nested-ssh"} {
		t.Run(mode, func(t *testing.T) {
			f := newApprovedFixture(t)
			body := []byte("print('qualified')\n")
			if mode == "nested-ssh" {
				body = []byte("import os; os.system('ssh other true')\n")
			}
			source := filepath.Join(f.dir, "adapter.py")
			if err := os.WriteFile(source, body, 0600); err != nil {
				t.Fatal(err)
			}
			hash := fmt.Sprintf("%x", sha256.Sum256(body))
			f.policy.Scripts = []FilePin{{Path: source, SHA256: hash}}
			f.policy.Deployments[0].Files[1].SHA256 = hash
			f.policy.Deployments[0].Files[1].Size = int64(len(body))
			f.policy.Deployments[0].RequireInterpreter = true
			f.policy.Deployments[0].Files = append(f.policy.Deployments[0].Files, infra.DeploymentFile{Path: "/usr/bin/python3", SHA256: strings.Repeat("f", 64), Size: 1, Roles: []string{"interpreter"}})
			j := JobPin{Node: "n1", Executable: "/usr/bin/python3", SHA256: strings.Repeat("f", 64), Argv: []string{"-I", "/opt/approved/adapter"}}
			switch mode {
			case "unlisted-body":
				j.Argv[1] = "/run/unlisted.py"
			case "no-body-pin":
				f.policy.Scripts = nil
			case "inline-eval":
				j.Argv = []string{"-c", "print('unqualified')"}
			}
			f.policy.Jobs["probe"] = j
			f.publish(t, nil)
			original := f.runtime.Readback
			f.runtime.Readback = func(ctx context.Context, p infra.DeploymentPolicy) (infra.DeploymentReceipt, error) {
				r, e := original(ctx, p)
				r.Files = map[string]string{}
				for _, file := range p.Files {
					r.Files[file.Path] = file.SHA256
				}
				encoded, _ := json.Marshal(p)
				r.PolicySHA256 = fmt.Sprintf("%x", sha256.Sum256(encoded))
				return r, e
			}
			p, err := ReadTrustedPolicy(f.profile, uint32(os.Getuid()))
			if err != nil {
				t.Fatal(err)
			}
			run, err := OpenApprovedRun(context.Background(), f.request, p, &f.runtime)
			if mode == "pinned-body" {
				if err != nil {
					t.Fatal(err)
				}
				defer run.Close()
				h, err := run.Scope().Start(JobSpec{Node: j.Node, Executable: j.Executable, SHA256: j.SHA256, Argv: j.Argv})
				if err != nil {
					t.Fatal(err)
				}
				if _, err = run.Scope().Wait(context.Background(), h); err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				run.Close()
				t.Fatal("unqualified interpreter code admitted")
			}
		})
	}
}

func TestApprovedEngineCannotInheritYamlOrRegistryConsent(t *testing.T) {
	f := newApprovedFixture(t)
	p, err := ReadTrustedPolicy(f.profile, uint32(os.Getuid()))
	if err != nil {
		t.Fatal(err)
	}
	run, err := OpenApprovedRun(context.Background(), f.request, p, &f.runtime)
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close()
	run.scenario.Env = map[string]string{"allow_mutating": "true"}
	r := NewRegistry()
	r.AllowMutating = true
	executed := false
	r.RegisterMutatingFunc("probe", TierChaos, func(context.Context, *ActionContext, Action) (map[string]string, error) {
		executed = true
		return nil, nil
	})
	result := run.Execute(context.Background(), NewEngine(r, nil), &ActionContext{})
	if result.Status != StatusFail || executed || !r.AllowMutating {
		t.Fatalf("consent escaped independent GO or shared registry changed: %#v executed=%v", result, executed)
	}
}
