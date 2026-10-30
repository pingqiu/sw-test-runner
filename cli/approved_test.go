package cli

import (
	tr "github.com/pingqiu/sw-test-runner"
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
