//go:build linux

package seaweed

import (
	"encoding/json"
	"testing"
)

func shiftRestartTrace(row map[string]any, shift float64) {
	for _, k := range []string{"started", "ended", "first_exact", "last_exact"} {
		row[k] = row[k].(float64) + shift
	}
	for _, field := range []string{"outcomes", "faults"} {
		for _, value := range row[field].([]any) {
			r := value.(map[string]any)
			r["started"], r["ended"] = r["started"].(float64)+shift, r["ended"].(float64)+shift
		}
	}
}

func TestDefaultRestartRolePreservesPlanBytes(t *testing.T) {
	x := ReadRestart{Episode: "volume-once", DC: "read-dc", RecoverDC: "recover-dc", RecoverHTTP: "control-http"}
	b, err := json.Marshal(x)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"episode":"volume-once","dc":"read-dc","recover_dc":"recover-dc","recover_http":"control-http"}` || x.targetRole() != "volume" {
		t.Fatalf("default volume plan changed: %s", b)
	}
}
