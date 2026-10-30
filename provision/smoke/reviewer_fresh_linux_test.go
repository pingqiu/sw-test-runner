//go:build linux

package smoke

import (
	"context"
	"encoding/json"
	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/infra"
	"github.com/pingqiu/sw-test-runner/packs/seaweed"
	"testing"
)

func TestReviewerFreshRejectsForeignExportBeforeSetup(t *testing.T) {
	for _, mode := range []string{"resource", "node", "role"} {
		t.Run(mode, func(t *testing.T) {
			c, a, raw, _ := freshFixture(t)
			var doc map[string]any
			if e := json.Unmarshal(raw, &doc); e != nil {
				t.Fatal(e)
			}
			host := doc["hosts"].([]any)[0].(map[string]any)
			switch mode {
			case "resource":
				doc["managed_resource_ids"] = []any{"/foreign/resource"}
			case "node":
				host["id"] = "foreign"
			case "role":
				host["roles"] = []any{"client"}
			}
			raw, _ = json.Marshal(doc)
			setups := 0
			old := a.setup
			a.setup = func(ctx context.Context, c Config, raw []byte) ([]byte, error) { setups++; return old(ctx, c, raw) }
			f := &fake{}
			command := func(ctx context.Context, dir, tool string, args ...string) ([]byte, error) {
				b, e := f.command(ctx, dir, tool, args...)
				if tool == "terraform" && args[0] == "output" {
					return raw, e
				}
				return b, e
			}
			r := (Controller{Command: command, Pack: f.pack, fresh: a}).Execute(context.Background(), "run", c)
			if r.Code == "COMPLETE" {
				t.Fatal("foreign export qualified")
			}
			if setups != 0 {
				t.Fatalf("foreign %s reached mutating trusted setup %d time(s) before export qualification; result=%s", mode, setups, r.Code)
			}
		})
	}
}

func TestReviewerFreshManagementSlotMustBeLiteralIP(t *testing.T) {
	c, a, raw, j := freshFixture(t)
	var doc map[string]any
	if e := json.Unmarshal(raw, &doc); e != nil {
		t.Fatal(e)
	}
	doc["hosts"].([]any)[0].(map[string]any)["management"].(map[string]any)["address"] = "vm.example.test"
	raw, _ = json.Marshal(doc)
	f := &fake{}
	command := func(ctx context.Context, dir, tool string, args ...string) ([]byte, error) {
		b, e := f.command(ctx, dir, tool, args...)
		if tool == "terraform" && args[0] == "output" {
			return raw, e
		}
		return b, e
	}
	pack := func(ctx context.Context, request, policy string, _ []byte) (string, error) {
		runtime := tr.ApprovedRuntime{Jobs: j, Readback: func(ctx context.Context, p infra.DeploymentPolicy) (infra.DeploymentReceipt, error) {
			return a.readback(ctx, p, "")
		}}
		r, b, e := tr.RunApprovedRequest(ctx, request, policy, seaweed.RegisterPack, &runtime, nil)
		if e != nil {
			return b, e
		}
		if r == nil || r.Status != tr.StatusPass {
			t.Fatalf("fixture strict path unexpectedly failed: %+v", r)
		}
		return b, nil
	}
	r := (Controller{Command: command, Pack: pack, fresh: a}).Execute(context.Background(), "run", c)
	if r.Code == "COMPLETE" || j.n != 0 {
		t.Fatalf("DNS hostname widened IP-only authority through normal strict entry: code=%s authorized_jobs=%d", r.Code, j.n)
	}
}
