package inventory_test

import (
	"github.com/pingqiu/sw-test-runner/inventory"
	"testing"
)

func TestExecutionEnvelopeRequired(t *testing.T) {
	d := fixture(t)
	delete(d, "deadlines")
	if _, err := inventory.Decode(bytesOf(t, d)); err == nil {
		t.Fatal("missing original deadlines accepted")
	}
}

func TestDeadlinesAndServicesCannotDrift(t *testing.T) {
	for _, mode := range []string{"missing service", "port collision", "out of range", "deadline alias", "reserve missing", "reserve over hard", "service changed at Join", "deadline changed at Join"} {
		t.Run(mode, func(t *testing.T) {
			req, d := pair(t)
			switch mode {
			case "missing service":
				d["hosts"].([]any)[0].(map[string]any)["services"] = []any{}
			case "port collision":
				firstHost(d)["services"] = append(firstHost(d)["services"].([]any), map[string]any{"name": "s3_http", "port": float64(9333)})
			case "out of range":
				firstHost(d)["services"].([]any)[0].(map[string]any)["port"] = float64(65536)
			case "deadline alias":
				d["deadlines"].(map[string]any)["Work"] = "2030-01-01T00:08:00Z"
			case "reserve missing":
				delete(d["deadlines"].(map[string]any), "cleanup_reserve_ms")
			case "reserve over hard":
				d["deadlines"].(map[string]any)["cleanup_reserve_ms"] = float64(60001)
			case "service changed at Join":
				firstHost(d)["services"].([]any)[0].(map[string]any)["port"] = float64(9444)
			case "deadline changed at Join":
				d["deadlines"].(map[string]any)["work"] = "2030-01-01T00:08:00Z"
			}
			if _, err := inventory.Join(req, bytesOf(t, d)); err == nil {
				t.Fatal("execution envelope drift accepted")
			}
		})
	}
}
