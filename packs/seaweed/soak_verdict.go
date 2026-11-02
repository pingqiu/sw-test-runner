package seaweed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/inventory"
)

// SoakVerdictSchema names the assessor's config and row contract.
const SoakVerdictSchema = "seaweed-soak-verdict/1"

// soakCriteria is the exact mandatory set (soak_assess.MANDATORY): a PASS
// carries every one of them and nothing else.
var soakCriteria = []string{"wrong_reads", "failed_sets", "get_outcomes", "terminal", "fault_errors", "misses", "coverage",
	"body", "client_kill", "rotation", "max_vid_advanced", "routes"}

// SoakBinding is what the assessor was told to judge, echoed in its row.
type SoakBinding struct {
	Schema string `json:"schema"`
	RunID  string `json:"run_id"`
	BootID string `json:"boot_id"`
	Root   string `json:"root"`
}

// observedBoot is the admitted boot of a host from the resolved observation.
func observedBoot(d *inventory.Document, host string) string {
	if d.Observation != nil {
		for _, h := range d.Observation.Hosts {
			if h.HostID == host {
				return h.BootID
			}
		}
	}
	return ""
}

func soakBinding(d *inventory.Document) (SoakBinding, error) {
	b := SoakBinding{Schema: SoakVerdictSchema, RunID: d.RunID, BootID: observedBoot(d, "m01"), Root: runRoot(d, "m01")}
	if b.RunID == "" || b.BootID == "" || b.Root == "" {
		return b, errors.New("soak assessor needs the run id, the admitted M01 boot and the M01 run root")
	}
	return b, nil
}

// SoakAssessArgs is the exact soak_assess.py argv for the M01 verdict job.
func SoakAssessArgs(d *inventory.Document, s SmokeSettings, x *SoakPlan) ([]string, error) {
	b, err := soakBinding(d)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(map[string]any{"schema": b.Schema, "root": b.Root, "run_id": b.RunID, "boot_id": b.BootID,
		"duration_s": x.DurationS, "quiescence_s": int(QuiescenceReserve.Seconds()), "client_kill_at_s": x.ClientKillAtS,
		"collection": x.Layout.Collection})
	return []string{"-I", path.Join(s.HelperDir, "soak_assess.py"), "--config", string(raw)}, err
}

// soakAssessHelpers are the pinned sources the verdict job imports.
var soakAssessHelpers = []string{"soak_assess.py", "soak_load.py", "soak_native_worker.py", "soak_oracle.py", "soak_stats.py"}

// SoakVerdict is the assessor's one bounded row. Go decodes and integrates
// it; the replay itself lives only in soak_assess.py.
type SoakVerdict struct {
	Status   string `json:"status"`
	Criteria map[string]struct {
		Outcome string `json:"outcome"`
		Reason  string `json:"reason"`
	} `json:"criteria"`
	K7        []map[string]any `json:"k7"`
	Resources string           `json:"resources"`
	Binding   SoakBinding      `json:"binding"`
	Error     string           `json:"error,omitempty"`
}

// verdictFields are the decoded field names. encoding/json matches struct
// fields case-insensitively, so a key that folds to one of them must be it.
var verdictFields = []string{"status", "criteria", "k7", "resources", "binding", "error", "outcome", "reason", "schema", "run_id", "boot_id", "root"}

// oneStrictValue admits exactly one JSON value with no duplicate object keys
// and no case-folded alias of a decoded field.
func oneStrictValue(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := uniqueKeys(dec); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("more than one JSON value")
	}
	return nil
}

func uniqueKeys(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil || (tok != json.Delim('{') && tok != json.Delim('[')) {
		return err
	}
	seen := map[string]bool{}
	for dec.More() {
		if tok == json.Delim('{') {
			k, err := dec.Token()
			if err != nil {
				return err
			}
			key := k.(string)
			if seen[key] {
				return fmt.Errorf("duplicate JSON key %q", key)
			}
			seen[key] = true
			for _, f := range verdictFields {
				if key != f && strings.EqualFold(key, f) {
					return fmt.Errorf("JSON key %q aliases %q", key, f)
				}
			}
		}
		if err = uniqueKeys(dec); err != nil {
			return err
		}
	}
	_, err = dec.Token()
	return err
}

// decodeVerdict accepts exactly one strict row for the expected run; a PASS
// must carry exactly the mandatory criteria, all PASS. Anything else fails closed.
func decodeVerdict(e tr.ExitInfo, want SoakBinding) (SoakVerdict, error) {
	var v SoakVerdict
	out, _, err := e.Output.Decode()
	if err != nil || e.Code != 0 {
		return v, errors.New("soak verdict job failed or its output is incomplete")
	}
	if err = oneStrictValue(out); err != nil {
		return v, fmt.Errorf("soak verdict is not one strict row: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(out))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&v); err != nil || (v.Status != "PASS" && v.Status != "FAIL") || v.Resources != "NOT_OBSERVED" {
		return v, errors.New("soak verdict malformed")
	}
	if v.Binding != want {
		return v, fmt.Errorf("soak verdict is bound to %+v, not this run", v.Binding)
	}
	for name, c := range v.Criteria {
		if !contains(soakCriteria, name) || (c.Outcome != "PASS" && c.Outcome != "FAIL") {
			return v, fmt.Errorf("soak verdict criterion %q unknown or malformed", name)
		}
		if c.Outcome != "PASS" && v.Status == "PASS" {
			return v, errors.New("soak verdict PASS with a failed criterion")
		}
	}
	if v.Status == "PASS" && (len(v.Criteria) != len(soakCriteria) || v.Error != "") {
		return v, errors.New("soak verdict PASS without every mandatory criterion")
	}
	return v, nil
}

// soakVerdict runs after the plan's ordinary collect jobs: the M02 evidence
// job, its bounded output published into the M01 run root, then the M01
// assessor. A FAIL or any error fails the pack; earlier failures already did.
func (r *runtimePack) soakVerdict(ctx context.Context, a *tr.ActionContext, p Plan, d *inventory.Document) (SoakVerdict, error) {
	var v SoakVerdict
	want, err := soakBinding(d)
	if err != nil {
		return v, err
	}
	_, e, err := a.RunApprovedPackJob(ctx, p.Soak.Evidence.Job, false)
	if err != nil {
		return v, err
	}
	m02, _, err := e.Output.Decode()
	var probe struct {
		Status string `json:"status"`
	}
	if err != nil || e.Code != 0 || oneStrictValue(m02) != nil || json.Unmarshal(m02, &probe) != nil || probe.Status != "SOAK_COLLECTED" {
		return v, errors.New("soak M02 evidence failed, truncated or malformed")
	}
	if err = publishEvidence(path.Join(want.Root, "m02-collect.json"), m02); err != nil {
		return v, err
	}
	if _, e, err = a.RunApprovedPackJob(ctx, p.Soak.Assess.Job, false); err != nil {
		return v, err
	}
	if v, err = decodeVerdict(e, want); err == nil && v.Status != "PASS" {
		err = errors.New("soak verdict FAIL")
	}
	return v, err
}
