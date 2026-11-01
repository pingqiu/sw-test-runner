package smoke

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"testing/synctest"
)

// Run 7: the sweep deleted vol2's OS disk and `wait --deleted` returned OK,
// yet the final RG and subscription lists right after still showed it; a
// recovery list seconds later was empty. Only IDs the sweep itself confirmed
// gone may be re-listed, at most 3 times, 10 s apart.
func TestFinalListLagOnlyForSweptIDs(t *testing.T) {
	raw, err := os.ReadFile("testdata/run7-final-list-lag.json") // run7's 027 output, IDs normalized
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err = json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	disk := rows[0]["id"].(string)
	foreign := strings.ReplaceAll(string(raw), "output-vol2-os", "output-vol3-os") // never swept
	for _, tc := range []struct {
		name   string
		lagged int    // final-list rounds that still show the disk
		final  string // what a lagging final list returns
		want   string
		rounds int
	}{
		{"lag-clears", 1, string(raw), "COMPLETE", 2},
		{"lag-persists", 99, string(raw), "CLEANUP_INCOMPLETE", 4},
		{"foreign-residue", 99, foreign, "CLEANUP_INCOMPLETE", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c := clusterFixture(t)
				f := &fake{}
				ctl := controller(f)
				destroyed, swept, rounds := false, false, 0
				ctl.Command = func(ctx context.Context, dir, tool string, args ...string) ([]byte, error) {
					if tool == "terraform" && args[0] == "destroy" {
						destroyed = true
					}
					if tool == "az" && args[0] == "resource" && destroyed {
						rg := strings.Contains(strings.Join(args, " "), "--resource-group")
						switch args[1] {
						case "list":
							if rg && !swept {
								swept = true
								return raw, nil
							}
							if rg {
								rounds++
							}
							if rounds <= tc.lagged {
								return []byte(tc.final), nil
							}
							return []byte(`[]`), nil
						case "show":
							b, _ := json.Marshal(rows[0])
							return b, nil
						case "delete", "wait":
							if !strings.Contains(strings.Join(args, " "), disk) {
								t.Fatal("swept a foreign ID", args)
							}
							return nil, nil
						}
					}
					return f.command(ctx, dir, tool, args...)
				}
				r := ctl.Execute(context.Background(), "run", c)
				if r.Code != tc.want || rounds != tc.rounds {
					t.Fatalf("rounds=%d result=%+v", rounds, r)
				}
			})
		})
	}
}
