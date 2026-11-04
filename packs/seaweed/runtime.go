package seaweed

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"gopkg.in/yaml.v3"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/inventory"
)

// Plan is shared scenario/profile data. It cannot nominate an executor,
// authority or credential and grants no execution permission.
type Plan struct {
	Version   string        `json:"version"`
	Profile   string        `json:"profile,omitempty"`
	Requested string        `json:"requested_inventory"`
	Resolved  string        `json:"resolved_inventory"`
	Preflight []Step        `json:"preflight"`
	Deploy    []Step        `json:"deploy"`
	Ready     []Step        `json:"ready"`
	Workload  []Step        `json:"workload"`
	Collect   []Step        `json:"collect"`
	Smoke     SmokeSettings `json:"smoke"`
	Restart   *ReadRestart  `json:"restart,omitempty"`
	Soak      *SoakPlan     `json:"soak,omitempty"`
}
type ReadRestart struct {
	Freeze      bool   `json:"freeze,omitempty"`
	Episode     string `json:"episode"`
	DC          string `json:"dc"`
	RecoverDC   string `json:"recover_dc"`
	RecoverHTTP string `json:"recover_http"`
	Role        string `json:"role,omitempty"`
	// Writes: report-only SETs across a master restart (role "master" only); DC is the write-load job.
	Writes bool `json:"writes,omitempty"`
}
type Step struct {
	Host string `json:"host"`
	Role string `json:"role"`
	Job  string `json:"job"`
}

var phases = []string{"seaweed_preflight", "seaweed_deploy", "seaweed_workload", "seaweed_collect", "seaweed_cleanup"}

