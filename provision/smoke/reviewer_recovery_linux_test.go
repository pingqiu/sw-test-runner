package smoke

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pingqiu/sw-test-runner/packs/seaweed"
	"golang.org/x/crypto/ssh"
)

func TestReviewerClusterRecoveryDoesNotRequireWorkloadPack(t *testing.T) {
	for _, failure := range []string{"destroy", "apply"} {
		for _, mode := range []string{"destroy", "verify"} {
			t.Run(failure+"/"+mode, func(t *testing.T) {
				c := clusterFixture(t)
				first := controller(&fake{fail: failure}).Execute(context.Background(), "run", c)
				if first.Code != "CLEANUP_INCOMPLETE" && first.Code != "APPLY_FAILED_OR_UNKNOWN" {
					t.Fatalf("setup failed: %+v", first)
				}
				next := &fake{}
				got := (Controller{Command: next.command}).Execute(context.Background(), mode, c)
				if got.Code == "CLUSTER_PACK_UNAVAILABLE" || len(next.events) == 0 {
					t.Fatalf("recovery blocked by unavailable workload pack: mode=%s code=%s events=%v", mode, got.Code, next.events)
				}
				if failure == "apply" && got.Code != "CREATE_UNRESOLVED" {
					t.Fatalf("UNKNOWN create was not retained: %+v", got)
				}
			})
		}
	}
}

func TestReviewerClusterKeyPairsMatchPublicPinsAndKeepClientSecretLocal(t *testing.T) {
	c := clusterFixture(t)
	const sentinel = "REVIEW-ONLY-CLIENT-PRIVATE-KEY-CONTENT"
	if err := os.WriteFile(c.Cloud.SSHKey, []byte(sentinel), 0600); err != nil {
		t.Fatal(err)
	}
	f := &fake{}
	ctl := controller(f)
	pins := map[string]string{}
	ctl.Command = func(ctx context.Context, dir, tool string, args ...string) ([]byte, error) {
		if tool == "terraform" && args[0] == "plan" {
			p := filepath.Join(dir, "host.auto.tfvars.json")
			b, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(b), sentinel) {
				t.Fatal("client private key entered Terraform input")
			}
			info, err := os.Stat(p)
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatal("host key input not private")
			}
			var v struct {
				Keys map[string]map[string]string `json:"node_host_keys"`
			}
			if err = json.Unmarshal(b, &v); err != nil || len(v.Keys) != 10 {
				t.Fatal("missing ten key pairs")
			}
			seen := map[string]bool{}
			for _, id := range []string{"meta", "vol1", "vol2", "vol3", "vol4", "vol5", "vol6", "client1", "client2", "client3"} {
				key := v.Keys[id]
				signer, err := ssh.ParsePrivateKey([]byte(key["ed25519_private"]))
				if err != nil {
					t.Fatal("invalid generated server key")
				}
				pub := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
				if key["ed25519_public"] != pub || seen[pub] {
					t.Fatal("mismatched/shared generated server key")
				}
				seen[pub] = true
				pins[id] = pub
			}
		}
		return f.command(ctx, dir, tool, args...)
	}
	ctl.Pack = func(ctx context.Context, b []byte, cloud seaweed.CloudConfig) (string, error) {
		if cloud.SSHKey != c.Cloud.SSHKey || len(cloud.HostKeys) != 10 {
			t.Fatal("cloud seam changed client key path/pin count")
		}
		for id, pin := range pins {
			if cloud.HostKeys[id] != pin {
				t.Fatal("server pin mismatch at pack seam")
			}
		}
		encoded, err := json.Marshal(cloud)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), sentinel) || strings.Contains(string(encoded), "BEGIN OPENSSH PRIVATE KEY") {
			t.Fatal("private material serialized into cloud config")
		}
		return f.pack(ctx, b, cloud)
	}
	if r := ctl.Execute(context.Background(), "run", c); r.Code != "COMPLETE" {
		t.Fatalf("%+v", r)
	}
}

func TestReviewerConflictingGeneratedNodeHostKeysRefuseBeforeEffects(t *testing.T) {
	c := clusterFixture(t)
	b, err := os.ReadFile(c.Variables)
	if err != nil {
		t.Fatal(err)
	}
	var vars map[string]any
	if err = json.Unmarshal(b, &vars); err != nil {
		t.Fatal(err)
	}
	vars["node_host_keys"] = map[string]any{}
	b, err = json.Marshal(vars)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(c.Variables, b, 0600); err != nil {
		t.Fatal(err)
	}
	c.VariablesSHA256 = digest(b)
	f := &fake{}
	got := controller(f).Execute(context.Background(), "run", c)
	if got.Code != "INPUT_REFUSED" || len(f.events) != 0 {
		t.Fatalf("caller node_host_keys accepted although run.auto overrides generated pins: code=%s events=%v", got.Code, f.events)
	}
}
