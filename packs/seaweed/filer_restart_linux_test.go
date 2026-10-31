//go:build linux

package seaweed

import (
	"context"
	"encoding/json"
	tr "github.com/pingqiu/sw-test-runner"
	"testing"
)

func filerFixture(t *testing.T, mode string) (*cpuFixture, *restartCPU) {
	f, n := restartFixture(t, mode)
	n.role = "filer"
	f.plan.Restart.Role, f.plan.Restart.Episode = "filer", "filer-once"
	replacement := f.policy.Jobs["deploy-filer"]
	replacement.Argv = append(append([]string{}, replacement.Argv...), "--replacement-filer")
	delete(f.policy.Jobs, "replacement-volume")
	f.policy.Jobs["replacement-filer"] = replacement
	f.policy.GenerationRestarts = map[string]tr.GenerationRestartPlan{"filer-once": {Victim: "deploy-filer", Replacement: "replacement-filer"}}
	f.publish(t, nil)
	return f, n
}
func TestFilerRestartThroughApprovedEngine(t *testing.T) {
	for _, mode := range []string{"positive", "zero-errors", "old-ready", "wrong-product", "missing-ready", "kill-identity", "lost-metadata", "second-window", "before-prekill"} {
		t.Run(mode, func(t *testing.T) {
			f, n := filerFixture(t, mode)
			r, _, err := tr.RunApprovedRequest(context.Background(), f.request, f.profile, registerFastRestart, &f.runtime, nil)
			if err != nil {
				t.Fatal(err)
			}
			want := tr.StatusFail
			if mode == "positive" || mode == "zero-errors" {
				want = tr.StatusPass
			}
			if r == nil || r.Status != want {
				t.Fatalf("mode=%s result=%+v", mode, r)
			}
			if want == tr.StatusPass && (n.seed != 1 || n.httpControls != 2 || n.dcRecovery != 1) {
				t.Fatalf("missing exact metadata/recovery controls: %+v", n.events)
			}
			if len(n.live) != 0 {
				t.Fatal("owned generations remain")
			}
		})
	}
}
func TestFilerRestartBindingRefusesWrongRoleOrVictim(t *testing.T) {
	for _, mode := range []string{"unknown-role", "volume-victim", "volume-argv"} {
		t.Run(mode, func(t *testing.T) {
			f, n := filerFixture(t, "positive")
			switch mode {
			case "unknown-role":
				f.plan.Restart.Role = "master"
			case "volume-victim":
				ep := f.policy.GenerationRestarts["filer-once"]
				ep.Victim = "deploy-volume"
				f.policy.GenerationRestarts["filer-once"] = ep
			case "volume-argv":
				j := f.policy.Jobs["replacement-filer"]
				j.Argv[len(j.Argv)-1] = "--replacement-volume"
				f.policy.Jobs["replacement-filer"] = j
			}
			f.publish(t, nil)
			r, _, err := tr.RunApprovedRequest(context.Background(), f.request, f.profile, registerFastRestart, &f.runtime, nil)
			if err == nil && (r == nil || r.Status != tr.StatusFail) {
				t.Fatal("invalid filer binding passed")
			}
			if n.authorized != 0 {
				t.Fatal("effect after binding refusal")
			}
		})
	}
}
func TestDefaultRestartRolePreservesPlanBytes(t *testing.T) {
	x := ReadRestart{Episode: "volume-once", DC: "read-dc", RecoverDC: "recover-dc", RecoverHTTP: "control-http"}
	b, err := json.Marshal(x)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"episode":"volume-once","dc":"read-dc","recover_dc":"recover-dc","recover_http":"control-http"}` || x.targetRole() != "volume" {
		t.Fatalf("default volume plan changed: %s", b)
	}
}
