// Package seaweed provides shared plans, evidence leaves and the direct execution path.
package seaweed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/inventory"
)

// Result is the machine contract in ActionResult.Output (the Registry value key).
// Effects lists executable changes, not speculative future implementation steps.
type Result struct {
	Schema              string               `json:"schema"`
	Status              string               `json:"status"`
	Code                string               `json:"code"`
	Remedy              string               `json:"remedy"`
	Action              string               `json:"action"`
	ExecutionAuthorized bool                 `json:"execution_authorized"`
	Effects             []string             `json:"effects"`
	State               string               `json:"state,omitempty"`
	Transport           string               `json:"transport,omitempty"`
	PlannedSteps        []Step               `json:"planned_steps,omitempty"`
	Cleanup             string               `json:"cleanup,omitempty"`
	Deadlines           *inventory.Deadlines `json:"deadlines,omitempty"`
}

func result(status, code, remedy, action string, d *inventory.Document) map[string]string {
	r := Result{Schema: "seaweed.pack-result/v1", Status: status, Code: code, Remedy: remedy, Action: action, Effects: []string{}}
	if d != nil {
		r.State = d.State
		r.Transport = d.Transport
	}
	// Result has only strings/bool/slices of strings; encoding cannot fail.
	b, _ := json.Marshal(r)
	return map[string]string{"value": string(b), "execution_authorized": "false"}
}
func phase(name string) bool {
	return contains(phases, name) || name == "seaweed_read_restart" || name == "seaweed_kv_soak"
}

// RegisterPack retains only local validation/planning; execution is seaweed-cloud direct.
func RegisterPack(r *tr.Registry) {
	r.RegisterFunc("seaweed_inventory_validate", tr.TierDevOps, validateInventory)
	r.RegisterFunc("seaweed_plan", tr.TierDevOps, plan)
}

// leaseEnvelope lets a worker-owned run hold the lab past ten minutes only when
// its pinned scenario is a valid KV soak plan under the same bind every phase
// runs (here against the declared pins and boots that readback must later
// equal). Anything else, including a soak label on another plan, is refused.

func invalid(action string, err error) (map[string]string, error) {
	const hint = "correct inventory against seaweed.inventory/v1 and preserve requested scope"
	return result("refused", "INVENTORY_INVALID", hint, action, nil), fmt.Errorf("INVENTORY_INVALID: %w; remedy: %s", err, hint)
}
func validateInventory(_ context.Context, _ *tr.ActionContext, a tr.Action) (map[string]string, error) {
	d, err := inventory.Decode([]byte(a.Params["inventory_json"]))
	if err != nil {
		return invalid(a.Action, err)
	}
	return result("valid_data_only", "OK", "validation grants no execution; use seaweed-cloud direct to run", a.Action, d), nil
}
func plan(_ context.Context, _ *tr.ActionContext, a tr.Action) (map[string]string, error) {
	if raw, ok := a.Params["plan_json"]; ok {
		r, err := PlanPhase(raw, a.Params["for_action"])
		b, _ := json.Marshal(r)
		return map[string]string{"value": string(b), "execution_authorized": "false"}, err
	}
	name := a.Params["for_action"]
	if !phase(name) {
		const hint = "select one supported Seaweed plan phase"
		return result("refused", "ACTION_UNSUPPORTED", hint, name, nil), fmt.Errorf("ACTION_UNSUPPORTED: remedy: %s", hint)
	}
	d, err := inventory.Decode([]byte(a.Params["inventory_json"]))
	if err != nil {
		return invalid(name, err)
	}
	// This skeleton has no executable mutations. Do not fabricate a deploy plan.
	return result("blocked_plan", "PLAN_INPUT_REQUIRED", "supply plan_json for data-only planning; execution uses seaweed-cloud direct", name, d), nil
}

// PlanPhase is a local dry-run: no authority, file access, native import or jobs.
func PlanPhase(raw, action string) (Result, error) {
	r := Result{Schema: "seaweed.pack-result/v1", Status: "refused", Code: "PLAN_INVALID", Remedy: "supply the exact joined inventory and explicit phase plan", Action: action, Effects: []string{}}
	if !phase(action) {
		r.Code = "ACTION_UNSUPPORTED"
		return r, errors.New(r.Code)
	}
	p, d, err := decodePlan(raw)
	if err != nil {
		return r, fmt.Errorf("PLAN_INVALID: %w", err)
	}
	r.Status = "plan_data_only"
	r.Code = "PLAN_DATA_ONLY"
	r.Remedy = "planning grants no execution; use seaweed-cloud direct"
	r.State = d.State
	r.Transport = d.Transport
	r.PlannedSteps = p.steps(action)
	r.Deadlines = &d.Deadlines
	r.Cleanup = "planning performs no cleanup; direct execution owns cleanup and postflight"
	return r, nil
}
