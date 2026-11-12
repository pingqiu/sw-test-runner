package smoke

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
)

// ProjectBudget is one project's two caps (see BUDGET-POLICY.md).
type ProjectBudget struct {
	PerRunCapUSD     float64 `json:"per_run_cap_usd"`
	CumulativeCapUSD float64 `json:"cumulative_cap_usd"`
}

// budgetOverride is an explicit approval that raises one run's per-run cap.
// The raw config bytes, not this struct, go to the evidence.
type budgetOverride struct {
	MessageID string   `json:"message_id"`
	AmountUSD *float64 `json:"amount_usd"`
	Approver  string   `json:"approver,omitempty"`
	Message   string   `json:"message,omitempty"`
}

// defaultBudget is today's gate: $3 per run, $10 cumulative.
var defaultBudget = ProjectBudget{PerRunCapUSD: 3, CumulativeCapUSD: 10}

var (
	projectName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,49}$`)
	messageID   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
)

// budgetGate checks the run's declared budget, abort estimate, estimate and
// the project's cumulative spend against the project's caps, raised for this
// run only by a well-formed override. It returns the evidence for the plan
// and the run's budget.json. Every refusal has its own text.
func budgetGate(c Config, d declaration) (map[string]any, error) {
	project, caps := "default", defaultBudget
	if c.Project != "" && c.Project != "default" {
		project = c.Project
		if !projectName.MatchString(project) {
			return nil, errors.New("budget project name must match [a-z0-9][a-z0-9-]{0,49}")
		}
	}
	if _, redefined := c.Budgets["default"]; redefined {
		return nil, errors.New("default budget is built in ($3 per run, $10 cumulative) and cannot be redefined")
	}
	if project != "default" {
		var known bool
		if caps, known = c.Budgets[project]; !known {
			return nil, fmt.Errorf("unknown budget project %q: add it to budgets", project)
		}
		if !(caps.PerRunCapUSD > 0) || !(caps.CumulativeCapUSD > 0) || math.IsInf(caps.CumulativeCapUSD, 0) {
			return nil, fmt.Errorf("budget project %q: caps must be positive", project)
		}
		if caps.PerRunCapUSD > caps.CumulativeCapUSD {
			return nil, fmt.Errorf("budget project %q: per-run cap exceeds cumulative cap", project)
		}
	}
	runCap := caps.PerRunCapUSD
	var override any
	if len(c.Override) > 0 {
		var o budgetOverride
		if e := StrictJSON(c.Override, &o); e != nil {
			return nil, fmt.Errorf("malformed budget override: %v", e)
		}
		if !messageID.MatchString(o.MessageID) {
			return nil, errors.New("budget override needs message_id (the approval message id, [A-Za-z0-9._:-], max 128)")
		}
		if o.AmountUSD == nil || !(*o.AmountUSD > 0) || math.IsInf(*o.AmountUSD, 0) {
			return nil, errors.New("budget override amount_usd must be a positive number")
		}
		if *o.AmountUSD <= caps.PerRunCapUSD {
			return nil, fmt.Errorf("budget override must raise the per-run cap: amount_usd %.2f <= project cap %.2f", *o.AmountUSD, caps.PerRunCapUSD)
		}
		runCap = *o.AmountUSD
		override = json.RawMessage(c.Override)
	}
	switch {
	case c.PriorUSD < 0:
		return nil, errors.New("prior_estimated_usd must not be negative")
	case d.Run.Budget <= 0 || d.Run.Budget > runCap:
		return nil, fmt.Errorf("run_declaration.budget_usd exceeds per-run cap %.2f of project %q", runCap, project)
	case d.Run.Abort <= 0 || d.Run.Abort > d.Run.Budget:
		return nil, errors.New("abort_estimate_usd must be positive and <= budget_usd")
	case c.EstimatedUSD <= 0 || c.EstimatedUSD > d.Run.Abort:
		return nil, errors.New("estimated_usd must be positive and <= abort_estimate_usd")
	case c.PriorUSD+c.EstimatedUSD > caps.CumulativeCapUSD:
		return nil, fmt.Errorf("prior %.2f + estimate %.2f exceeds cumulative cap %.2f of project %q (an override does not raise it)", c.PriorUSD, c.EstimatedUSD, caps.CumulativeCapUSD, project)
	}
	return map[string]any{
		"project":                  project,
		"per_run_cap_usd":          runCap,
		"project_per_run_cap_usd":  caps.PerRunCapUSD,
		"cumulative_cap_usd":       caps.CumulativeCapUSD,
		"prior_estimated_usd":      c.PriorUSD,
		"cumulative_estimated_usd": c.PriorUSD + c.EstimatedUSD,
		"override":                 override,
	}, nil
}

// EncodeVerbatim writes v as one JSON line without HTML escaping, so an
// approval message containing <, > or & reaches the evidence byte for byte
// (json.Marshal and a default Encoder rewrite them as <, >, &).
func EncodeVerbatim(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

// marshalVerbatim is EncodeVerbatim into bytes, without the trailing newline.
func marshalVerbatim(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := EncodeVerbatim(&buf, v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}
