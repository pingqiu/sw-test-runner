package smoke

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/pingqiu/sw-test-runner/packs/seaweed"
	"golang.org/x/crypto/ssh"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func clusterFixture(t *testing.T) Config {
	c := fixture(t)
	b, _ := os.ReadFile(c.Variables)
	var v map[string]any
	json.Unmarshal(b, &v)
	v["cluster"] = true
	b, _ = json.Marshal(v)
	os.WriteFile(c.Variables, b, 0600)
	c.VariablesSHA256 = digest(b)
	return c
}

func TestTenNodeKeysBeforeApplyAndPartialCreateStillDestroys(t *testing.T) {
	c := clusterFixture(t)
	f := &fake{fail: "apply"}
	ctl := controller(f)
	checked := false
	ctl.Command = func(ctx context.Context, dir, tool string, args ...string) ([]byte, error) {
		if tool == "terraform" && args[0] == "plan" {
			b, e := os.ReadFile(filepath.Join(dir, "host.auto.tfvars.json"))
			if e != nil {
				t.Fatal(e)
			}
			var v struct {
				Keys map[string]map[string]string `json:"node_host_keys"`
			}
			if json.Unmarshal(b, &v) != nil || len(v.Keys) != 10 {
				t.Fatal("missing per-node keys before create")
			}
			seen := map[string]bool{}
			for id, k := range v.Keys {
				s, e := ssh.ParsePrivateKey([]byte(k["ed25519_private"]))
				if e != nil {
					t.Fatal(e)
				}
				pub := string(ssh.MarshalAuthorizedKey(s.PublicKey()))
				if seen[pub] || len(k["ed25519_public"]) == 0 {
					t.Fatal("shared/empty key", id)
				}
				seen[pub] = true
			}
			checked = true
		}
		return f.command(ctx, dir, tool, args...)
	}
	r := ctl.Execute(context.Background(), "run", c)
	if !checked || r.Code != "APPLY_FAILED_OR_UNKNOWN" || !r.CreateUnresolved || !f.cleanContext {
		t.Fatalf("%+v checked=%v events=%v", r, checked, f.events)
	}
}

func TestTenNodePassesEveryPinnedKeyToPack(t *testing.T) {
	c := clusterFixture(t)
	f := &fake{}
	ctl := controller(f)
	ctl.Pack = func(ctx context.Context, b []byte, cloud seaweed.CloudConfig) (string, error) {
		if len(cloud.HostKeys) != 10 {
			t.Fatal("ten pins not passed to pack")
		}
		return "bundle", nil
	}
	if r := ctl.Execute(context.Background(), "run", c); r.Code != "COMPLETE" {
		t.Fatalf("%+v", r)
	}
}

func TestTenNodePlanDoesNotCreate(t *testing.T) {
	f := &fake{}
	r := controller(f).Execute(context.Background(), "plan", clusterFixture(t))
	if r.Code != "PLAN_ONLY" || r.Plan["vm_count"] != 10 || len(f.events) != 0 || len(r.Effects) != 0 {
		t.Fatalf("ten-node dry-run must describe ten VMs without effects: %+v %+v", r, f.events)
	}
}

func TestTenNodeExecutionReachesDefaultPackAndStillCleansUp(t *testing.T) {
	f := &fake{}
	r := (Controller{Command: f.command}).Execute(context.Background(), "run", clusterFixture(t))
	if r.Code != "PACK_FAILED" || !strings.Contains(r.Error, "exact") || !r.RemainingObserved || !f.cleanContext || !strings.Contains(strings.Join(f.events, ","), "terraform apply") {
		t.Fatalf("%+v %v", r, f.events)
	}
}

func TestFailedClusterStandaloneRecoveryDoesNotNeedPack(t *testing.T) {
	c := clusterFixture(t)
	f := &fake{fail: "pack", live: true}
	firstDestroy := true
	command := func(ctx context.Context, dir, tool string, args ...string) ([]byte, error) {
		if tool == "terraform" && args[0] == "destroy" {
			if firstDestroy {
				firstDestroy = false
				return nil, errors.New("interrupted cleanup")
			}
			f.live = false
		}
		return f.command(ctx, dir, tool, args...)
	}
	broken := (Controller{Command: command, Pack: f.pack}).Execute(context.Background(), "run", c)
	if broken.Code != "CLEANUP_INCOMPLETE" || len(broken.Remaining) != 1 {
		t.Fatalf("failed run did not leave the simulated VM: %+v", broken)
	}
	c.Source = "/original-checkout-gone"
	c.Cloud.Files = nil
	recovery := Controller{Command: command}
	for _, mode := range []string{"destroy", "verify"} {
		result := recovery.Execute(context.Background(), mode, c)
		want := "COMPLETE"
		if mode == "verify" {
			want = "EMPTY"
		}
		if result.Code != want || !result.RemainingObserved || len(result.Remaining) != 0 {
			t.Fatalf("pack-free %s failed: %+v", mode, result)
		}
	}
}
