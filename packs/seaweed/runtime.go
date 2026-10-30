package seaweed

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"gopkg.in/yaml.v3"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/inventory"
)

// Plan is data embedded in scenario.metadata.seaweed_plan, therefore pinned by
// S0b's independent GO. It cannot nominate an executor, authority or credential.
// Protected installation is a separate prerequisite; deploy starts owned jobs.
type Plan struct {
	Version   string        `json:"version"`
	Requested string        `json:"requested_inventory"`
	Resolved  string        `json:"resolved_inventory"`
	Preflight []Step        `json:"preflight"`
	Deploy    []Step        `json:"deploy"`
	Ready     []Step        `json:"ready"`
	Workload  []Step        `json:"workload"`
	Collect   []Step        `json:"collect"`
	Smoke     SmokeSettings `json:"smoke"`
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
		return append(append([]Step{}, p.Deploy...), p.Ready...)
	case phases[2]:
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
func bind(p Plan, d *inventory.Document, in tr.PackInputs) error {
	var scenario tr.Scenario
	if err := yaml.Unmarshal(in.Scenario, &scenario); err != nil {
		return err
	}
	if len(scenario.Phases) != len(phases) {
		return errors.New("complete five-phase scenario required")
	}
	for i, p := range scenario.Phases {
		if p.Always != (i == 4) || p.Parallel || p.Repeat > 1 || len(p.Actions) != 1 {
			return errors.New("phase shape changed")
		}
		a := p.Actions[0]
		if a.Action != phases[i] || a.IgnoreError || a.Retry != 0 || a.Node != "" || a.Target != "" || a.Replica != "" || len(a.Params) != 0 {
			return errors.New("phase skipped/reordered/retried/ignored or redirected")
		}
	}
	b := in.Binding
	if d.Target != "onprem" || (d.Transport != "rc" && d.Transport != "dc") || b.Workload != "kv" || b.Transport != "rdma" {
		return errors.New("this slice requires on-prem KV with RC/DC and no HTTP fallback")
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
	for _, h := range d.Hosts {
		hosts[h.ID] = h
		if h.RunRoot != b.OutputDir {
			return errors.New("S0b requires one exact per-host output root")
		}
		for _, disk := range h.Disks {
			if *disk.FormatAllowed {
				return errors.New("formatting is outside this reproduction slice")
			}
		}
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
				if err := checkSmoke(j, in, d, s.Host, p.Smoke); err != nil {
					return err
				}
			} else if s.Role != "" {
				return errors.New("probe role must be empty")
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

type session struct {
	mu         sync.Mutex
	planHash   string
	next       int
	terminal   error
	cleanupErr error
}
type runtimePack struct {
	mu       sync.Mutex
	sessions map[*tr.RunScope]*session
}

func (r *runtimePack) run(ctx context.Context, a *tr.ActionContext, act tr.Action) (out map[string]string, returned error) {
	defer func() {
		if returned != nil {
			a.FailApprovedPack(returned)
		}
	}()
	in, err := a.ApprovedPackInputs()
	if err != nil {
		return unavailable(ctx, a, act)
	}
	if len(act.Params) != 0 {
		return refusal(act.Action, "PLAN_INVALID", errors.New("runtime phases take no caller parameters"))
	}
	var sealed tr.Scenario
	if err := yaml.Unmarshal(in.Scenario, &sealed); err != nil {
		return refusal(act.Action, "PLAN_INVALID", err)
	}
	raw := sealed.Metadata["seaweed_plan"]
	p, d, err := decodePlan(raw)
	if err == nil {
		err = bind(p, d, in)
	}
	if err != nil {
		return refusal(act.Action, "BINDING_REFUSED", err)
	}
	r.mu.Lock()
	s := r.sessions[a.Safety]
	if s == nil {
		s = &session{planHash: hash([]byte(raw))}
		r.sessions[a.Safety] = s
	}
	r.mu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if act.Action == phases[4] {
		err = a.Safety.Close()
		s.cleanupErr = errors.Join(s.cleanupErr, err)
		if s.cleanupErr != nil {
			return refusal(act.Action, "CLEANUP_UNPROVEN", s.cleanupErr)
		}
		s.next = 5
		return admittedResult("settled", "OK", "owned issuers settled; no cloud or disk deletion performed", act.Action, d), nil
	}
	if s.terminal != nil || s.planHash != hash([]byte(raw)) || s.next >= 4 || phases[s.next] != act.Action {
		return refusal(act.Action, "PHASE_ORDER_REFUSED", errors.New("phase repeated, reordered or changed"))
	}
	for _, step := range p.steps(act.Action) {
		background := act.Action == phases[1] && containsStep(p.Deploy, step.Job)
		if _, _, err = a.RunApprovedPackJob(ctx, step.Job, background); err != nil {
			s.terminal = err
			return refusal(act.Action, "OWNED_JOB_FAILED", err)
		}
	}
	s.next++
	return admittedResult("completed", "OK", "inspect S0b authority/job receipts and owned stdout artifacts", act.Action, d), nil
}
func containsStep(a []Step, label string) bool {
	for _, s := range a {
		if s.Job == label {
			return true
		}
	}
	return false
}
func refusal(action, code string, err error) (map[string]string, error) {
	const hint = "restore exact approved inventory/plan/deployment/lease; preserve UNKNOWN and cleanup evidence"
	return result("refused", code, hint, action, nil), fmt.Errorf("%s: %w; remedy: %s", code, err, hint)
}

// MarshalPlan is the common producer path; no cloud-specific deployment path.
// It checks inventory Join. S0b binding and all job pins are checked at execution.
func MarshalPlan(p Plan) ([]byte, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	_, _, err = decodePlan(string(b))
	return bytes.Clone(b), err
}

func admittedResult(status, code, remedy, action string, d *inventory.Document) map[string]string {
	r := Result{Schema: "seaweed.pack-result/v1", Status: status, Code: code, Remedy: remedy, Action: action, ExecutionAuthorized: true, Effects: []string{"owned_jobs_only"}, State: d.State, Transport: d.Transport}
	if status == "settled" {
		r.Effects = []string{"settle_owned_issuers"}
	}
	b, _ := json.Marshal(r)
	return map[string]string{"value": string(b), "execution_authorized": "true"}
}
