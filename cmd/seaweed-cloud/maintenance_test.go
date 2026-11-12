package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestMaintenanceECUsesArtifactAdmissionInsteadOfStaticLicenseGate(t *testing.T) {
	for _, task := range []string{"ec_repair", "ec_vacuum"} {
		t.Run(task, func(t *testing.T) {
			dir := t.TempDir()
			raw, err := os.ReadFile("../../inventory/testdata/http.requested.json")
			if err != nil {
				t.Fatal(err)
			}
			var doc map[string]any
			if err = json.Unmarshal(raw, &doc); err != nil {
				t.Fatal(err)
			}
			host := doc["hosts"].([]any)[0].(map[string]any)
			cloneRaw, _ := json.Marshal(host)
			var second map[string]any
			json.Unmarshal(cloneRaw, &second)
			second["id"] = "node2"
			second["management"].(map[string]any)["address"] = "192.0.2.11"
			second["data"].(map[string]any)["address"] = "192.0.2.11"
			doc["hosts"] = []any{host, second}
			raw, _ = json.Marshal(doc)
			inv, cfg := filepath.Join(dir, "inventory.json"), filepath.Join(dir, "config.json")
			os.WriteFile(inv, raw, 0600)
			raw, _ = json.Marshal(map[string]any{"maintenance_task": task, "cluster_chaos_cases": []string{"C1-single-kill"}, "replication": "001"})
			os.WriteFile(cfg, raw, 0600)
			code, out := directCall(t, "--inventory", inv, "--config", cfg, "--out", filepath.Join(dir, "out"), "--timeout", "25m")
			// A no-artifact request must fail ordinary artifact admission. It
			// cannot assert that a target's real default trial is unavailable.
			if code == 0 || code == 78 || out.Code == "ENVIRONMENT" || out.Rows != "" {
				t.Fatalf("EC bypassed artifact admission: exit=%d %+v", code, out)
			}
		})
	}
}
