//go:build linux

package seaweed

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/pingqiu/sw-test-runner/inventory"
)

// Replication 001 is opt-in and cloud HTTP only; absent leaves every argv,
// config and URL exactly as before.
func TestReplicationIsOptInAndCloudOnly(t *testing.T) {
	f := newHTTPFixture(t)
	var d inventory.Document
	if err := json.Unmarshal([]byte(f.plan.Requested), &d); err != nil {
		t.Fatal(err)
	}
	d.State = "resolved"
	d.Hosts[0].Roles = []string{"master", "filer", "volume", "s3", "client"}
	d.Hosts[0].Services = []inventory.Service{{Name: "filer_http", Port: 8888}, {Name: "filer_grpc", Port: 18888}, {Name: "volume_http", Port: 8080}}
	s := SmokeSettings{NativeDir: "/r", HelperDir: "/r/worker", Keys: 16, Length: 1 << 20}
	plain, err := SmokeArgs(&d, d.Hosts[0].ID, s)
	if err != nil || strings.Contains(plain[3], "replication") {
		t.Fatal("default config changed:", plain, err)
	}
	s.Replication = "001"
	args, err := SmokeArgs(&d, d.Hosts[0].ID, s)
	if err != nil || !strings.Contains(args[3], `"replication":"001"`) {
		t.Fatal("001 not carried to the loader:", args, err)
	}
	for _, bad := range []string{"000", "010", "002"} {
		s.Replication = bad
		if _, err = SmokeArgs(&d, d.Hosts[0].ID, s); err == nil {
			t.Fatal("accepted replication", bad)
		}
	}
	d.Transport, s.Replication = "dc", "001"
	if _, err = SmokeArgs(&d, d.Hosts[0].ID, s); err == nil {
		t.Fatal("lab DC accepted replication")
	}

	if cloudGrowReplication("") != "" || cloudGrowReplication("001") != "&replication=001" {
		t.Fatal("grow replication parameter")
	}
	h := inventory.Host{Services: []inventory.Service{{Name: "master_http", Port: 9333}}}
	if !strings.Contains(cloudServices(h, h, "/r", true, "")["master"], "-defaultReplication=000 ") || !strings.Contains(cloudServices(h, h, "/r", true, "001")["master"], "-defaultReplication=001 ") {
		t.Fatal("master default replication")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	raw := cloudInput(t, "127.0.0.1:1") // a valid one-VM inventory
	files := map[string]CloudFile{"weed": {}, "weed-volume": {}, "native.tar": {}}
	for _, rep := range []string{"001", "002"} {
		if _, err := RunCloudHTTP(ctx, raw, CloudConfig{Files: files, Replication: rep}); err == nil || !strings.Contains(err.Error(), "replication") {
			t.Fatal("one-VM or invalid replication accepted:", rep, err)
		}
	}
	for _, c := range []CloudConfig{{ChaosSet: "A"}, {ChaosSet: "C", Replication: "001"}, {ChaosSet: "B", Replication: "001", Chaos: true}} {
		c.Files = files
		if _, err := RunCloudHTTP(ctx, raw, c); err == nil || !strings.Contains(err.Error(), "chaos") {
			t.Fatalf("invalid chaos_set accepted: %+v %v", c, err)
		}
	}
}
