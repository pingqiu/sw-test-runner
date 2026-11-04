package seaweed

import (
	"strings"
	"testing"
	"time"

	"github.com/pingqiu/sw-test-runner/inventory"
)

// Every transient unit carries RuntimeMaxSec up to the run's hard deadline, so
// the host's systemd stops it even if the controller is gone.
func TestUnitLifetimeRunsToTheHardDeadline(t *testing.T) {
	hard := time.Date(2026, 11, 4, 6, 0, 0, 0, time.UTC)
	defer func(now func() time.Time) { cloudNow = now }(cloudNow)
	d := inventory.Document{RunID: "r1", Deadlines: inventory.Deadlines{Hard: hard.Format(time.RFC3339Nano)}}
	for _, c := range []struct {
		left time.Duration
		want string
	}{
		{90 * time.Second, "RuntimeMaxSec=90"},
		{90*time.Second - time.Millisecond, "RuntimeMaxSec=90"}, // rounded up, never short
		{0, "RuntimeMaxSec=1"},                                  // past the deadline: stop at once
		{-time.Hour, "RuntimeMaxSec=1"},
	} {
		cloudNow = func() time.Time { return hard.Add(-c.left) }
		if got := cloudStart(d, "/r", "volume", "X"); got != `sudo systemd-run --unit='sw-cloud-r1-volume' --property=`+c.want+` --property=WorkingDirectory='/r' X` {
			t.Errorf("%v before the hard deadline: %s", c.left, got)
		}
	}
	if got := cloudStart(inventory.Document{RunID: "r1"}, "/r", "volume", "X"); strings.Contains(got, "RuntimeMaxSec") {
		t.Errorf("a document without a hard deadline got a lifetime: %s", got)
	}
}
