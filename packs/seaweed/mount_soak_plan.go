package seaweed

import (
	"encoding/json"
	"errors"
	"net"
	"path"
	"strconv"

	"github.com/pingqiu/sw-test-runner/inventory"
)

// MountSoakCase is the read_case name of the Stage 1 mount soak. It reuses the
// direct two-host path: preflight, deploy and READY, one bounded worker step on
// M01, the assessor, then collect and cleanup.
const MountSoakCase = "mount-soak"

// mountSoakHelpers must be byte-identical to the embedded sources on M01.
var mountSoakHelpers = []string{"mount_soak.py", "mount_assess.py"}

// The Stage 1 r4 schedule: load phases, partition episodes, the report-only
// same-file append at the end of the last phase. The worker step is bounded
// by their sum plus mount, heal-recovery and verification time.
var (
	mountSoakPhasesS   = []int{900, 600, 300}
	mountSoakEpisodesS = []int{60, 180}
)

const (
	mountSoakMounts       = 4
	mountSoakDualAppendS  = 300
	mountSoakWorkerBudget = 3600 // seconds for the whole worker step
	// mountSoakWriteBudget bounds W1 well inside the profile's 24 x 32 MiB of
	// volumes: space is only reclaimed by vacuum, and ENOSPC would prove nothing.
	mountSoakWriteBudget = 150 << 20
	mountSoakAssessBudget = 600
)

// mountSoakConfig is the worker's config, derived only from the joined inventory.
type mountSoakConfig struct {
	Root       string   `json:"root"`
	Weed       string   `json:"weed"`
	Filer      string   `json:"filer"`
	FilerPath  string   `json:"filer_path"`
	VolumeHost string   `json:"volume_host"`
	VolumePort int      `json:"volume_port"`
	PhasesS    []int    `json:"phases_s"`
	EpisodesS  []int    `json:"episodes_s"`
	Iptables   []string `json:"iptables"`
	Mounts     int      `json:"mounts"`
	DualS      int      `json:"dual_append_s"`
	Budget     int64    `json:"write_budget_bytes"`
}

// MountSoakArgs produces the exact worker argv for M01. The partition targets
// exactly M02's volume HTTP port, the only path a direct upload can take: the
// inventory must have one volume server, so reassignment has nowhere to go.
func MountSoakArgs(d *inventory.Document, s SmokeSettings) ([]string, error) {
	var filer, volume *inventory.Host
	for i := range d.Hosts {
		h := &d.Hosts[i]
		if contains(h.Roles, "filer") {
			filer = h
		}
		if contains(h.Roles, "volume") {
			if volume != nil {
				return nil, errors.New("mount soak needs exactly one volume server")
			}
			volume = h
		}
	}
	if filer == nil || volume == nil || volume.ID == "m01" || filer.ID == "m01" {
		return nil, errors.New("mount soak needs the filer and the volume server on M02")
	}
	filerHTTP, volumeHTTP := endpoint(*filer, "filer_http"), endpoint(*volume, "volume_http")
	host, portText, err := net.SplitHostPort(volumeHTTP)
	if err != nil || filerHTTP == "" {
		return nil, errors.New("mount soak needs filer_http and volume_http services")
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(mountSoakConfig{Root: runRoot(d, "m01"), Weed: path.Join(path.Dir(s.HelperDir), "bin", "weed"), Filer: filerHTTP,
		FilerPath: "/testops/" + d.RunID + "/mountsoak", VolumeHost: host, VolumePort: port, PhasesS: mountSoakPhasesS, EpisodesS: mountSoakEpisodesS,
		Iptables: []string{"sudo", "-n"}, Mounts: mountSoakMounts, DualS: mountSoakDualAppendS, Budget: mountSoakWriteBudget})
	if err != nil {
		return nil, err
	}
	return []string{"-I", path.Join(s.HelperDir, "mount_soak.py"), "--config", string(raw)}, nil
}

// MountAssessArgs is the exact assessor argv.
func MountAssessArgs(d *inventory.Document, s SmokeSettings) []string {
	return []string{"-I", path.Join(s.HelperDir, "mount_assess.py"), "--root", runRoot(d, "m01")}
}

// mountVerdict is the assessor's one-line result.
type mountVerdict struct {
	Schema  string `json:"schema"`
	Status  string `json:"status"`
	Cleanup string `json:"cleanup"`
}

// decodeMountVerdict accepts only the assessor's own schema and a known status.
func decodeMountVerdict(raw []byte) (mountVerdict, error) {
	var v mountVerdict
	if err := json.Unmarshal(raw, &v); err != nil {
		return v, err
	}
	switch {
	case v.Schema != "seaweed-mount-verdict/1":
		return v, errors.New("mount soak: foreign verdict schema")
	case v.Status != "PASS" && v.Status != "FAIL" && v.Status != "INVALID" && v.Status != "ENVIRONMENT" && v.Status != "CLEANUP_UNKNOWN":
		return v, errors.New("mount soak: unknown verdict status")
	}
	return v, nil
}
