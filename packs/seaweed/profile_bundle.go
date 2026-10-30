package seaweed

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

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
}
type ProfileBundle struct {
	ExecutionAuthorized bool                 `json:"execution_authorized"`
	Plan                Plan                 `json:"plan"`
	Jobs                map[string]tr.JobPin `json:"jobs"`
	ScenarioYAML        string               `json:"scenario_yaml"`
}

// BuildProfile emits only data using the same producers the runtime verifies.
// It performs no observations, SSH, native import, installation or authority read.
func BuildProfile(input ProfileInput) (ProfileBundle, error) {
	out := ProfileBundle{Jobs: map[string]tr.JobPin{}}
	d, err := inventory.Join([]byte(input.Requested), []byte(input.Resolved))
	if err != nil {
		return out, err
	}
	out.Plan = Plan{Version: "seaweed.pack-plan/v1", Profile: "m01-m02-v1", Requested: input.Requested, Resolved: input.Resolved, Smoke: input.Smoke}
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
			args, err := ProfileArgs(d, h.ID, action, role, input.Smoke, files)
			if err != nil {
				return Step{}, err
			}
			out.Jobs[label] = tr.JobPin{Node: h.ID, Executable: "/usr/bin/python3.12", SHA256: interpreter, Argv: args}
			return Step{Host: h.ID, Role: role, Job: label}, nil
		}
		for _, name := range []string{"lab_profile.py", "vf_preflight.py", "kv_smoke.py", "soak_native_worker.py", "soak_oracle.py"} {
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
			if role == "client" {
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
	raw, err := MarshalPlan(out.Plan)
	if err != nil {
		return out, err
	}
	scenario := tr.Scenario{Name: "m01-m02-inventory-kv-smoke", Metadata: map[string]string{"seaweed_plan": string(raw)}}
	for i, name := range phases {
		scenario.Phases = append(scenario.Phases, tr.Phase{Name: name, Always: i == 4, Actions: []tr.Action{{Action: name, Timeout: "90s"}}})
	}
	body, err := yaml.Marshal(scenario)
	if err != nil {
		return out, err
	}
	out.ScenarioYAML = string(body)
	return out, nil
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
