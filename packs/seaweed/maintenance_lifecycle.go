package seaweed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pingqiu/sw-test-runner/inventory"
)

type maintenanceRootRecord struct {
	Schema    string `json:"schema"`
	RunID     string `json:"run_id"`
	RoleRunID string `json:"role_run_id"`
	Path      string `json:"path"`
	Dev       uint64 `json:"dev"`
	Inode     uint64 `json:"ino"`
	UID       uint32 `json:"uid"`
}
type maintenanceUnit struct {
	Unit string `json:"unit"`
	Root string `json:"root"`
}
type maintenanceRootPath struct {
	Path string `json:"path"`
}
type maintenanceHostGroup struct {
	Host  inventory.Host
	Units []maintenanceUnit
	Roots []maintenanceRootPath
	Ports []int
}

func maintenanceLifecycleCommand(config map[string]any) (string, error) {
	code, err := workerFiles.ReadFile("worker/maintenance_lifecycle.py")
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(config)
	if err != nil {
		return "", err
	}
	return "python3 -I -c " + cloudQuote(string(code)) + " " + cloudQuote(string(raw)), nil
}

func (m *maintenanceRuntime) recordRoot(h inventory.Host, raw string) error {
	var r maintenanceRootRecord
	if err := maintenanceDecode([]byte(raw), &r); err != nil {
		return err
	}
	if r.Schema != "seaweed.maintenance-root/v1" || r.RunID != m.d.RunID || r.RoleRunID != m.r.docs[h.ID].RunID || r.Path != h.RunRoot || r.Inode == 0 || r.Dev == 0 {
		return errors.New("maintenance prepare receipt is not bound to this role")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.receipts == nil {
		m.receipts = map[string]maintenanceRootRecord{}
	}
	if _, exists := m.receipts[h.ID]; exists {
		return errors.New("duplicate maintenance prepare")
	}
	m.receipts[h.ID] = r
	return m.r.save("maintenance-root-receipts.json", m.receipts)
}

func (m *maintenanceRuntime) groups() []maintenanceHostGroup {
	byHost := map[string]*maintenanceHostGroup{}
	ids := []string{}
	for id := range m.hosts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		h := m.hosts[id]
		key := h.Management.Address + ":" + strconv.Itoa(h.Management.Port)
		g := byHost[key]
		if g == nil {
			g = &maintenanceHostGroup{Host: h}
			byHost[key] = g
		}
		g.Roots = append(g.Roots, maintenanceRootPath{h.RunRoot})
		for _, s := range h.Services {
			g.Ports = append(g.Ports, s.Port)
		}
		for _, role := range h.Roles {
			if role == "client" {
				continue
			}
			for generation := 1; generation <= 4; generation++ {
				name := role
				if generation > 1 {
					name += "-g" + strconv.Itoa(generation)
				}
				g.Units = append(g.Units, maintenanceUnit{"sw-cloud-" + m.r.docs[id].RunID + "-" + name + ".service", h.RunRoot})
			}
		}
		if id == "meta" {
			for _, role := range []string{"maintenance-task", "maintenance-admin", "maintenance-worker"} {
				g.Units = append(g.Units, maintenanceUnit{"sw-cloud-" + m.r.docs[id].RunID + "-" + role + ".service", h.RunRoot})
			}
		}
	}
	keys := []string{}
	for key := range byHost {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	groups := []maintenanceHostGroup{}
	for _, key := range keys {
		groups = append(groups, *byHost[key])
	}
	return groups
}

func (m *maintenanceRuntime) lifecycleConfig(g maintenanceHostGroup, stage string) map[string]any {
	config := map[string]any{"stage": stage, "roots": g.Roots, "units": g.Units, "ports": g.Ports, "collection": m.base["collection"]}
	m.mu.Lock()
	defer m.mu.Unlock()
	receipts := []maintenanceRootRecord{}
	for _, root := range g.Roots {
		for _, r := range m.receipts {
			if root.Path == r.Path {
				receipts = append(receipts, r)
			}
		}
	}
	config["receipts"] = receipts
	return config
}

func (m *maintenanceRuntime) arm(ctx context.Context) error {
	for _, g := range m.groups() {
		fresh, err := maintenanceLifecycleCommand(m.lifecycleConfig(g, "fresh"))
		if err != nil {
			return err
		}
		if _, err = m.r.step(ctx, g.Host.ID, "maintenance-fresh-host", 25, "sudo -n "+fresh); err != nil {
			return err
		}
	}
	for _, g := range m.groups() {
		seconds := int(time.Until(m.budget.hard).Seconds()) - 45
		if seconds < 60 {
			return errors.New("maintenance backstop cannot fit hard deadline")
		}
		units := []string{}
		for _, u := range g.Units {
			units = append(units, cloudQuote(u.Unit))
		}
		// Emergency stop is one bounded systemd request for exact, pre-gated
		// names. It never reads/deletes protocol data and never uses a glob.
		stop := "systemctl stop " + strings.Join(units, " ")
		unit := "sw-maint-" + m.d.RunID + "-backstop"
		q := cloudQuote
		check := "for u in " + q(unit+".timer") + " " + q(unit+".service") + `; do test "$(sudo -n systemctl show -p LoadState --value "$u")" = not-found; done; `
		cmd := check + "sudo -n systemd-run --unit=" + q(unit) + " --on-active=" + strconv.Itoa(seconds) + " --timer-property=AccuracySec=1s --property=RemainAfterExit=yes --property=TimeoutStartSec=40 --property=TimeoutStopSec=5 /bin/sh -ec " + q("echo MAINTENANCE_BACKSTOP_FIRED; exec "+stop) + "; sudo -n systemctl is-active --quiet " + q(unit+".timer")
		if _, err := m.r.step(ctx, g.Host.ID, "maintenance-arm-backstop", 15, cmd); err != nil {
			return err
		}
		m.armed = append(m.armed, g.Host)
	}
	return m.r.save("maintenance-backstops.json", map[string]any{"armed_hosts": m.armed, "hard": m.budget.hard, "cleanup_reserve_seconds": 300})
}

func (m *maintenanceRuntime) cleanup() error {
	// New connections survive a cancelled qualification context. Exactly two
	// physical-host operations run concurrently; never 10 serial per-role budgets.
	ctx, cancel := context.WithDeadline(context.Background(), m.budget.hard)
	defer cancel()
	if !m.effects {
		// No prepare was dispatched. Prove zero effect without deleting anything,
		// then disarm only timers whose successful arm receipt we actually hold.
		for _, g := range m.groups() {
			n := m.c.node(g.Host)
			if err := n.ConnectContext(ctx); err != nil {
				return err
			}
			cmd, err := maintenanceLifecycleCommand(m.lifecycleConfig(g, "fresh"))
			if err != nil {
				n.Close()
				return err
			}
			dir := filepath.Join(m.c.Output, "maintenance-cleanup", g.Host.ID)
			if err := os.MkdirAll(dir, 0700); err != nil {
				n.Close()
				return err
			}
			_, err = cloudStep(ctx, n, dir, "zero-effect.json", "maintenance-zero-effect", 25, "sudo -n "+cmd)
			n.Close()
			if err != nil {
				return err
			}
		}
		return m.disarm(ctx)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var failure error
	for _, g := range m.groups() {
		wg.Add(1)
		go func(g maintenanceHostGroup) {
			defer wg.Done()
			err := m.cleanupHost(ctx, g)
			mu.Lock()
			failure = errors.Join(failure, err)
			mu.Unlock()
		}(g)
	}
	wg.Wait()
	if failure != nil {
		return failure
	} // Keep both backstops when ANY proof is lost.
	return m.disarm(ctx)
}

func (m *maintenanceRuntime) disarm(ctx context.Context) error {
	for _, h := range m.armed {
		n := m.c.node(h)
		if err := n.ConnectContext(ctx); err != nil {
			return err
		}
		unit := "sw-maint-" + m.d.RunID + "-backstop"
		cmd := `test "$(sudo -n systemctl show -p ExecMainStartTimestampMonotonic --value ` + cloudQuote(unit+".service") + `)" = 0; sudo -n systemctl stop ` + cloudQuote(unit+".timer") + `; if [ "$(sudo -n systemctl show -p LoadState --value ` + cloudQuote(unit+".service") + `)" != not-found ]; then sudo -n systemctl stop ` + cloudQuote(unit+".service") + `; fi; for u in ` + cloudQuote(unit+".service") + " " + cloudQuote(unit+".timer") + `; do s=$(sudo -n systemctl show -p ActiveState --value "$u") || exit 1; test "$s" = inactive; done; echo DISARMED`
		dir := filepath.Join(m.c.Output, "maintenance-cleanup", h.ID)
		if err := os.MkdirAll(dir, 0700); err != nil {
			n.Close()
			return err
		}
		_, err := cloudStep(ctx, n, dir, "disarm.json", "maintenance-disarm", 20, cmd)
		n.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func (m *maintenanceRuntime) cleanupHost(ctx context.Context, g maintenanceHostGroup) error {
	dir := filepath.Join(m.c.Output, "maintenance-cleanup", g.Host.ID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	n := m.c.node(g.Host)
	if err := n.ConnectContext(ctx); err != nil {
		return err
	}
	defer n.Close()
	unit := "sw-maint-" + m.d.RunID + "-backstop"
	check := `test "$(sudo -n systemctl show -p ExecMainStartTimestampMonotonic --value ` + cloudQuote(unit+".service") + `)" = 0; sudo -n systemctl is-active --quiet ` + cloudQuote(unit+".timer")
	// An arm failure can precede setup: no timer means no clean qualification,
	// but teardown still attempts stop and preserves all evidence.
	_, backstopErr := cloudStep(ctx, n, dir, "backstop-check.json", "maintenance-backstop-check", 10, check)
	preserve, err := maintenanceLifecycleCommand(m.lifecycleConfig(g, "preserve"))
	if err != nil {
		return err
	}
	raw, preserveErr := cloudStep(ctx, n, dir, "preserve.json", "maintenance-preserve", 35, "sudo -n "+preserve)
	preserveErr = errors.Join(preserveErr, os.WriteFile(filepath.Join(dir, "preserved.json"), []byte(raw), 0600))
	config := m.lifecycleConfig(g, "cleanup")
	config["remove"] = preserveErr == nil && backstopErr == nil && !m.retain
	command, err := maintenanceLifecycleCommand(config)
	if err != nil {
		return err
	}
	out, cleanupErr := cloudStep(ctx, n, dir, "cleanup.json", "maintenance-cleanup", 180, "sudo -n "+command)
	var result struct {
		Status    string   `json:"status"`
		Removed   []string `json:"removed"`
		Units     int      `json:"units"`
		Listeners int      `json:"listeners"`
		Processes int      `json:"run_processes"`
	}
	if cleanupErr == nil {
		cleanupErr = maintenanceDecode([]byte(out), &result)
		if cleanupErr == nil && (result.Status != "CLEAN" || result.Units != 0 || result.Listeners != 0 || result.Processes != 0) {
			cleanupErr = fmt.Errorf("maintenance cleanup incomplete: %s", strings.TrimSpace(out))
		}
		if cleanupErr == nil {
			want := []string{}
			for _, r := range config["receipts"].([]maintenanceRootRecord) {
				want = append(want, r.Path)
			}
			sort.Strings(want)
			sort.Strings(result.Removed)
			if !reflect.DeepEqual(want, result.Removed) {
				cleanupErr = errors.New("maintenance cleanup removed-path receipt differs")
			}
		}
	}
	// Independent second command, not a re-read of the leaf's own status string.
	checks := []string{}
	for _, root := range g.Roots {
		checks = append(checks, "test ! -e "+cloudQuote(root.Path)+" && test ! -L "+cloudQuote(root.Path))
	}
	if cleanupErr == nil {
		_, cleanupErr = cloudStep(ctx, n, dir, "independent-roots.json", "maintenance-independent-roots", 10, strings.Join(checks, "; "))
	}
	return errors.Join(backstopErr, preserveErr, cleanupErr)
}
