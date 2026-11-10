package seaweed

import (
	"encoding/json"
	"errors"
	"path"
	"slices"
	"time"

	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/inventory"
)

// SoakSetup is lab_profile.py's soak_setup block (exact keys there).
type SoakSetup struct {
	Collection  string `json:"collection"`
	Replication string `json:"replication"`
	TTL         string `json:"ttl"`
	GrowCount   int    `json:"grow_count"`
	// MasterGenerations is what the admitted fault schedule produces; collect refuses any other history.
	MasterGenerations int `json:"master_generations"`
}

// soakLoadKeyPrefix is r9's LMCache-shaped key form; each run has a fresh cluster.
const soakLoadKeyPrefix = "native-mp@00000000@0@"

func runRoot(d *inventory.Document, host string) string {
	for _, h := range d.Hosts {
		if h.ID == host {
			return h.RunRoot
		}
	}
	return ""
}

// SoakLoadArgs is the exact soak_load.py argv for the M01 load task.
func SoakLoadArgs(d *inventory.Document, s SmokeSettings, x *SoakPlan) ([]string, error) {
	c, err := smokeConfig(d, "m01", s)
	if err != nil {
		return nil, err
	}
	var master *inventory.Host
	for i := range d.Hosts {
		if contains(d.Hosts[i].Roles, "master") {
			master = &d.Hosts[i]
		}
	}
	if master == nil || runRoot(d, "m01") == "" {
		return nil, errors.New("soak load needs a master and the M01 run root")
	}
	l := x.Layout
	config := map[string]any{"root": runRoot(d, "m01"), "smoke": c, "master_http": "http://" + endpoint(*master, "master_http"),
		"native": map[string]any{"filer_grpc": c.FilerGRPC, "filer_http": c.FilerHTTP, "native_options": map[string]any{
			"capacity_bytes": 1, "root": "/" + l.Collection, "collection": l.Collection, "replication": l.Replication}},
		"layout": l, "duration_s": x.DurationS, "pacing_s": x.PacingS, "quiescence_s": int(QuiescenceReserve.Seconds()),
		"client_kill_at_s": x.ClientKillAtS, "key_prefix": soakLoadKeyPrefix, "write_stop_s": 30}
	if x.Pattern != nil {
		config["pattern"] = x.Pattern // absent for the soak, so its argv is unchanged
	}
	if x.rc() {
		config["read_transport"] = "rc" // absent is DC: a DC soak's argv is unchanged
	}
	if x.rowMode() { // stage 3; absent otherwise, so every other argv is unchanged
		f, r := x.Faults[0], x.Row
		config["fault_row"] = map[string]any{"kind": f.kind(), "episode": f.Episode, "at_s": f.AtS, "refused": append([]string{}, r.Refused...), "audit_s": r.AuditS}
	}
	raw, err := json.Marshal(config)
	if err != nil {
		return nil, err
	}
	return []string{"-I", path.Join(s.HelperDir, "soak_load.py"), "--config", string(raw)}, nil
}

// soakFaultActions are the pinned soak_fault.py jobs of a stage-3 row, all on M02.
var soakFaultActions = []string{"apply", "restore", "sample", "mark-load", "mark-quiescence"}

// SoakFaultArgs is the exact soak_fault.py argv for one action of the row: the volume's units (the deploy
// generation and, for F1, its replacement), every other M02 service unit to watch, and the row's settings.
func SoakFaultArgs(d *inventory.Document, s SmokeSettings, x *SoakPlan, action string) ([]string, error) {
	var m02 *inventory.Host
	for i := range d.Hosts {
		if d.Hosts[i].ID == "m02" {
			m02 = &d.Hosts[i]
		}
	}
	if m02 == nil || m02.RunRoot == "" || !x.rowMode() || x.Row == nil || !slices.Contains(soakFaultActions, action) {
		return nil, errors.New("soak fault job needs a stage-3 row and the M02 run root")
	}
	volumes := 0
	for _, h := range d.Hosts {
		if contains(h.Roles, "volume") {
			volumes++
		}
	}
	if volumes != 1 || !contains(m02.Roles, "volume") { // F1 attributes every PUT to the one volume it kills
		return nil, errors.New("a stage-3 row needs exactly one volume, on M02")
	}
	unit := func(label string) string { return "sw-cloud-" + d.RunID + "-" + label }
	f, r := x.Faults[0], x.Row
	units, watch := []string{unit("deploy-m02-volume")}, map[string]string{}
	if f.restarts() {
		units = append(units, unit("replacement-m02-volume"))
	}
	for _, role := range m02.Roles {
		if role != "volume" && role != "client" {
			watch[role] = unit("deploy-m02-" + role)
		}
	}
	config := map[string]any{"root": m02.RunRoot, "run_id": d.RunID, "episode": f.Episode, "kind": f.kind(), "units": units, "watch": watch,
		"python": "/usr/bin/python3.12", "script": path.Join(s.HelperDir, "soak_fault.py"),
		"deadman_s": r.HoldS + 2*int(InjectBound/time.Second), "management_addrs": []string{m02.Management.Address},
		"netdev": r.Netdev, "rdma_device": r.RDMADevice, "rdma_port": r.RDMAPort, "setup_ports": r.SetupPorts,
		"memlock_bytes": r.MemlockBytes, "nofile_extra": r.NofileExtra, "mount": r.Mount, "free_below_bytes": r.FreeBelowBytes,
		"max_ballast_bytes": r.MaxBallastBytes, "metrics_url": r.MetricsURL, "metrics": r.Metrics,
		"volume_status_url": "http://" + endpoint(*m02, "volume_http") + "/status", // the product's own open-volume list
		"sample_s":          2, "quiescence_s": r.QuiescenceS,
		"max_s": int((DeployReserve + secs(x.DurationS) + QuiescenceReserve + secs(r.AuditS) + secs(r.QuiescenceS) + LoadExitGrace) / time.Second)}
	raw, err := json.Marshal(config)
	return []string{"-I", path.Join(s.HelperDir, "soak_fault.py"), "--config", string(raw), "--action", action}, err
}

// checkSoakTask admits one soak task: exact argv from the joined inventory,
// the pinned interpreter, and the exact helper bytes read back on that host.
func checkSoakTask(j tr.JobPin, in tr.PackInputs, host string, want []string, err error, s SmokeSettings, helpers []string, native bool) error {
	if err != nil {
		return err
	}
	if j.Node != host || !slices.Equal(j.Argv, want) || j.Executable != "/usr/bin/python3.12" || in.Files[host][j.Executable] != j.SHA256 {
		return errors.New("soak task argv/interpreter not derived from the joined inventory")
	}
	return checkPinned(in, host, s, helpers, native)
}

// setupSettings carries the soak_setup block for the M02 setup job only.
func (p Plan) setupSettings() SmokeSettings {
	s := p.profileSettings(false)
	l := p.Soak.Layout
	generations := 1
	for _, f := range p.Soak.Faults {
		if f.Role == "master" {
			generations = 2
		}
	}
	s.soakSetup = &SoakSetup{Collection: l.Collection, Replication: l.Replication, TTL: l.TTL, GrowCount: p.Soak.GrowCount, MasterGenerations: generations}
	return s
}
