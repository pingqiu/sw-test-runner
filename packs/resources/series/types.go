// Package series ports the frozen soak resource assessment over retained receipts.
// It performs no collection, process control, or runner registration.
package series

import "github.com/pingqiu/sw-test-runner/packs/resources/gomemory"

type Policy string

const (
	Startup54d     Policy = "54d2d185-startup"
	Replacement9b5 Policy = "9b5d0dca-replacement"
)

type Identity = gomemory.Identity
type Capture struct {
	Host      string   `json:"host"`
	Boot      string   `json:"boot"`
	Namespace string   `json:"namespace"`
	Domain    string   `json:"domain"`
	HZ        int64    `json:"hz"`
	Start     *float64 `json:"start"`
	End       *float64 `json:"end"`
}
type Process struct {
	Identity
	State   string   `json:"state"`
	Status  string   `json:"status"`
	RSS     *float64 `json:"rss_bytes"`
	FDs     *float64 `json:"fds"`
	Pin     *float64 `json:"VmPin_bytes"`
	Lock    *float64 `json:"VmLck_bytes"`
	Capture *Capture `json:"resource_capture"`
}
type Budget struct {
	Bytes *float64 `json:"bytes"`
}
type Server struct {
	Processes []Process           `json:"processes"`
	Data      map[string]*float64 `json:"dat_files"`
	VolumeDir *float64            `json:"volume_dir_bytes"`
	Auxiliary *float64            `json:"auxiliary_bytes"`
	Budgets   []Budget            `json:"pin_budgets"`
	Gauges    map[string]*float64 `json:"required_gauges"`
}
type Sample struct {
	Body        *float64  `json:"body_elapsed"`
	BodyBracket []float64 `json:"capture_body"`
	Server      Server    `json:"server"`
	Clients     []Process `json:"clients"`
}
type Generation struct {
	Identity Identity `json:"identity"`
}
type Restart struct {
	New  *Generation `json:"new"`
	Pair *Restart    `json:"pair"`
}
type Replacement struct {
	Role string   `json:"role"`
	New  Identity `json:"new"`
}
type Fault struct {
	Kind        string        `json:"kind"`
	Body        *float64      `json:"body_elapsed"`
	Started     *float64      `json:"started"`
	Finished    *float64      `json:"finished"`
	Victim      *int          `json:"victim_index"`
	Replacement *Identity     `json:"replacement"`
	Result      *Restart      `json:"result"`
	Clients     []Replacement `json:"replacement_clients"`
}
type Final struct {
	Processes []Process           `json:"processes"`
	Clients   []Process           `json:"final_clients"`
	RDMA      map[string]*float64 `json:"rdma"`
	Baseline  struct {
		RDMA map[string]*float64 `json:"rdma"`
	} `json:"preworkload_baseline"`
}
type Input struct {
	Policy     Policy                       `json:"policy"`
	Samples    []Sample                     `json:"series"`
	Final      Final                        `json:"final"`
	Faults     []Fault                      `json:"faults"`
	GoEvidence map[string]gomemory.Evidence `json:"go_evidence"`
}
type Detail struct {
	Identity    Identity           `json:"identity"`
	Status      string             `json:"status"`
	Reason      string             `json:"reason,omitempty"`
	Samples     int                `json:"samples,omitempty"`
	RSSBaseline float64            `json:"rss_baseline"`
	RSSTail     float64            `json:"rss_tail"`
	RSSGrowth   float64            `json:"rss_growth"`
	Slope       float64            `json:"rss_slope_mib_per_min"`
	FDBaseline  float64            `json:"fd_baseline"`
	FDMax       float64            `json:"fd_max"`
	FDWindow    []float64          `json:"fd_baseline_window,omitempty"`
	FDFrom      float64            `json:"fd_checked_from"`
	PinWindow   []float64          `json:"baseline_window,omitempty"`
	Baseline    map[string]float64 `json:"baseline,omitempty"`
	Final       map[string]float64 `json:"final,omitempty"`
	GoMemory    *gomemory.Result   `json:"go_memory,omitempty"`
}
type Check struct {
	Status  string   `json:"status"`
	Reason  string   `json:"reason,omitempty"`
	Details []Detail `json:"details,omitempty"`
	MaxData *float64 `json:"max_data_bytes,omitempty"`
	MaxAux  *float64 `json:"max_aux_bytes,omitempty"`
}
type Result struct {
	Status string           `json:"status"`
	Checks map[string]Check `json:"checks"`
}
