package main

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestCLIRefusesUsageAndMissingConfigWithJSON(t *testing.T) {
	for _, args := range [][]string{nil, {"run"}, {"run", "--config", "/definitely-absent"}, {"run", "--config", "/absent", "--surprise"}, {"pins", "--source", "/absent", "--variables", "/absent"}} {
		var out bytes.Buffer
		if run(args, &out) == 0 {
			t.Fatalf("accepted %v", args)
		}
		var r map[string]any
		if json.Unmarshal(out.Bytes(), &r) != nil || r["code"] == "" || r["remedy"] == "" {
			t.Fatalf("not AI-operable: %s", out.Bytes())
		}
	}
}
