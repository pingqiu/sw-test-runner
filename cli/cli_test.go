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
