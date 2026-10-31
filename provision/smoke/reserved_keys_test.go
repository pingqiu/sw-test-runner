package smoke

import (
	"context"
	"encoding/json"
	"os"
	"testing"
)

func TestGeneratedHostKeyInputsAreReserved(t *testing.T) {
	for _, key := range []string{"host_keys", "node_host_keys", "run_id"} {
		for _, value := range []any{nil, map[string]any{}, map[string]any{"meta": "caller"}} {
			t.Run(key, func(t *testing.T) {
				c := clusterFixture(t)
				b, err := os.ReadFile(c.Variables)
				if err != nil {
					t.Fatal(err)
				}
				var vars map[string]any
				if err = json.Unmarshal(b, &vars); err != nil {
					t.Fatal(err)
				}
				vars[key] = value
				b, err = json.Marshal(vars)
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(c.Variables, b, 0600); err != nil {
					t.Fatal(err)
				}
				c.VariablesSHA256 = digest(b)
				f := &fake{}
				r := controller(f).Execute(context.Background(), "run", c)
				if r.Code != "INPUT_REFUSED" || len(f.events) != 0 || len(r.Effects) != 0 {
					t.Fatalf("reserved input reached effects: %+v %v", r, f.events)
				}
				if _, err = os.Stat(c.Output); !os.IsNotExist(err) {
					t.Fatalf("output created before admission: %v", err)
				}
			})
		}
	}
}
