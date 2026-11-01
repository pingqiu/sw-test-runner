package seaweed

import (
	"encoding/json"
	"errors"
	"path"
	"slices"

	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/inventory"
)

// SoakSetup is lab_profile.py's soak_setup block (exact keys there).
type SoakSetup struct {
	Collection  string `json:"collection"`
	Replication string `json:"replication"`
	TTL         string `json:"ttl"`
	GrowCount   int    `json:"grow_count"`
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
	raw, err := json.Marshal(map[string]any{"root": runRoot(d, "m01"), "smoke": c, "master_http": "http://" + endpoint(*master, "master_http"),
		"native": map[string]any{"filer_grpc": c.FilerGRPC, "filer_http": c.FilerHTTP, "native_options": map[string]any{
			"capacity_bytes": 1, "root": "/" + l.Collection, "collection": l.Collection, "replication": l.Replication}},
		"layout": l, "duration_s": x.DurationS, "pacing_s": x.PacingS, "quiescence_s": int(QuiescenceReserve.Seconds()),
		"client_kill_at_s": x.ClientKillAtS, "key_prefix": soakLoadKeyPrefix})
	if err != nil {
		return nil, err
	}
	return []string{"-I", path.Join(s.HelperDir, "soak_load.py"), "--config", string(raw)}, nil
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
	s.soakSetup = &SoakSetup{Collection: l.Collection, Replication: l.Replication, TTL: l.TTL, GrowCount: p.Soak.GrowCount}
	return s
}