func (p Plan) steps(phase string) []Step {
	switch phase {
	case phases[0]:
		return p.Preflight
	case phases[1]:
		steps := append(append([]Step{}, p.Deploy...), p.Ready...)
		if p.Soak != nil { // the cache layout, once every service is READY
			steps = append(steps, p.Soak.Setup)
		}
		return steps
	case phases[2], "seaweed_read_restart", "seaweed_kv_soak":
		return p.Workload
	case phases[3]:
		return p.Collect
	}
	return nil
}
func decodePlan(raw string) (Plan, *inventory.Document, error) {
	var p Plan
	if len(raw) > inventory.MaxBytes || strings.Contains(raw, "{{") {
		return p, nil, errors.New("plan too large or templated")
	}
	// Token pass rejects duplicate/case-alias keys even inside nested objects.
	scan := json.NewDecoder(strings.NewReader(raw))
	if err := uniqueJSON(scan, 0); err != nil {
		return p, nil, err
	}
	if _, err := scan.Token(); err != io.EOF {
		return p, nil, errors.New("trailing plan")
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return p, nil, err
	}
	if p.Version != "seaweed.pack-plan/v1" {
		return p, nil, errors.New("unsupported plan")
	}
	if p.Profile != "" && p.Profile != "m01-m02-v1" {
		return p, nil, errors.New("unsupported host profile")
	}
	d, err := inventory.Join([]byte(p.Requested), []byte(p.Resolved))
	return p, d, err
}
func uniqueJSON(d *json.Decoder, depth int) error {
	if depth > 32 {
		return errors.New("plan nesting exceeds 32")
	}
	t, err := d.Token()
	if err != nil {
		return err
	}
	switch t {
	case json.Delim('{'):
		seen := map[string]bool{}
		for d.More() {
			k, e := d.Token()
			if e != nil {
				return e
			}
			s, ok := k.(string)
			if !ok || seen[strings.ToLower(s)] {
				return errors.New("duplicate plan key")
			}
			seen[strings.ToLower(s)] = true
			// All pack field tags are lower-case; do not accept Go's case aliases.
			if s != strings.ToLower(s) {
				return errors.New("noncanonical plan key")
			}
			if e = uniqueJSON(d, depth+1); e != nil {
				return e
			}
		}
		_, err = d.Token()
	case json.Delim('['):
		for d.More() {
			if err = uniqueJSON(d, depth+1); err != nil {
				return err
			}
		}
		_, err = d.Token()
	}
	return err
}
func hash(raw []byte) string { return fmt.Sprintf("%x", sha256.Sum256(raw)) }
func bind(p Plan, d *inventory.Document, in tr.PackInputs, now time.Time) error {
	var scenario tr.Scenario
	if err := yaml.Unmarshal(in.Scenario, &scenario); err != nil {
		return err
	}
	if len(scenario.Phases) != len(phases) {
		return errors.New("complete five-phase scenario required")
	}
	if p.Soak != nil && p.Restart != nil {
		return errors.New("soak and restart are exclusive")
	}
	for i, phase := range scenario.Phases {
		if phase.Always != (i == 4) || phase.Parallel || phase.Repeat > 1 || len(phase.Actions) != 1 {
			return errors.New("phase shape changed")
		}
		a := phase.Actions[0]
		want := phases[i]
		if i == 2 && p.Restart != nil {
			want = "seaweed_read_restart"
		}
		if i == 2 && p.Soak != nil {
			want = "seaweed_kv_soak"
		}
		if a.Action != want || a.IgnoreError || a.Retry != 0 || a.Node != "" || a.Target != "" || a.Replica != "" || len(a.Params) != 0 {
			return errors.New("phase skipped/reordered/retried/ignored or redirected")
		}
	}
	b := in.Binding
	// The ten-minute rule stays for every non-soak plan; a soak plan's ceiling
	// is SoakHorizon, checked in bindSoak (contract r3, R3).
	if p.Profile == "m01-m02-v1" && p.Soak == nil && b.HardDeadline.Sub(now) > 10*time.Minute {
		return errors.New("M01/M02 profile requires original hard deadline within ten minutes; future windows cannot launch early")
	}
	rdma := d.Target == "onprem" && (d.Transport == "rc" || d.Transport == "dc") && b.Transport == "rdma"
	http := d.Target == "azure" && d.Transport == "http" && b.Transport == "http"
	if b.Workload != "kv" || (!rdma && !http) {
		return errors.New("require explicit on-prem RC/DC or Azure HTTP KV profile; no transport fallback")
	}
	cb := inventory.ConsumerBinding{RequestedSHA256: hash([]byte(p.Requested)), ResolvedSHA256: hash([]byte(p.Resolved)), RunID: b.RunID, Transport: d.Transport, ArtifactManifestSHA256: b.Artifacts.SHA256, Deadlines: inventory.Deadlines{Work: b.WorkDeadline.UTC().Format(time.RFC3339Nano), Hard: b.HardDeadline.UTC().Format(time.RFC3339Nano), CleanupReserveMS: b.CleanupReserveMS}}
	for _, h := range b.Hosts {
		address, port, err := net.SplitHostPort(h.Host)
		if err != nil {
			return err
		}
		n, err := strconv.Atoi(port)
		if err != nil {
			return err
		}
		cb.Hosts = append(cb.Hosts, inventory.HostBinding{ID: h.Node, Address: address, SSHPort: n, User: h.User, HostKey: h.HostKey, RunRoot: h.Root, BootID: in.Boots[h.Node]})
	}
	if _, err := inventory.Bind([]byte(p.Requested), []byte(p.Resolved), cb); err != nil {
		return err
	}
	hosts := map[string]inventory.Host{}
	roles := map[string]int{}
	for _, h := range d.Hosts {
		hosts[h.ID] = h
		for _, role := range h.Roles {
			roles[role]++
		}
		if h.RunRoot != b.OutputDir {
			return errors.New("S0b requires one exact per-host output root")
		}
		for _, disk := range h.Disks {
			if *disk.FormatAllowed {
				return errors.New("formatting is outside this reproduction slice")
			}
		}
	}
	if roles["client"] == 0 || len(p.Workload) == 0 || roles["master"] == 0 || roles["filer"] != 1 || roles["volume"] != 1 {
		return errors.New("required smoke needs a client, nonempty workload, master, one filer and one volume")
	}
	used := map[string]bool{}
	check := func(steps []Step, kind string) error {
		coverage := map[string]bool{}
		for _, s := range steps {
			h, ok := hosts[s.Host]
			if !ok {
				return errors.New("foreign step host")
			}
			j, ok := in.Jobs[s.Job]
			if !ok || j.Node != s.Host || used[s.Job] {
				return errors.New("unapproved, foreign or reused job label")
			}
			used[s.Job] = true
			key := s.Host
			if kind == "deploy" {
				key += "/" + s.Role
				if s.Role == "client" || !contains(h.Roles, s.Role) {
					return errors.New("foreign deployed role")
				}
			} else if kind == "workload" {
				if s.Role != "client" || !contains(h.Roles, "client") {
					return errors.New("workload requires a declared client")
				}
				var err error
				if p.Soak != nil {
					want, e := SoakLoadArgs(d, p.Smoke, p.Soak)
					err = checkSoakTask(j, in, s.Host, want, e, p.Smoke, []string{"kv_smoke.py", "soak_load.py", "soak_native_worker.py", "soak_oracle.py", "soak_stats.py"}, true)
				} else if p.Restart != nil {
					err = checkReadTask(j, in, d, s.Host, p.Smoke, "seed", "dc")
				} else {
					err = checkSmoke(j, in, d, s.Host, p.Smoke)
				}
				if err != nil {
					return err
				}
			} else if s.Role != "" {
				return errors.New("probe role must be empty")
			}
			if p.Profile == "m01-m02-v1" && kind != "workload" {
				if err := checkProfile(j, in, d, s.Host, kind, s.Role, p.profileSettings(false)); err != nil {
					return err
				}
			}
			if coverage[key] {
				return errors.New("duplicate step coverage")
			}
			coverage[key] = true
		}
		for _, h := range d.Hosts {
			if kind == "deploy" {
				for _, r := range h.Roles {
					if r != "client" && !coverage[h.ID+"/"+r] {
						return errors.New("missing owned service")
					}
				}
			} else if (kind != "workload" || contains(h.Roles, "client")) && !coverage[h.ID] {
				return errors.New("incomplete phase host coverage")
			}
		}
		return nil
	}
	for _, v := range []struct {
		steps []Step
		kind  string
	}{{p.Preflight, "preflight"}, {p.Deploy, "deploy"}, {p.Ready, "ready"}, {p.Workload, "workload"}, {p.Collect, "collect"}} {
		if err := check(v.steps, v.kind); err != nil {
			return err
		}
	}
	if p.Restart != nil {
		return bindRestart(p, d, in, used)
	}
	if p.Soak != nil {
		return bindSoak(p, d, in, used, now)
	}
	return nil
}
func contains(a []string, s string) bool {
	for _, x := range a {
		if x == s {
			return true
		}
	}
	return false
}

func containsStep(a []Step, label string) bool {
	for _, s := range a {
		if s.Job == label {
			return true
		}
	}
	return false
}

// MarshalPlan is the common producer path; no cloud-specific deployment path.
// It checks inventory Join and grants no execution permission.
func MarshalPlan(p Plan) ([]byte, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	_, _, err = decodePlan(string(b))
	return bytes.Clone(b), err
}
