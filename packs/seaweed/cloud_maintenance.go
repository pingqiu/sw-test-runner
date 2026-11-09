package seaweed

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/pingqiu/sw-test-runner/inventory"
)

const maintenanceWork = 1200 * time.Second
const maintenanceReserve = 300 * time.Second

func maintenanceAdmission(ctx context.Context, d inventory.Document, c CloudConfig) error {
	allowed := map[string]map[string]bool{
		"volume.balance":         {"C1-single-kill": true, "C2-rolling": true},
		"volume.fix.replication": {"C1-single-kill": true, "C4-meta-restart": true},
		"ec_repair":              {"C1-single-kill": true, "C4-meta-restart": true},
		"ec_vacuum":              {"C1-single-kill": true, "C4-meta-restart": true},
	}
	if len(c.ClusterChaosCases) != 1 || !allowed[c.MaintenanceTask][c.ClusterChaosCases[0]] || d.Target != "onprem" || len(d.Hosts) != 2 || c.Replication != "001" || c.Chaos || c.ChaosSet != "" || c.ECStage1 || c.KVSoak || c.KVLoad || c.MasterWrites || c.ReadCase != "" || c.SWCtl != nil || c.ECUnderQuorum || c.ECReconstruction || c.ECVolumeImpl != "" {
		return errors.New("maintenance requires one approved task/fault pair, two onprem hosts, replication 001 and no other selector")
	}
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) < maintenanceWork+maintenanceReserve-time.Second {
		return errors.New("maintenance requires --timeout at least 25m including the 300s cleanup reserve")
	}
	return errors.New("maintenance runtime is not integrated yet")
}

// Each new step must fit its entire maximum before qualification ends. Cleanup
// uses the separate reserve; caller deadlines may only shorten these bounds.
type maintenanceBudget struct{ work, hard time.Time }

func maintenanceNewBudget(ctx context.Context, now time.Time) maintenanceBudget {
	hard := now.Add(maintenanceWork + maintenanceReserve)
	if d, ok := ctx.Deadline(); ok && d.Before(hard) {
		hard = d
	}
	return maintenanceBudget{hard.Add(-maintenanceReserve), hard}
}

func (b maintenanceBudget) admit(now time.Time, maximum time.Duration) error {
	if maximum <= 0 || now.Add(maximum).After(b.work) {
		return fmt.Errorf("maintenance step maximum %s cannot fit before qualification deadline", maximum)
	}
	return nil
}
