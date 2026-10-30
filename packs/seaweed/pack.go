// Package seaweed is the shared deploy pack skeleton. Runtime integration is pending S0b-min.
package seaweed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/inventory"
)

// ErrS0bUnavailable is unconditional until the reviewed S0b adapter lands.
// Caller variables, a plan result or registry flags cannot bypass it.
var ErrS0bUnavailable = errors.New("S0B_UNAVAILABLE: S0b-min admission unavailable; remedy: wait for the reviewed adapter; no execution authorized")

// Result is the machine contract in ActionResult.Output (the Registry value key).
// Effects lists executable changes, not speculative future implementation steps.
type Result struct {
	Schema              string   `json:"schema"`
	Status              string   `json:"status"`
	Code                string   `json:"code"`
	Remedy              string   `json:"remedy"`
	Action              string   `json:"action"`
	ExecutionAuthorized bool     `json:"execution_authorized"`
	Effects             []string `json:"effects"`
	State               string   `json:"state,omitempty"`
	Transport           string   `json:"transport,omitempty"`
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
	switch name {
	case "seaweed_preflight", "seaweed_deploy", "seaweed_workload", "seaweed_collect", "seaweed_cleanup":
		return true
	}
	return false
}

// RegisterPack reserves shared phase names without enabling execution.
// It is opt-in registration and is not wired into the CLI.
func RegisterPack(r *tr.Registry) {
	r.RegisterFunc("seaweed_inventory_validate", tr.TierDevOps, validateInventory)
	r.RegisterFunc("seaweed_plan", tr.TierDevOps, plan)
	for _, name := range []string{"seaweed_preflight", "seaweed_deploy", "seaweed_workload", "seaweed_collect", "seaweed_cleanup"} {
		r.RegisterMutatingFunc(name, tr.TierDevOps, unavailable)
	}
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
	return result("refused", "S0B_UNAVAILABLE", "wait for the reviewed S0b adapter; do not retry or bypass admission", a.Action, nil), ErrS0bUnavailable
}
