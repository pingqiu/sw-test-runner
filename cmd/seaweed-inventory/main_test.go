package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestJSONRefusalContract(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		args []string
		code string
	}{
		{"usage", []string{"--json"}, "USAGE"},
		{"unsupported", []string{"--json", "run", "never-read"}, "USAGE"},
		{"input", []string{"--json", "validate", filepath.Join(dir, "missing")}, "INPUT_UNREADABLE"},
		{"invalid", []string{"--json", "validate", bad}, "INVENTORY_INVALID"},
		{"join", []string{"--json", "join", "../../examples/inventory/http.requested.json", bad}, "INVENTORY_JOIN_REFUSED"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			exit := execute(c.args, &out)
			var r map[string]any
			if exit != 2 || json.Unmarshal(out.Bytes(), &r) != nil || r["code"] != c.code || r["remedy"] == "" || r["execution_authorized"] != false {
				t.Fatalf("exit=%d JSON=%s", exit, out.String())
			}
		})
	}
}
