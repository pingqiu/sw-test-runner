package smoke

import (
	"context"
	"slices"
	"testing"
)

func TestTerraformMutationsLimitParallelism(t *testing.T) {
	for _, mode := range []string{"run", "recovery"} {
		t.Run(mode, func(t *testing.T) {
			c := clusterFixture(t)
			f := &fake{}
			ctl := controller(f)
			seen := map[string]int{}
			ctl.Command = func(ctx context.Context, dir, tool string, args ...string) ([]byte, error) {
				if tool == "terraform" && (args[0] == "apply" || args[0] == "destroy") {
					seen[args[0]]++
					if !slices.Contains(args, "-parallelism=3") {
						t.Errorf("unbounded ARM mutation concurrency: %q", args)
					}
				}
				return f.command(ctx, dir, tool, args...)
			}
			r := ctl.Execute(context.Background(), "run", c)
			if r.Code != "COMPLETE" {
				t.Fatal(r)
			}
			if mode == "recovery" {
				r = ctl.Execute(context.Background(), "destroy", c)
				if r.Code != "COMPLETE" {
					t.Fatal(r)
				}
			}
			want := 1
			if mode == "recovery" {
				want = 2
			}
			if seen["apply"] != 1 || seen["destroy"] != want {
				t.Fatal(seen)
			}
		})
	}
}
