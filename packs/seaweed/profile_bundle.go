package seaweed

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/inventory"
	"gopkg.in/yaml.v3"
)

// ProfileInput is declared installation data, not a deployment receipt or GO.
type ProfileInput struct {
	Requested string                       `json:"requested_inventory"`
	Resolved  string                       `json:"resolved_inventory"`
	Smoke     SmokeSettings                `json:"smoke"`
	Files     map[string]map[string]string `json:"files"`
	// Soak carries soak SETTINGS only (schedule, rotation, layouts); the
	// builder names and pins every soak job, as bind will verify them.
	Soak *SoakPlan `json:"soak,omitempty"`
}
type ProfileBundle struct {
	ExecutionAuthorized bool                 `json:"execution_authorized"`
	Plan                Plan                 `json:"plan"`
	Jobs                map[string]tr.JobPin `json:"jobs"`
	ScenarioYAML        string               `json:"scenario_yaml"`
	// Restarts are the soak faults' generation-restart plans for the policy.
	Restarts map[string]tr.GenerationRestartPlan `json:"generation_restarts,omitempty"`
}

// BuildProfile emits only data using the same producers the runtime verifies.
// It performs no observations, SSH, native import, installation or authority read.
func BuildProfile(input ProfileInput) (ProfileBundle, error) {
	out := ProfileBundle{Jobs: map[string]tr.JobPin{}, Restarts: map[string]tr.GenerationRestartPlan{}}
	d, err := inventory.Join([]byte(input.Requested), []byte(input.Resolved))
	if err != nil {
		return out, err
	}
	out.Plan = Plan{Version: "seaweed.pack-plan/v1", Profile: "m01-m02-v1", Requested: input.Requested, Resolved: input.Resolved, Smoke: input.Smoke}
	if input.Soak != nil {
		x := *input.Soak
		if x.Setup != (Step{}) || x.Evidence != (Step{}) || x.Assess != (Step{}) || len(x.FaultReady) != 0 {
			return out, errors.New("soak input carries settings only; the builder names the soak jobs")
		}
		out.Plan.Soak = &x
	}
	if len(input.Files) != 2 {
		return out, errors.New("profile requires exact two-host declared file set")
	}
	for _, h := range d.Hosts {
		files := input.Files[h.ID]
		interpreter := files["/usr/bin/python3.12"]
		if len(interpreter) != 64 {
			return out, errors.New("profile interpreter pin missing")
		}
		add := func(action, role string) (Step, error) {
			label := fmt.Sprintf("%s-%s", action, h.ID)
			if role != "" {
				label += "-" + role
			}
			args, err := ProfileArgs(d, h.ID, action, role, out.Plan.profileSettings(false), files)
			if err != nil {
				return Step{}, err
			}
			out.Jobs[label] = tr.JobPin{Node: h.ID, Executable: "/usr/bin/python3.12", SHA256: interpreter, Argv: args}
			return Step{Host: h.ID, Role: role, Job: label}, nil
		}
		helpers := []string{"lab_profile.py", "vf_preflight.py", "kv_smoke.py", "soak_native_worker.py", "soak_oracle.py"}
		if input.Soak != nil && h.ID == "m01" {
			helpers = append(helpers, "soak_load.py", "soak_stats.py", "soak_assess.py")
		}
		for _, name := range helpers {
			body, err := workerFiles.ReadFile("worker/" + name)
			if err != nil {
				return out, err
			}
			if files[path.Join(input.Smoke.HelperDir, name)] != hash(body) {
				return out, errors.New("declared helper bytes differ from compiled profile: " + name)
			}
		}
		for _, phase := range []struct {
			name string
			dest *[]Step
		}{{"preflight", &out.Plan.Preflight}, {"ready", &out.Plan.Ready}, {"collect", &out.Plan.Collect}} {
			step, err := add(phase.name, "")
			if err != nil {
				return out, err
			}
			*phase.dest = append(*phase.dest, step)
		}
		for _, role := range h.Roles {
			if role == "client" && input.Soak != nil {
				continue // the soak load is the workload (addSoak)
			} else if role == "client" {
				args, err := SmokeArgs(d, h.ID, input.Smoke)
				if err != nil {
					return out, err
				}
				label := "smoke-" + h.ID
				out.Jobs[label] = tr.JobPin{Node: h.ID, Executable: "/usr/bin/python3.12", SHA256: interpreter, Argv: args}
				out.Plan.Workload = append(out.Plan.Workload, Step{Host: h.ID, Role: role, Job: label})
			} else {
				step, err := add("deploy", role)
				if err != nil {
					return out, err
				}
				out.Plan.Deploy = append(out.Plan.Deploy, step)
			}
		}
	}
	name, timeouts := "m01-m02-inventory-kv-smoke", map[string]string{}
	if out.Plan.Soak != nil {
		if err = addSoak(&out, d, input.Files); err != nil {
			return out, err
		}
		x := out.Plan.Soak
		name = "m01-m02-kv-rotation-soak"
		timeouts[phases[2]] = fmt.Sprintf("%ds", x.DurationS+int((QuiescenceReserve+WitnessWait+2*time.Minute).Seconds()))
		timeouts[phases[3]] = fmt.Sprintf("%ds", int(CollectReserve.Seconds()))
	}
	raw, err := MarshalPlan(out.Plan)
	if err != nil {
		return out, err
	}
	scenario := tr.Scenario{Name: name, Metadata: map[string]string{"seaweed_plan": string(raw)}}
	for i, phase := range phases {
		action, timeout := phase, "90s"
		if t, ok := timeouts[phase]; ok {
			timeout = t
		}
		if i == 2 && out.Plan.Soak != nil {
			action = "seaweed_kv_soak"
		}
		scenario.Phases = append(scenario.Phases, tr.Phase{Name: phase, Always: i == 4, Actions: []tr.Action{{Action: action, Timeout: timeout}}})
	}
	body, err := yaml.Marshal(scenario)
	if err != nil {
		return out, err
	}
	out.ScenarioYAML = string(body)
	return out, nil
}

