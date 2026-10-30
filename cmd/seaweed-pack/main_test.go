package main

import (
	"encoding/json"
	"github.com/pingqiu/sw-test-runner/packs/seaweed"
	"io"
	"os"
	"testing"
)

func TestActualPlanCommandNeverExecutesAndRefusesBadInput(t *testing.T) {
	for _, phase := range []string{"seaweed_preflight", "seaweed_deploy", "seaweed_workload", "seaweed_collect", "seaweed_cleanup", "raw_ssh"} {
		t.Run(phase, func(t *testing.T) {
			old := os.Stdout
			r, w, _ := os.Pipe()
			os.Stdout = w
			rc := run([]string{"--json", "plan", phase, "../../examples/seaweed-onprem-plan.json"})
			w.Close()
			os.Stdout = old
			raw, _ := io.ReadAll(r)
			r.Close()
			var v seaweed.Result
			if json.Unmarshal(raw, &v) != nil || v.ExecutionAuthorized || len(v.Effects) != 0 || v.Remedy == "" {
				t.Fatalf("bad contract %s", raw)
			}
			if phase == "raw_ssh" {
				if rc == 0 || v.Code != "ACTION_UNSUPPORTED" {
					t.Fatalf("unsupported accepted %s", raw)
				}
			} else if rc != 0 || v.Code != "S0B_REQUIRED" {
				t.Fatalf("bad plan rc=%d %s", rc, raw)
			}
		})
	}
}
