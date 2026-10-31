package seaweed

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestReviewerSeedRequiresCounterSnapshots(t *testing.T) {
	original := pythonReadOutput(t, "seed", "dc")
	for _, mode := range []string{"missing-snapshots", "copied-path", "fallback", "write-count", "read-count", "read-error", "regressed-counter"} {
		t.Run(mode, func(t *testing.T) {
			lines := bytes.Split(bytes.TrimSpace(original), []byte{'\n'})
			var row map[string]any
			if err := json.Unmarshal(lines[len(lines)-1], &row); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "missing-snapshots":
				delete(row, "before")
				delete(row, "after")
			case "copied-path":
				s := row["after"].(map[string]any)["loader"].(map[string]any)
				s["copied_reads"] = 2
				s["copied_bytes"] = 128
				s["direct_dc_reads"] = 0
				s["direct_dc_bytes"] = 0
			case "write-count":
				row["after"].(map[string]any)["loader"].(map[string]any)["write_objects"] = 1
			case "read-count":
				row["after"].(map[string]any)["loader"].(map[string]any)["read_objects"] = 3
			case "read-error":
				row["after"].(map[string]any)["read_errors"] = 1
			case "regressed-counter":
				row["before"].(map[string]any)["loader"].(map[string]any)["write_bytes"] = 99999
			case "fallback":
				row["after"].(map[string]any)["loader"].(map[string]any)["http_fallback_allowed"] = true
			}
			lines[len(lines)-1], _ = json.Marshal(row)
			if _, err := CheckReadTaskOutput(captured(bytes.Join(lines, []byte{'\n'})), readConfig(), "seed", "dc"); err == nil {
				t.Fatal("seed with missing/contradictory counters accepted")
			}
		})
	}
}

func TestReviewerFailureRequiresExactOperationKeyJoin(t *testing.T) {
	original := pythonReadOutput(t, "recovery", "dc")
	for _, mode := range []string{"missing-operation-id", "null-operation-id", "wrong-known-key", "positive"} {
		t.Run(mode, func(t *testing.T) {
			var row map[string]any
			if err := json.Unmarshal(original, &row); err != nil {
				t.Fatal(err)
			}
			start := row["started"].(float64)
			row["stage"] = "load"
			row["elapsed"] = 120.
			row["ended"] = start + 120
			row["attempts"] = 3
			f := map[string]any{"action": "get", "key": "m01-0", "operation_id": 0, "target": readConfig().RDMAAddress, "transport": "dc", "started": start + 40, "ended": start + 41, "ok": false, "results": nil, "error": "completed error"}
			switch mode {
			case "missing-operation-id":
				delete(f, "operation_id")
			case "null-operation-id":
				f["operation_id"] = nil
			case "wrong-known-key":
				f["key"] = "m01-1"
			}
			row["faults"] = []any{f}
			row["after"].(map[string]any)["read_errors"] = 1
			b, _ := json.Marshal(row)
			_, err := CheckReadTaskOutput(captured(b), readConfig(), "load", "dc")
			if mode == "positive" && err != nil {
				t.Fatal(err)
			}
			if mode != "positive" && err == nil {
				t.Fatal("unbound operation/key accepted")
			}
		})
	}
}
