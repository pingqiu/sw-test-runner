package smoke

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/pingqiu/sw-test-runner/packs/seaweed"
	"golang.org/x/crypto/ssh"
)

// The block product's three-node R4 profile (three_node): node-1..node-3 of
// the already allowed Standard_D2s_v5, each with its own pinned server key,
// and the multi-node handling the ten-node cluster has.

func threeNodeFixture(t *testing.T) Config {
	c := fixture(t)
	setVar(t, &c, "three_node", true)
	return c
}

func TestThreeNodePlanDescribesThreeVMsAndCreatesNothing(t *testing.T) {
	f := &fake{}
	r := controller(f).Execute(context.Background(), "plan", threeNodeFixture(t))
	if r.Code != "PLAN_ONLY" || r.Plan["vm_count"] != 3 || r.Plan["vm_size"] != "Standard_D2s_v5" || len(f.events) != 0 || len(r.Effects) != 0 {
		t.Fatalf("three-node dry run must describe three D2s_v5 VMs without effects: %+v %v", r, f.events)
	}
}

// The profile widens nothing: only D2s_v5; L16s_v3, D4s_v5 and the ten-node
// cluster at the same time are refused before any call.
func TestThreeNodeAdmitsOnlyD2sV5AndNotWithTheCluster(t *testing.T) {
	for _, tc := range []struct {
		name, size string
		cluster    bool
	}{
		{"L16s_v3", "Standard_L16s_v3", false},
		{"D4s_v5", "Standard_D4s_v5", false},
		{"with the cluster", "Standard_D2s_v5", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := threeNodeFixture(t)
			setVar(t, &c, "vm_size", tc.size)
			if tc.cluster {
				setVar(t, &c, "cluster", true)
			}
			calls := 0
			r := (Controller{Command: func(context.Context, string, string, ...string) ([]byte, error) {
				calls++
				return nil, nil
			}}).Execute(context.Background(), "plan", c)
			if r.Code != "INPUT_REFUSED" || calls != 0 {
				t.Fatalf("code %s error %q calls %d, want INPUT_REFUSED with 0 calls", r.Code, r.Error, calls)
			}
		})
	}
}

// Before any create, host.auto.tfvars.json pins exactly node-1..node-3, each
// a distinct parseable ed25519 key; the pack gets the same three public pins;
// the three OS disks are tagged.
func TestThreeNodePinsOneKeyPerNodeBeforeCreate(t *testing.T) {
	c := threeNodeFixture(t)
	f := &fake{disks: 3}
	ctl := controller(f)
	var planned []string
	ctl.Command = func(ctx context.Context, dir, tool string, args ...string) ([]byte, error) {
		if tool == "terraform" && args[0] == "plan" {
			b, e := os.ReadFile(filepath.Join(dir, "host.auto.tfvars.json"))
			if e != nil {
				t.Fatal(e)
			}
			var v struct {
				Keys  map[string]map[string]string `json:"node_host_keys"`
				RunID string                       `json:"run_id"`
			}
			if e = json.Unmarshal(b, &v); e != nil || v.RunID == "" {
				t.Fatalf("host vars %v run %q", e, v.RunID)
			}
			seen := map[string]bool{}
			for id, k := range v.Keys {
				planned = append(planned, id)
				s, e := ssh.ParsePrivateKey([]byte(k["ed25519_private"]))
				if e != nil {
					t.Fatal(e)
				}
				pub := string(ssh.MarshalAuthorizedKey(s.PublicKey()))
				if seen[pub] || !strings.HasPrefix(k["ed25519_public"], "ssh-ed25519 ") {
					t.Fatal("shared or empty key", id)
				}
				seen[pub] = true
			}
		}
		return f.command(ctx, dir, tool, args...)
	}
	var pinned []string
	ctl.Pack = func(ctx context.Context, b []byte, cloud seaweed.CloudConfig) (string, error) {
		for id := range cloud.HostKeys {
			pinned = append(pinned, id)
		}
		return "bundle", nil
	}
	r := ctl.Execute(context.Background(), "run", c)
	sort.Strings(planned)
	sort.Strings(pinned)
	want := "node-1,node-2,node-3"
	if r.Code != "COMPLETE" || strings.Join(planned, ",") != want || strings.Join(pinned, ",") != want {
		t.Fatalf("code %s (%s) planned %v pinned %v", r.Code, r.Error, planned, pinned)
	}
	tags := 0
	for _, e := range f.events {
		if e == "az tag" {
			tags++
		}
	}
	if tags != 3 {
		t.Fatalf("want three OS disk tag updates, got %d: %v", tags, f.events)
	}
}

// Terraform reporting another number of OS disks than the profile's nodes
// refuses before the pack runs, and the run still cleans up.
func TestThreeNodeRefusesAWrongDiskCount(t *testing.T) {
	f := &fake{disks: 10}
	r := controller(f).Execute(context.Background(), "run", threeNodeFixture(t))
	if r.Code != "DISK_TAG_FAILED" || !strings.Contains(r.Error, "require 3 explicit OS disk IDs") || !f.cleanContext {
		t.Fatalf("%+v %v", r, f.events)
	}
}
