package smoke

import (
	"context"
	"testing"
)

func TestFreshRunRequiresIndependentApprovalBeforeAnyCreate(t *testing.T) {
	f := &fake{}
	r := (Controller{Command: f.command}).Execute(context.Background(), "run", fixture(t))
	if r.Code != "FRESH_APPROVAL_REFUSED" || len(f.events) != 0 {
		t.Fatalf("unapproved fresh path issued commands: code=%s events=%v", r.Code, f.events)
	}
}
