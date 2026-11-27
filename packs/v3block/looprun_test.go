package v3block

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/actions"
	"github.com/pingqiu/sw-test-runner/randomrun"
)

const soakScenario = "../../scenarios/v3block/soak.yaml"

// The block's loops are data: every action a registered action and one the
// loop runs through.
func TestBlockLoopsAreRegisteredActions(t *testing.T) {
	f, err := blockLoops()
	if err != nil {
		t.Fatal(err)
	}
	l, ok := f.Loops["soak"]
	if !ok {
		t.Fatal("no loop soak")
	}
	r := tr.NewRegistry()
	actions.RegisterSystemActions(r)
	RegisterPack(r)
	for _, list := range [][]randomrun.LoopStep{l.Setup, l.Cycle, {*l.Sweep}, l.Teardown} {
		for _, s := range list {
			if !r.Has(s.Action) {
				t.Errorf("%s is not registered", s.Action)
			}
		}
	}
	// Red control: the same file naming an action the pack does not have.
	bad := strings.Replace(string(loopsJSON), `"v3_op_stop"`, `"v3_op_halt"`, 1)
	if _, err := randomrun.ParseLoops([]byte(bad), func(a string) bool { _, ok := genHandlers[a]; return ok }); err == nil {
		t.Fatal("an unregistered action loaded")
	}
}

func loopAct(iterations string) tr.Action {
	return tr.Action{Action: "v3_loop_run", Node: "host", Params: map[string]string{
		"loop": "soak", "seed": "3", "iterations": iterations, "blocks": "64", "pool": "v01 v02 v03 v04"}}
}

