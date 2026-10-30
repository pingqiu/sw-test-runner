package cli

import (
	"encoding/json"
	tr "github.com/pingqiu/sw-test-runner"
	"io"
	"os"
	"testing"
)

func TestApprovedCLIHasNoRuntimeOrLegacyFallbackFlag(t *testing.T) {
	called := false
	register := func(*tr.Registry) { called = true }
	for _, args := range [][]string{
		{"approved-run"},
		{"approved-run", "--request", "/absent", "--policy", "/absent", "--fake"},
		{"approved-run", "--request", "/absent", "--policy", "/absent", "extra-shell"},
		{"approved-run", "--request", "/absent", "--policy", "/absent"},
	} {
		if code := Run(register, args); code == 0 {
			t.Fatalf("unqualified entry passed: %v", args)
		}
	}
	if called {
		t.Fatal("approved entry loaded the legacy pack registry")
	}
}

func TestApprovedJSONRefusalIsMachineReadable(t *testing.T) {
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	code := Run(func(*tr.Registry) {}, []string{"approved-run", "--json", "--request", "/definitely-absent", "--policy", "/definitely-absent"})
	_ = w.Close()
	os.Stdout = old
	raw, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil {
		t.Fatal(err)
	}
	var out struct{ Schema, Code, Remedy string }
	if code == 0 || json.Unmarshal(raw, &out) != nil || out.Schema != "seaweed.approved-result/v1" || out.Code != "S0B_ADMISSION_OR_EVIDENCE_REFUSED" || out.Remedy == "" {
		t.Fatalf("refusal lost: rc=%d body=%s", code, raw)
	}
}
