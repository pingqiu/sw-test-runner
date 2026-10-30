// Package seaweed is the shared deploy pack skeleton. Runtime integration is pending S0b-min.
package seaweed

import (
	"context"
	"errors"
	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/inventory"
)

// ErrS0bUnavailable is the unconditional runtime refusal until the reviewed
// S0b adapter lands. Caller variables or registry flags cannot bypass it.
var ErrS0bUnavailable = errors.New("seaweed pack: S0b-min admission unavailable; no execution authorized")

// RegisterPack reserves shared phase names without enabling execution.
// It is opt-in registration and is not wired into the CLI.
func RegisterPack(r *tr.Registry) {
	r.RegisterFunc("seaweed_inventory_validate", tr.TierDevOps, validateInventory)
	for _, name := range []string{"seaweed_preflight", "seaweed_deploy", "seaweed_workload", "seaweed_collect", "seaweed_cleanup"} {
		r.RegisterMutatingFunc(name, tr.TierDevOps, unavailable)
	}
}
func validateInventory(_ context.Context, _ *tr.ActionContext, a tr.Action) (map[string]string, error) {
	d, err := inventory.Decode([]byte(a.Params["inventory_json"]))
	if err != nil {
		return nil, err
	}
	return map[string]string{"status": "valid_data_only", "execution_authorized": "false", "state": d.State, "transport": d.Transport}, nil
}
func unavailable(context.Context, *tr.ActionContext, tr.Action) (map[string]string, error) {
	return nil, ErrS0bUnavailable
}
