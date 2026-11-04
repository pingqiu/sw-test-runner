//go:build linux

package seaweed

import (
	"encoding/json"
	"github.com/pingqiu/sw-test-runner/inventory"
	"os"
	"strings"
	"testing"
	"time"
)

// newHTTPInventory is synthetic input data only: no policy, GO, lease or owned executor.
func newHTTPInventory(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile("../../examples/inventory/dc.resolved.json")
	if err != nil {
		t.Fatal(err)
	}
	d, err := inventory.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	h := d.Hosts[0]
	h.ID = "m02"
	h.Roles = []string{"master", "filer", "volume", "s3", "client"}
	h.RunRoot = "/run/pack-cpu"
	h.Management.Address = "192.0.2.2"
	h.Data.Address = "192.0.2.2"
	h.RDMA = inventory.RDMA{Status: "not_applicable"}
	h.ResourceID = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/test/providers/Microsoft.Compute/virtualMachines/smoke"
	services := h.Services
	h.Services = nil
	for _, s := range services {
		if !strings.HasPrefix(s.Name, "rdma") && s.Name != "dc_control" {
			h.Services = append(h.Services, s)
		}
	}
	d.RunID = "pack-cpu"
	d.Target = "azure"
	d.Transport = "http"
	d.State = "requested"
	d.Observation = nil
	d.Hosts = []inventory.Host{h}
	d.ManagedResourceIDs = []string{h.ResourceID}
	d.ArtifactManifestSHA256 = strings.Repeat("a", 64)
	d.Deadlines = inventory.Deadlines{Work: time.Now().Add(90 * time.Second).UTC().Format(time.RFC3339Nano), Hard: time.Now().Add(100 * time.Second).UTC().Format(time.RFC3339Nano), CleanupReserveMS: 5000}
	raw, err = json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
