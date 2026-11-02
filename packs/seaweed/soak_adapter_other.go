//go:build !linux

package seaweed

import (
	"context"
	"errors"

	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/inventory"
)

// The soak controller runs only on the admitted Linux M01 host.
func (r *runtimePack) soak(ctx context.Context, a *tr.ActionContext, p Plan, d *inventory.Document, in tr.PackInputs, s *session) (map[string]string, error) {
	return refusal("seaweed_kv_soak", "BINDING_REFUSED", errors.New("the soak controller runs only on Linux M01"))
}
