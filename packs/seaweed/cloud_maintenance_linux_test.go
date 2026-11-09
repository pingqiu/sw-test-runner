//go:build linux

package seaweed

import (
	"context"
	"encoding/json"
	"github.com/pingqiu/sw-test-runner/inventory"
	"strings"
	"testing"
	"time"
)

func TestMaintenanceAdmissionBeforeArtifactOrHostWork(t *testing.T) {
	for _, body := range []string{
		`{"maintenance_task":"foreign","cluster_chaos_cases":["C1-single-kill"],"replication":"001"}`,
		`{"maintenance_task":"volume.balance","cluster_chaos_cases":["C5-two-kill"],"replication":"001"}`,
		`{"maintenance_task":"ec_vacuum","cluster_chaos_cases":["C2-rolling"],"replication":"001"}`,
	} {
		var c CloudConfig
		if err := json.Unmarshal([]byte(body), &c); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Second)
		var d inventory.Document
		json.Unmarshal(directInput(t, "127.0.0.1:1"), &d)
		h := d.Hosts[0]
		h.ID = "second"
		h.Management.Address = "127.0.0.2"
		h.Data.Address = "10.84.1.11"
		d.Hosts = append(d.Hosts, h)
		_, err := RunCloudHTTP(ctx, processJSON(d), c)
		cancel()
		if err == nil || !strings.Contains(err.Error(), "maintenance") {
			t.Fatalf("maintenance admission did not refuse before artifact/host work: %v", err)
		}
	}
}

func TestMaintenanceReserveAndPhaseMaxima(t *testing.T) {
	now := time.Unix(1000, 0)
	ctx, cancel := context.WithDeadline(context.Background(), now.Add(1500*time.Second))
	defer cancel()
	b := maintenanceNewBudget(ctx, now)
	for _, x := range []struct {
		offset, seconds int
		allowed         bool
	}{{0, 240, true}, {240, 600, true}, {840, 240, true}, {1080, 120, true}, {1081, 120, false}, {1200, 1, false}, {0, 0, false}} {
		if got := b.admit(now.Add(time.Duration(x.offset)*time.Second), time.Duration(x.seconds)*time.Second); (got == nil) != x.allowed {
			t.Fatalf("%+v: %v", x, got)
		}
	}
	if !b.hard.Equal(now.Add(1500*time.Second)) || !b.work.Equal(now.Add(1200*time.Second)) {
		t.Fatal("reserve changed")
	}
}

func TestMaintenanceStepAdmitsKillGraceBeforeDispatch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	r := &chaosRun{maintenance: &maintenanceRuntime{}}
	// There is deliberately no node: a budget refusal must precede dispatch.
	if _, err := r.step(ctx, "absent", "bounded-step", 2, ":"); err == nil || !strings.Contains(err.Error(), "cannot fit") {
		t.Fatalf("timeout -k 5 grace was not admitted: %v", err)
	}
}
