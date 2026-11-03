//go:build linux

package seaweed

import (
	"context"
	"encoding/json"
	tr "github.com/pingqiu/sw-test-runner"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

type ecPlantTamper struct {
	*ecCPU
	mode string
}

func (n *ecPlantTamper) Wait(ctx context.Context, id string) (tr.ExitInfo, error) {
	e, err := n.ecCPU.Wait(ctx, id)
	if err != nil {
		return e, err
	}
	s := n.specs[id]
	if !s.CaptureOutput || len(s.Argv) < 4 || s.Argv[2] != "--config" {
		return e, nil
	}
	var c struct{ Label string }
	_ = json.Unmarshal([]byte(s.Argv[3]), &c)
	if c.Label != "planted" {
		return e, nil
	}
	raw, _, _ := e.Output.Decode()
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	r := m["outcomes"].([]any)[0].(map[string]any)
	original := r["original"].(map[string]any)
	switch n.mode {
	case "run":
		m["run_id"] = "foreign"
	case "route":
		r["route"], original["route"] = "volume", "volume"
	case "key":
		r["key"], original["key"] = "ec-foreign", "ec-foreign"
	case "original-digest":
		original["sha256"] = "foreign"
	case "mutated-digest":
		r["sha256"] = "foreign"
	case "time":
		original["ended"] = 3
	case "not-original-exact":
		original["exact"] = false
	case "nested":
		original["original"] = map[string]any{}
	}
	raw, _ = json.Marshal(m)
	e.Output = captured(raw).Output
	return e, nil
}
func TestECAmendPlantEvidenceJoin(t *testing.T) {
	for _, mode := range []string{"run", "route", "key", "original-digest", "mutated-digest", "time", "not-original-exact", "nested"} {
		t.Run(mode, func(t *testing.T) {
			f, n := ecFixture(t, "positive")
			f.runtime.Jobs = &ecPlantTamper{n, mode}
			r, _, err := tr.RunApprovedRequest(context.Background(), f.request, f.profile, RegisterPack, &f.runtime, nil)
			if err != nil {
				t.Fatal(err)
			}
			if r == nil || r.Status != tr.StatusFail {
				t.Fatalf("unjoined planted evidence passed: %+v", r)
			}
		})
	}
}

func TestECDeclaredArgvCompatibility(t *testing.T) {
	f, _ := ecFixture(t, "positive")
	jobs := map[string]map[string]any{}
	for label, j := range f.policy.Jobs {
		jobs[label] = map[string]any{"argv": j.Argv}
	}
	raw, err := json.Marshal(jobs)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "jobs.json")
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	result, err := exec.Command("python3", "worker/ec_argv_fixture_test.py", path).CombinedOutput()
	if err != nil {
		t.Fatalf("declared argv executable join: %v\n%s", err, result)
	}
	t.Log(string(result))
}
