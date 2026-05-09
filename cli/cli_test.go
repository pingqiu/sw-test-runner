package cli

import (
	"testing"

	tr "github.com/pingqiu/sw-test-runner"
)

func TestTerminalRunStateMapsCancelledResult(t *testing.T) {
	state, summary := terminalRunState(&tr.ScenarioResult{
		Status:    tr.StatusFail,
		Cancelled: true,
		Error:     "cancelled before phase k8s_fio",
	})
	if state != tr.RunStateCancelled {
		t.Fatalf("state = %s, want cancelled", state)
	}
	if summary != "cancelled before phase k8s_fio" {
		t.Fatalf("summary = %q", summary)
	}
}

func TestTerminalRunStateMapsOrdinaryFailure(t *testing.T) {
	state, summary := terminalRunState(&tr.ScenarioResult{
		Status: tr.StatusFail,
		Error:  "phase failed",
	})
	if state != tr.RunStateFail {
		t.Fatalf("state = %s, want fail", state)
	}
	if summary != "phase failed" {
		t.Fatalf("summary = %q", summary)
	}
}

func TestApplyBundleValidationProfileProtocolReleaseGate(t *testing.T) {
	var opts tr.BundleValidationOptions
	if err := applyBundleValidationProfile("protocol-release-gate", &opts); err != nil {
		t.Fatal(err)
	}
	if !opts.RequirePass || !opts.RequireTiming || !opts.RequireChildBundles {
		t.Fatalf("profile did not enable strict gates: %+v", opts)
	}
	if opts.ExpectScenario != "protocol-release-gate-suite" {
		t.Fatalf("scenario = %q", opts.ExpectScenario)
	}
	if got, want := len(opts.ExpectedChildren), 4; got != want {
		t.Fatalf("children = %d, want %d", got, want)
	}
}

func TestApplyBundleValidationProfilePreservesExplicitFields(t *testing.T) {
	opts := tr.BundleValidationOptions{
		ExpectScenario:   "custom",
		ExpectedChildren: []string{"one"},
	}
	if err := applyBundleValidationProfile("protocol-release-gate", &opts); err != nil {
		t.Fatal(err)
	}
	if opts.ExpectScenario != "custom" {
		t.Fatalf("scenario overwritten: %q", opts.ExpectScenario)
	}
	if len(opts.ExpectedChildren) != 1 || opts.ExpectedChildren[0] != "one" {
		t.Fatalf("children overwritten: %+v", opts.ExpectedChildren)
	}
}