// The soak loop over the block handlers (faked): the declared order and
// counts, writes split one lba per call, and a history CheckLoop accepts.
func TestLoopRunSoakOverTheBlockHandlers(t *testing.T) {
	f := &fakeProduct{}
	f.install(t)
	actx, p := genRunCtx(t)
	out, err := v3LoopRun(context.Background(), actx, loopAct("40"))
	if err != nil {
		t.Fatal(err)
	}
	_ = actx.History.Close()
	if out["value"] != "iterations" || out["iterations"] != "40" {
		t.Fatalf("out %v", out)
	}
	calls := map[string]int{}
	var snapDeletes []string
	for _, c := range f.calls {
		calls[c.Action]++
		if c.Action == "v3_op_snapshot_delete" {
			snapDeletes = append(snapDeletes, c.Params["name"])
		}
	}
	// 4 volumes x 40 iterations: a write of 4 lbas (4 calls) and a read; a
	// snapshot at 10, 20, 30, 40, each observed first (the "before" the
	// driver places an unknown snapshot by); a delete at 40 of the one taken
	// at 10. The sweep: one read per volume, then an observation of each.
	want := map[string]int{"v3_op_create": 4, "v3_op_attach": 4, "v3_op_write": 4 * 40 * 4, "v3_op_read": 4*40 + 4,
		"v3_op_snapshot": 16, "v3_op_snapshot_delete": 4, "v3_op_observe": 4 + 16, "v3_op_stop": 4}
	if fmt.Sprint(calls) != fmt.Sprint(want) {
		t.Fatalf("calls %v\nwant  %v", calls, want)
	}
	if !slices.Equal(snapDeletes, []string{"v01-s10", "v02-s10", "v03-s10", "v04-s10"}) {
		t.Fatalf("snapshot deletes %v", snapDeletes)
	}
	m := &blockModel{}
	if err := randomrun.CheckLoop(p, m.Observe, m.ValidProcess); err != nil {
		t.Fatal(err)
	}
	// Replay through the same handlers matches.
	actx2, _ := genRunCtx(t)
	if _, err := v3LoopReplay(context.Background(), actx2, tr.Action{Node: "host", Params: map[string]string{"history": p}}); err != nil {
		t.Fatal(err)
	}
	for name, act := range map[string]tr.Action{
		"no loop":      {Params: map[string]string{"loop": "nope", "seed": "1", "iterations": "1", "pool": "a", "blocks": "8"}},
		"no seed":      {Params: map[string]string{"loop": "soak", "iterations": "1", "pool": "a", "blocks": "8"}},
		"few blocks":   {Params: map[string]string{"loop": "soak", "seed": "1", "iterations": "1", "pool": "a", "blocks": "2"}},
		"no iteration": {Params: map[string]string{"loop": "soak", "seed": "1", "pool": "a", "blocks": "8"}},
	} {
		if _, err := v3LoopRun(context.Background(), actx2, act); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The soak scenario is M2's setup and teardown with phase soak running
// v3_loop_run, judged by the checkers that need no model.
func TestSoakScenarioIsM2SetupWithTheLoop(t *testing.T) {
	m2, err := tr.ParseFile(m2Scenario)
	if err != nil {
		t.Fatal(err)
	}
	s, err := tr.ParseFile(soakScenario)
	if err != nil {
		t.Fatal(err)
	}
	r := tr.NewRegistry()
	actions.RegisterSystemActions(r)
	RegisterPack(r)
	var loop tr.Action
	var judge string
	steps := func(sc *tr.Scenario, skip string) (out []string) {
		for _, p := range sc.Phases {
			for _, a := range p.Actions {
				if !r.Has(a.Action) {
					t.Errorf("%s phase %s: action %s is not registered", sc.Name, p.Name, a.Action)
				}
				if a.Action == "v3_loop_run" {
					loop = a
				}
				if strings.HasSuffix(a.Params["env.CHECK"], "/checkers/history_check.py") && sc == s {
					judge = a.Params["cmd"]
				}
				if p.Name != skip {
					out = append(out, p.Name+"/"+a.Action)
				}
			}
		}
		return out
	}
	if a, b := steps(m2, "random"), steps(s, "soak"); !slices.Equal(a, b) {
		t.Fatalf("soak steps %v differ from M2's %v", b, a)
	}
	if loop.Action == "" || len(s.Phases) != len(m2.Phases) {
		t.Fatal("no v3_loop_run phase")
	}
	for _, p := range s.Phases {
		if p.Name == "soak" && (len(p.Actions) != 1 || p.Actions[0].Action != "v3_loop_run") {
			t.Fatalf("phase soak: %+v", p.Actions)
		}
	}
	// One source: the judge reads env checkers, whose default is the set a
	// fault soak exercises (a run with faults off passes C0,C1).
	if !strings.Contains(judge, `--checkers "{{ checkers }}"`) || s.Env["checkers"] != "C0,C1,C6,DUR" {
		t.Fatalf("judge %q, env checkers %q", judge, s.Env["checkers"])
	}
	f, _ := blockLoops()
	if _, ok := f.Loops[s.Env["loop"]]; !ok {
		t.Fatalf("env loop %q is not in loops.json", s.Env["loop"])
	}
	// The defaults make a valid run: the env through the action's parser and
	// the fault schedule over the cycle.
	get := func(k string) string {
		v := loop.Params[k]
		for ek, ev := range s.Env {
			v = strings.ReplaceAll(v, "{{ "+ek+" }}", ev)
		}
		return v
	}
	c, err := randomrun.ParseLoopConfig("v3_loop_run", get)
	if err != nil {
		t.Fatal(err)
	}
	if c.Faults == 0 || c.Iterations == 0 || len(c.Pool) == 0 || c.HealthyWait == "" {
		t.Fatalf("defaults %+v", c)
	}
	n := 0
	for i := range c.FaultProcesses {
		if c.FaultProcesses[i] != "blockmaster" {
			n++
		}
	}
	if n > len(c.Pool) {
		t.Errorf("fault processes name %d holders, the pool has %d", n, len(c.Pool))
	}
	if _, err := NewFaultSchedule(c.Seed, 100, c.FaultProcesses, c.Faults, c.RestartWithin); err != nil {
		t.Errorf("the default schedule is refused: %v", err)
	}
	if !strings.HasPrefix(s.Env["work_prefix"], "/root/soak-run-") {
		t.Errorf("work_prefix %q", s.Env["work_prefix"])
	}
}
