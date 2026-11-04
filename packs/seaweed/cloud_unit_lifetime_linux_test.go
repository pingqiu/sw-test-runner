package seaweed

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/pingqiu/sw-test-runner/inventory"
)

// Opt-in, on a host with systemd and sudo (SW_SYSTEMD_UNIT_TEST=1): the exact
// command cloudStart builds starts a unit that nobody stops, and systemd ends it
// at the hard deadline by itself.
func TestUnitLifetimeStopsAnOrphanedUnitAtTheDeadline(t *testing.T) {
	if os.Getenv("SW_SYSTEMD_UNIT_TEST") != "1" {
		t.Skip("set SW_SYSTEMD_UNIT_TEST=1 on a host with systemd and sudo")
	}
	d := inventory.Document{RunID: "lifetime-" + time.Now().UTC().Format("150405"),
		Deadlines: inventory.Deadlines{Hard: time.Now().Add(3 * time.Second).UTC().Format(time.RFC3339Nano)}}
	unit := "sw-cloud-" + d.RunID + "-sleeper.service"
	sh := func(c string) string {
		out, _ := exec.Command("/bin/sh", "-c", c).CombinedOutput()
		return strings.TrimSpace(string(out))
	}
	t.Cleanup(func() { sh("sudo systemctl stop " + unit + "; sudo systemctl reset-failed " + unit) })
	if out, err := exec.Command("/bin/sh", "-c", cloudStart(d, "/tmp", "sleeper", "/bin/sleep 600")).CombinedOutput(); err != nil {
		t.Fatalf("start: %v %s", err, out)
	}
	started := time.Now()
	if s := sh("systemctl is-active " + unit); s != "active" {
		t.Fatalf("unit %s right after start, want active", s)
	}
	for time.Since(started) < 15*time.Second && sh("systemctl is-active "+unit) == "active" {
		time.Sleep(200 * time.Millisecond)
	}
	if r, ran := sh("systemctl show -p Result --value "+unit), time.Since(started); r != "timeout" || ran < 2*time.Second || ran > 8*time.Second {
		t.Errorf("unit ended with Result=%q after %v, want timeout at about 3 s", r, ran.Round(100*time.Millisecond))
	}
}
