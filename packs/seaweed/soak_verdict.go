package seaweed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path"

	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/inventory"
)

// SoakAssessArgs is the exact soak_assess.py argv for the M01 verdict job.
func SoakAssessArgs(d *inventory.Document, s SmokeSettings, x *SoakPlan) ([]string, error) {
	if runRoot(d, "m01") == "" {
		return nil, errors.New("soak assessor needs the M01 run root")
	}
	raw, err := json.Marshal(map[string]any{"root": runRoot(d, "m01"), "duration_s": x.DurationS,
		"client_kill_at_s": x.ClientKillAtS, "collection": x.Layout.Collection})
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
	Error     string           `json:"error,omitempty"`
}

// decodeVerdict accepts exactly one strict row; anything else fails closed.
func decodeVerdict(e tr.ExitInfo) (SoakVerdict, error) {
	var v SoakVerdict
	out, _, err := e.Output.Decode()
	if err != nil || e.Code != 0 {
		return v, errors.New("soak verdict job failed or its output is incomplete")
	}
	if lines := bytes.Split(bytes.TrimSpace(out), []byte("\n")); len(lines) != 1 {
		return v, errors.New("soak verdict must be exactly one row")
	}
	dec := json.NewDecoder(bytes.NewReader(out))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&v); err != nil || (v.Status != "PASS" && v.Status != "FAIL") || v.Resources != "NOT_OBSERVED" {
		return v, errors.New("soak verdict malformed")
	}
	if v.Status == "PASS" && (len(v.Criteria) == 0 || v.Error != "") {
		return v, errors.New("soak verdict PASS without criteria")
	}
	for _, c := range v.Criteria {
		if c.Outcome != "PASS" && v.Status == "PASS" {
			return v, errors.New("soak verdict PASS with a failed criterion")
		}
	}
	return v, nil
}

// soakVerdict runs after the plan's ordinary collect jobs: the M02 evidence
// job, its bounded output published into the M01 run root, then the M01
// assessor. A FAIL or any error fails the pack; earlier failures already did.
func (r *runtimePack) soakVerdict(ctx context.Context, a *tr.ActionContext, p Plan, d *inventory.Document) (SoakVerdict, error) {
	var v SoakVerdict
	_, e, err := a.RunApprovedPackJob(ctx, p.Soak.Evidence.Job, false)
	if err != nil {
		return v, err
	}
	m02, _, err := e.Output.Decode()
	var probe struct {
		Status string `json:"status"`
	}
	if err != nil || e.Code != 0 || json.Unmarshal(m02, &probe) != nil || probe.Status != "SOAK_COLLECTED" {
		return v, errors.New("soak M02 evidence failed, truncated or malformed")
	}
	if err = publishEvidence(path.Join(runRoot(d, "m01"), "m02-collect.json"), m02); err != nil {
		return v, err
	}
	if _, e, err = a.RunApprovedPackJob(ctx, p.Soak.Assess.Job, false); err != nil {
		return v, err
	}
	if v, err = decodeVerdict(e); err == nil && v.Status != "PASS" {
		err = errors.New("soak verdict FAIL")
	}
	return v, err
}
