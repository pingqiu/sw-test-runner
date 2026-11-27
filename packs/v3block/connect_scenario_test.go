package v3block

import (
	"slices"
	"strings"
	"testing"

	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/actions"
)

// The connect-settings scenario parses, names only registered actions, keeps
// M1's setup and teardown, and runs its two cases with the typed fault
// operations on the one volume of its pool.
func TestConnectSettingsScenario(t *testing.T) {
	s, err := tr.ParseFile("../../scenarios/v3block/connect-settings.yaml")
	if err != nil {
		t.Fatal(err)
	}
	r := tr.NewRegistry()
	actions.RegisterSystemActions(r)
	RegisterPack(r)
	var phases []string
	for _, p := range s.Phases {
		phases = append(phases, p.Name)
		for _, a := range p.Actions {
			if !r.Has(a.Action) {
				t.Errorf("phase %s: action %s is not registered", p.Name, a.Action)
			}
			if a.Action == "v3_op_kill" || a.Action == "v3_op_restart" {
				if a.Params["process"] != "blockvolume:0" {
					t.Errorf("phase %s: %s of %q, want the pool's one holder", p.Name, a.Action, a.Params["process"])
				}
			}
		}
	}
	want := []string{"build", "store", "master", "volume", "positive", "negative", "teardown", "judge"}
	if !slices.Equal(phases, want) {
		t.Fatalf("phases %v, want %v", phases, want)
	}
	if strings.Fields(s.Env["gen_pool"])[0] != "c1" || len(strings.Fields(s.Env["gen_pool"])) != 1 {
		t.Errorf("pool %q: one volume, c1", s.Env["gen_pool"])
	}
	if _, ok := s.Env["guest_connect"]; !ok {
		t.Error("no guest_connect env: the options under test must be declared")
	}
	restarts := 0
	for _, p := range s.Phases {
		for _, a := range p.Actions {
			if a.Action == "v3_op_restart" {
				restarts++
				if p.Name != "positive" {
					t.Errorf("a restart in phase %s: only the positive case restarts the holder", p.Name)
				}
			}
		}
	}
	if restarts != 1 {
		t.Errorf("%d restarts, want 1", restarts)
	}
}
