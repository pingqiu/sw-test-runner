//go:build linux

package seaweed

import (
	"context"
	"encoding/json"
	"os"

	tr "github.com/pingqiu/sw-test-runner"
	"strings"
	"testing"
)

func TestECAmendCaseAliasesCannotSplitPinAndWorker(t *testing.T) {
	body, err := workerFiles.ReadFile("worker/ec_service.py")
	if err != nil {
		t.Fatal(err)
	}
	goodHash := strings.Repeat("d", 64)
	config := `{"root":"/outside/not-approved","Root":"/run/approved","role":"h0","exe":"/outside/not-installed","Exe":"/opt/product","sha256":"` + strings.Repeat("e", 64) + `","SHA256":"` + goodHash + `"}`
	j := tr.JobPin{Node: "m02", Executable: "/usr/bin/python3.12", SHA256: strings.Repeat("b", 64), Argv: []string{"-I", "/opt/worker/ec_service.py", config}}
	in := tr.PackInputs{Binding: tr.RunBinding{OutputDir: "/run/approved"}, Jobs: map[string]tr.JobPin{"h0": j}, Files: map[string]map[string]string{"m02": {"/opt/worker/ec_service.py": hash(body), j.Executable: j.SHA256, "/opt/product": goodHash}}, Scripts: map[string][]byte{"service": body}}
	if err := ecPin(in, "h0", "ec_service.py"); err == nil {
		t.Fatal("case-alias config accepted: Go pins uppercase approved root/product, Python uses lowercase foreign values")
	}
}

func TestECAmendUnicodeDecisionAliasRefuses(t *testing.T) {
	body, _ := workerFiles.ReadFile("worker/ec_service.py")
	goodHash := strings.Repeat("d", 64)
	config := `{"root":"/run/approved","role":"h0","exe":"/opt/product","sha256":"` + strings.Repeat("e", 64) + `","\u017fha256":"` + goodHash + `"}`
	j := tr.JobPin{Node: "m02", Executable: "/usr/bin/python3.12", SHA256: strings.Repeat("b", 64), Argv: []string{"-I", "/opt/worker/ec_service.py", config}}
	in := tr.PackInputs{Binding: tr.RunBinding{OutputDir: "/run/approved"}, Jobs: map[string]tr.JobPin{"h0": j}, Files: map[string]map[string]string{"m02": {"/opt/worker/ec_service.py": hash(body), j.Executable: j.SHA256, "/opt/product": goodHash}}, Scripts: map[string][]byte{"service": body}}
	if err := ecPin(in, "h0", "ec_service.py"); err == nil {
		t.Fatal("Unicode long-s alias split Go/Python digest pin")
	}
}

type reviewerECReplay struct {
	*ecCPU
	control json.RawMessage
}

func (n *reviewerECReplay) Wait(ctx context.Context, id string) (tr.ExitInfo, error) {
	e, err := n.ecCPU.Wait(ctx, id)
	if err != nil {
		return e, err
	}
	s := n.specs[id]
	if !s.CaptureOutput {
		return e, nil
	}
	arg := s.Argv[2]
	if arg == "--config" {
		arg = s.Argv[3]
	}
	var c struct{ Label string }
	_ = json.Unmarshal([]byte(arg), &c)
	if c.Label == "planted" {
		e.Output = captured(n.control).Output
	}
	return e, nil
}

func TestECAmendRealCorruptResponseCannotEarnControlPass(t *testing.T) {
	raw, err := os.ReadFile("testdata/ec-preplant-controls.json")
	if err != nil {
		t.Fatal(err)
	}
	var receipts struct {
		Cases []struct {
			Mode   string
			Plant  bool
			Exit   int
			Output json.RawMessage
		}
	}
	if err = json.Unmarshal(raw, &receipts); err != nil {
		t.Fatal(err)
	}
	bad := receipts.Cases[1]
	if bad.Mode != "corrupt" || !bad.Plant || bad.Exit != 0 {
		t.Fatal("counterexample not reproduced")
	}
	f, n := ecFixture(t, "positive")
	f.runtime.Jobs = &reviewerECReplay{ecCPU: n, control: bad.Output}
	result, bundle, err := tr.RunApprovedRequest(context.Background(), f.request, f.profile, RegisterPack, &f.runtime, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || result.Status != tr.StatusFail {
		t.Fatalf("real corrupted pre-plant response accepted as control; result=%+v bundle=%s", result, bundle)
	}
}

type reviewerECSmall struct{ *ecCPU }

func (n *reviewerECSmall) Wait(ctx context.Context, id string) (tr.ExitInfo, error) {
	e, err := n.ecCPU.Wait(ctx, id)
	if err != nil {
		return e, err
	}
	s := n.specs[id]
	if !s.CaptureOutput {
		return e, nil
	}
	var c struct{ Label string }
	_ = json.Unmarshal([]byte(s.Argv[2]), &c)
	if c.Label == "capture" {
		raw, _, _ := e.Output.Decode()
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		for i, v := range m["shards"].([]any) {
			if i >= 4 {
				v.(map[string]any)["job"] = "h4"
			}
		}
		raw, _ = json.Marshal(m)
		e.Output = captured(raw).Output
	}
	return e, nil
}
func TestECAmendFiveHolderTopologyRefuses(t *testing.T) {
	f, n := ecFixture(t, "positive")
	prior := f.editScenario
	f.editScenario = func(s *tr.Scenario) {
		prior(s)
		var p EC1aPlan
		_ = json.Unmarshal([]byte(s.Metadata["seaweed_ec_plan"]), &p)
		p.Services = p.Services[:8]
		raw, _ := json.Marshal(p)
		s.Metadata["seaweed_ec_plan"] = string(raw)
	}
	f.publish(t, map[string]string{"m02": "/service/private-key"})
	f.runtime.Jobs = &reviewerECSmall{n}
	r, _, err := tr.RunApprovedRequest(context.Background(), f.request, f.profile, RegisterPack, &f.runtime, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != tr.StatusFail {
		t.Fatal("five holders with 1/1/1/1/10 shards earned PASS")
	}
}
