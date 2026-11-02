// Package seaweed binds shared deployment phases to the strict S0b runtime.
package seaweed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/inventory"
	"gopkg.in/yaml.v3"
)

// ErrS0bUnavailable refuses any caller without the exact ApprovedRun context.
// Caller variables, a plan result or registry flags cannot bypass it.
var ErrS0bUnavailable = errors.New("S0B_UNAVAILABLE: strict S0b admission absent; remedy: use the protected approved-run adapter; no execution authorized")

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

// RegisterPack uses owned S0b jobs only; it exposes no raw shell/provider route.
// It is opt-in registration and is not wired into the CLI.
func RegisterPack(r *tr.Registry) {
	runtime := &runtimePack{sessions: map[*tr.RunScope]*session{}, pause: func() <-chan time.Time { return time.After(30 * time.Second) }}
	r.RegisterFunc("seaweed_inventory_validate", tr.TierDevOps, validateInventory)
	r.RegisterFunc("seaweed_plan", tr.TierDevOps, plan)
	for _, name := range []string{"seaweed_preflight", "seaweed_deploy", "seaweed_workload", "seaweed_collect", "seaweed_cleanup"} {
		r.RegisterMutatingFunc(name, tr.TierDevOps, runtime.run)
	}
	r.RegisterMutatingFunc("seaweed_read_restart", tr.TierChaos, runtime.run)
	r.RegisterMutatingFunc(tr.SoakAction, tr.TierChaos, runtime.run)
	r.RegisterLeaseEnvelope(leaseEnvelope)
}

// leaseEnvelope lets a worker-owned run hold the lab past ten minutes only when
// its pinned scenario is a valid KV soak plan under the same bind every phase
// runs (here against the declared pins and boots that readback must later
// equal). Anything else, including a soak label on another plan, is refused.
func leaseEnvelope(in tr.PackInputs) (time.Duration, error) {
	var sealed tr.Scenario
	if err := yaml.Unmarshal(in.Scenario, &sealed); err != nil {
		return 0, err
	}
	p, d, err := decodePlan(sealed.Metadata["seaweed_plan"])
	if err != nil {
		return 0, err
	}
	if p.Soak == nil {
		return 0, errors.New("not a KV soak plan")
	}
	if err = bind(p, d, in, clock()); err != nil {
		return 0, err
	}
	return tr.SoakHorizon, nil
}
func invalid(action string, err error) (map[string]string, error) {
	const hint = "correct inventory against seaweed.inventory/v1 and preserve requested scope"
	return result("refused", "INVENTORY_INVALID", hint, action, nil), fmt.Errorf("INVENTORY_INVALID: %w; remedy: %s", err, hint)
}
func validateInventory(_ context.Context, _ *tr.ActionContext, a tr.Action) (map[string]string, error) {
	d, err := inventory.Decode([]byte(a.Params["inventory_json"]))
	if err != nil {
		return invalid(a.Action, err)
	}
	return result("valid_data_only", "OK", "validation grants no execution; obtain S0b admission after the adapter lands", a.Action, d), nil
}
func plan(_ context.Context, _ *tr.ActionContext, a tr.Action) (map[string]string, error) {
	if raw, ok := a.Params["plan_json"]; ok {
		r, err := PlanPhase(raw, a.Params["for_action"])
		b, _ := json.Marshal(r)
		return map[string]string{"value": string(b), "execution_authorized": "false"}, err
	}
	name := a.Params["for_action"]
	if !phase(name) {
		const hint = "select one registered Seaweed runtime phase"
		return result("refused", "ACTION_UNSUPPORTED", hint, name, nil), fmt.Errorf("ACTION_UNSUPPORTED: remedy: %s", hint)
	}
	d, err := inventory.Decode([]byte(a.Params["inventory_json"]))
	if err != nil {
		return invalid(name, err)
	}
	// This skeleton has no executable mutations. Do not fabricate a deploy plan.
	return result("blocked_plan", "S0B_UNAVAILABLE", "wait for the reviewed S0b adapter and executable phase plan", name, d), nil
}
func unavailable(_ context.Context, _ *tr.ActionContext, a tr.Action) (map[string]string, error) {
	return result("refused", "S0B_UNAVAILABLE", "use the protected approved-run adapter; do not bypass admission", a.Action, nil), ErrS0bUnavailable
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
	r.Code = "S0B_REQUIRED"
	r.Remedy = "obtain independent GO, live lease and protected readback before effects"
	r.State = d.State
	r.Transport = d.Transport
	r.PlannedSteps = p.steps(action)
	r.Deadlines = &d.Deadlines
	r.Cleanup = "settle only this S0b scope's POSSIBLE/owned issuers; preserve UNKNOWN; no disk/cloud deletion"
	return r, nil
}