// addSoak pins the soak jobs with the producers bind verifies: the M01 load
// (the workload) and assessor, the M02 setup and evidence, a fault-ready probe
// per host, and per fault the m02 victim's replacement and restart plan.
func addSoak(out *ProfileBundle, d *inventory.Document, files map[string]map[string]string) error {
	p := &out.Plan
	x := p.Soak
	pin := func(label, host string, args []string, err error) (Step, error) {
		if err != nil {
			return Step{}, err
		}
		out.Jobs[label] = tr.JobPin{Node: host, Executable: "/usr/bin/python3.12", SHA256: files[host]["/usr/bin/python3.12"], Argv: args}
		return Step{Host: host, Job: label}, nil
	}
	var err error
	args, e := SoakLoadArgs(d, p.Smoke, x)
	load, err := pin("soak-load-m01", "m01", args, e)
	if err != nil {
		return err
	}
	load.Role = "client"
	p.Workload = []Step{load}
	args, e = SoakAssessArgs(d, p.Smoke, x)
	if x.Assess, err = pin("soak-assess-m01", "m01", args, e); err != nil {
		return err
	}
	args, e = ProfileArgs(d, "m02", "soak_setup", "", p.setupSettings(), files["m02"])
	if x.Setup, err = pin("soak-setup-m02", "m02", args, e); err != nil {
		return err
	}
	args, e = ProfileArgs(d, "m02", "soak_collect", "", p.setupSettings(), files["m02"])
	if x.Evidence, err = pin("soak-evidence-m02", "m02", args, e); err != nil {
		return err
	}
	for _, h := range d.Hosts {
		args, e = ProfileArgs(d, h.ID, "ready", "", p.profileSettings(true), files[h.ID])
		step, err := pin("fault-ready-"+h.ID, h.ID, args, e)
		if err != nil {
			return err
		}
		x.FaultReady = append(x.FaultReady, step)
	}
	for _, f := range x.Faults {
		victim := ""
		for _, s := range p.Deploy {
			if s.Host == "m02" && s.Role == f.Role {
				victim = s.Job
			}
		}
		if victim == "" {
			return fmt.Errorf("soak fault %s has no m02 %s deploy step", f.Episode, f.Role)
		}
		args, e = ProfileArgs(d, "m02", "deploy", f.Role, p.profileSettings(false), files["m02"])
		replacement := "replacement-m02-" + f.Role
		if _, err = pin(replacement, "m02", append(args, "--replacement-"+f.Role), e); err != nil {
			return err
		}
		out.Restarts[f.Episode] = tr.GenerationRestartPlan{Victim: victim, Replacement: replacement}
	}
	return nil
}

// DecodeProfileInput preserves strict nested JSON: duplicate/aliased fields
// cannot silently select a different inventory or product file declaration.
func DecodeProfileInput(raw []byte) (ProfileInput, error) {
	var in ProfileInput
	if len(raw) > inventory.MaxBytes {
		return in, errors.New("profile input too large")
	}
	scan := json.NewDecoder(strings.NewReader(string(raw)))
	if err := uniqueJSON(scan, 0); err != nil {
		return in, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return in, err
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return in, errors.New("trailing profile input")
	}
	return in, nil
}
