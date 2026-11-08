package seaweed

import (
	"encoding/json"
	"github.com/pingqiu/sw-test-runner/inventory"
	"os"
	"testing"
)

// Replay the preserved r9 row projection. Only the GPU row differs between
// rejection and acceptance; the explicit declaration preserves UNKNOWN.
func TestSWCtlR9DoctorProjectionNamesOnlyGPUException(t *testing.T) {
	raw, err := os.ReadFile("testdata/swctl.r9.doctor.json")
	if err != nil {
		t.Fatal(err)
	}
	var saved struct {
		Doctor    swctlReply
		Inventory inventory.Document
	}
	if err = json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	var profile swctlCluster
	profile.KV.Volume.RDMA.Push.Port = 18610
	profile.KV.Volume.RDMA.DC.Control = 18612
	profile.KV.LMCache.DC.Enabled = true
	profile.KV.Rotation.Enabled = true
	if allowedSmokeDoctor(saved.Doctor, saved.Inventory, profile) {
		t.Fatal("undeclared r9 GPU UNKNOWN accepted")
	}
	if !allowedSmokeDoctor(saved.Doctor, saved.Inventory, profile, "m01") {
		t.Fatal("declared r9 GPU UNKNOWN refused")
	}
	if allowedSmokeDoctor(saved.Doctor, saved.Inventory, profile, "m02") {
		t.Fatal("foreign declaration excused m01")
	}
	for n := range saved.Doctor.Items {
		i := &saved.Doctor.Items[n]
		if i.ID == "lmcache.shape" && i.Host == "m01" {
			i.Status = "PASS"
			i.Code = "OK"
		}
	}
	if !allowedSmokeDoctor(saved.Doctor, saved.Inventory, profile) {
		t.Fatal("GPU-only repaired r9 still refused")
	}
}
