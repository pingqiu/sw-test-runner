package smoke

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// setVar rewrites one top-level variable of the fixture and re-pins the file.
func setVar(t *testing.T, c *Config, key string, value any) {
	t.Helper()
	b, e := os.ReadFile(c.Variables)
	var v map[string]any
	if e == nil {
		e = json.Unmarshal(b, &v)
	}
	if e != nil {
		t.Fatal(e)
	}
	v[key] = value
	b, _ = json.Marshal(v)
	if e = os.WriteFile(c.Variables, b, 0600); e != nil {
		t.Fatal(e)
	}
	c.VariablesSHA256 = digest(b)
}

// The VM size allowlist: D2s_v5 (one node or the cluster) and one L16s_v3 node
// are admitted; L16s_v3 for the ten-node cluster and any other size or region are
// refused before any cloud call.
func TestVMSizeAllowlist(t *testing.T) {
	for _, tc := range []struct {
		name     string
		size     string
		location string
		cluster  bool
		ok       bool
	}{
		{"one D2s_v5", "Standard_D2s_v5", "westus2", false, true},
		{"cluster D2s_v5", "Standard_D2s_v5", "westus2", true, true},
		{"one L16s_v3", "Standard_L16s_v3", "westus2", false, true},
		{"cluster L16s_v3", "Standard_L16s_v3", "westus2", true, false},
		{"other size", "Standard_D4s_v5", "westus2", false, false},
		{"lower-case size", "standard_l16s_v3", "westus2", false, false},
		{"other region", "Standard_L16s_v3", "eastus", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := fixture(t)
			setVar(t, &c, "vm_size", tc.size)
			setVar(t, &c, "location", tc.location)
			if tc.cluster {
				setVar(t, &c, "cluster", true)
			}
			calls := 0
			r := (Controller{Command: func(context.Context, string, string, ...string) ([]byte, error) {
				calls++
				return nil, nil
			}}).Execute(context.Background(), "plan", c)
			if tc.ok && r.Code != "PLAN_ONLY" {
				t.Fatalf("code %s (%s), want PLAN_ONLY", r.Code, r.Error)
			}
			if !tc.ok && (r.Code != "INPUT_REFUSED" || !strings.Contains(r.Error, "Standard_L16s_v3") || calls != 0) {
				t.Fatalf("code %s error %q calls %d, want INPUT_REFUSED naming the allowlist with 0 calls", r.Code, r.Error, calls)
			}
		})
	}
}
