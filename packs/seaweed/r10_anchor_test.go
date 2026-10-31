package seaweed

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestR10ActualTraceWithPreKillAnchor(t *testing.T) {
	raw, err := os.ReadFile("testdata/r10-dc-load.json")
	if err != nil {
		t.Fatal(err)
	}
	c := SmokeConfig{RunID: "s0b-m01m02-20261031-r10-read-restart", Client: "m01", Transport: "dc", Keys: 4, Length: 1048576, Root: "/testops/s0b-m01m02-20261031-r10-read-restart/m01", RDMAAddress: "10.0.2.3:20980"}
	// R10 did NOT record P. Replay-only fixture: recorded load start + the
	// independently fixed 30s schedule; never a fitted/claimed lab timestamp.
	const fixtureP = 405656.462523641
	for _, mode := range []string{"real-trace", "old-ready-anchor", "miss-before-P", "close-after-P-plus-60"} {
		t.Run(mode, func(t *testing.T) {
			var row map[string]any
			if err := json.Unmarshal(raw, &row); err != nil {
				t.Fatal(err)
			}
			p := fixtureP
			switch mode {
			case "old-ready-anchor":
				p = 405658.984701257
			case "miss-before-P":
				p = 405657.071772398 // first miss starts 100us earlier
			case "close-after-P-plus-60":
				outcomes := row["outcomes"].([]any)
				prior := fixtureP + 60.001
				outcomes[105].(map[string]any)["ended"] = prior
				for _, value := range outcomes[106:] {
					r := value.(map[string]any)
					if r["started"].(float64) < prior {
						r["started"], r["ended"] = prior, prior+0.000001
					}
					prior = r["ended"].(float64)
				}
			}
			body, _ := json.Marshal(row)
			_, err := CheckReadTaskOutput(captured(body), c, "load", "dc", ReadWindow{p})
			if mode == "real-trace" && err != nil {
				t.Fatal(err)
			}
			if mode != "real-trace" && (err == nil || !strings.Contains(err.Error(), "outage outside pre-Kill 60s window")) {
				t.Fatalf("wrong refusal: %v", err)
			}
		})
	}
}
