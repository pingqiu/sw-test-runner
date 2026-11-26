package v3block

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func pool(n int) []string {
	var p []string
	for i := 1; i <= n; i++ {
		p = append(p, fmt.Sprintf("g%d", i))
	}
	return p
}

// run drives a generator against a product that answers ok to every
// ordinary operation and refused to every negative one, and returns the
// proposals.
func run(t *testing.T, cfg GenConfig) ([]GenOp, *Generator) {
	t.Helper()
	g, err := NewGenerator(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var ops []GenOp
	for {
		o, ok := g.Next()
		if !ok {
			break
		}
		ops = append(ops, o)
		if o.Negative != "" {
			g.Learn(o, "refused")
		} else {
			g.Learn(o, "ok")
		}
	}
	return ops, g
}

func TestGeneratorSameSeedSameRunAnotherSeedAnotherRun(t *testing.T) {
	cfg := GenConfig{Seed: 42, Ops: 500, Pool: pool(12), Blocks: 16}
	a, _ := run(t, cfg)
	b, _ := run(t, cfg)
	if !reflect.DeepEqual(a, b) {
		t.Fatal("the same seed proposed two different runs")
	}
	cfg.Seed = 43
	c, _ := run(t, cfg)
	if reflect.DeepEqual(a, c) {
		t.Fatal("another seed proposed the same run")
	}
}

// oracle is an independent statement of the operator pages' rules, kept
// apart from the generator's own shadow: it says whether an operation is
// allowed in the current state.
type oracle struct {
	state   map[string]string
	snaps   map[string]map[string]bool
	targets map[string]map[string]bool // vol -> snapshot names with a live restore target
	target  map[string]string          // restore target -> its source
	fromSn  map[string]string          // restore target -> the snapshot it was restored from
	host    map[string]int             // the holder host each volume serves (or moved) on
}

func newOracle() *oracle {
	return &oracle{state: map[string]string{}, snaps: map[string]map[string]bool{},
		targets: map[string]map[string]bool{}, target: map[string]string{}, fromSn: map[string]string{}, host: map[string]int{}}
}

func (o *oracle) allowed(op GenOp) bool {
	v := op.Volume
	st := o.state[v]
	switch op.Action {
	case "v3_op_create":
		return st == ""
	case "v3_op_attach":
		return (st == "created" || st == "stopped" || st == "moved") && op.Params["host"] == fmt.Sprint(o.host[v])
	case "v3_op_write", "v3_op_read", "v3_op_stop":
		return st == "serving"
	case "v3_op_snapshot":
		return st == "serving" && len(o.snaps[v]) < MaxSnapshots
	case "v3_op_snapshot_delete":
		return st == "serving" && o.snaps[v][op.Params["name"]] && !o.targets[v][op.Params["name"]]
	case "v3_op_restore":
		return st == "serving" && o.snaps[v][op.Params["snapshot"]] && o.state[op.Params["target_volume"]] == ""
	case "v3_op_move":
		return st == "serving" && o.target[v] == "" && len(o.targets[v]) == 0
	case "v3_op_delete":
		return (st == "created" || st == "stopped") && len(o.targets[v]) == 0
	}
	return false
}

func (o *oracle) apply(op GenOp) {
	v := op.Volume
	if o.snaps[v] == nil {
		o.snaps[v], o.targets[v] = map[string]bool{}, map[string]bool{}
	}
	switch op.Action {
	case "v3_op_create":
		o.state[v] = "created"
	case "v3_op_attach":
		o.state[v] = "serving"
	case "v3_op_snapshot":
		o.snaps[v][op.Params["name"]] = true
	case "v3_op_snapshot_delete":
		delete(o.snaps[v], op.Params["name"])
	case "v3_op_restore":
		t := op.Params["target_volume"]
		o.state[t], o.target[t], o.fromSn[t], o.host[t] = "created", v, op.Params["snapshot"], o.host[v]
		o.targets[v][op.Params["snapshot"]] = true
	case "v3_op_stop":
		o.state[v] = "stopped"
	case "v3_op_move":
		o.state[v], o.host[v] = "moved", 1-o.host[v]
	case "v3_op_delete":
		o.state[v] = "deleted"
		if src := o.target[v]; src != "" {
			delete(o.targets[src], o.fromSn[v]) // only this target's snapshot is freed
		}
	}
}

func TestGeneratorOrdinaryOpsMeetTheirPreconditionNegativesBreakOne(t *testing.T) {
	for seed := uint64(1); seed <= 20; seed++ {
		ops, _ := run(t, GenConfig{Seed: seed, Ops: 400, Pool: pool(10), Blocks: 8})
		o := newOracle()
		for _, op := range ops {
			ok := o.allowed(op)
			if op.Negative == "" && !ok {
				t.Fatalf("seed %d op %d: %s %v proposed against its precondition", seed, op.No, op.Action, op.Params)
			}
			if op.Negative != "" {
				if ok || op.Params["expect"] != "refused" {
					t.Fatalf("seed %d op %d: negative %q is allowed or not declared refused: %s %v", seed, op.No, op.Negative, op.Action, op.Params)
				}
				continue
			}
			o.apply(op)
		}
	}
}

func TestGeneratorNegativeShareAndEveryLimitProbed(t *testing.T) {
	ops, _ := run(t, GenConfig{Seed: 7, Ops: 4000, Pool: pool(120), Blocks: 8})
	neg := 0
	limits := map[string]int{}
	for _, o := range ops {
		if o.Negative != "" {
			neg++
			limits[o.Negative]++
		}
	}
	share := float64(neg) / float64(len(ops))
	if share < 0.05 || share > 0.15 {
		t.Fatalf("negative share %.3f, want about 0.1", share)
	}
	for _, l := range []string{"create a deleted name", "delete while attached", "delete a snapshot that has a restore target"} {
		if limits[l] == 0 {
			t.Errorf("limit %q never probed in 4000 operations: %v", l, limits)
		}
	}
}

func TestGeneratorEndsOnTheBudgetOrAnExhaustedPool(t *testing.T) {
	ops, g := run(t, GenConfig{Seed: 3, Ops: 50, Pool: pool(40), Blocks: 4})
	if len(ops) != 50 || g.Ended() != "ops" {
		t.Fatalf("%d ops, ended %q", len(ops), g.Ended())
	}
	// A tiny pool, weights that only create and delete: every name is used once
	// and burnt, then nothing is possible.
	ops, g = run(t, GenConfig{Seed: 3, Ops: 1000, Pool: pool(3), Blocks: 4, Negative: 0.01,
		Weights: map[string]int{"v3_op_create": 1, "v3_op_delete": 1}})
	if g.Ended() != "pool exhausted" || len(ops) >= 1000 {
		t.Fatalf("%d ops, ended %q", len(ops), g.Ended())
	}
	creates := map[string]int{}
	for _, o := range ops {
		if o.Action == "v3_op_create" && o.Negative == "" {
			creates[o.Volume]++
		}
	}
	for n, c := range creates {
		if c != 1 {
			t.Fatalf("name %s created %d times", n, c)
		}
	}
}

func TestGeneratorUnknownTakesTheVolumeOut(t *testing.T) {
	g, _ := NewGenerator(GenConfig{Seed: 1, Ops: 200, Pool: pool(1), Blocks: 4, Negative: 0.01})
	o, _ := g.Next()
	if o.Action != "v3_op_create" {
		t.Fatalf("first op %s", o.Action)
	}
	g.Learn(o, "unknown")
	for {
		o, ok := g.Next()
		if !ok {
			break
		}
		if o.Volume == "g1" && o.Negative == "" {
			t.Fatalf("an operation on a volume whose state is unknown: %s", o.Action)
		}
	}
}

func TestGeneratorWeightsByTimeAndReportsCoverage(t *testing.T) {
	ops, g := run(t, GenConfig{Seed: 11, Ops: 10000, Pool: pool(400), Blocks: 16})
	count := map[string]int{}
	for _, o := range ops {
		if o.Negative == "" {
			count[o.Action]++
		}
	}
	for a := range DefaultWeights {
		if count[a] == 0 {
			t.Errorf("%s never proposed in 10000 operations", a)
		}
	}
	// Time weighting: a delete costs 65 s, a write 1 s; deletes must not be
	// starved (they hold about their weight share of the time).
	if count["v3_op_delete"]*65 < count["v3_op_write"]/10 {
		t.Errorf("deletes starved: %d deletes, %d writes", count["v3_op_delete"], count["v3_op_write"])
	}
	cov := g.Coverage()
	if cov["proposed"] != 10000 || cov["estimated_ops_per_hour"].(int) <= 0 {
		t.Fatalf("coverage %v", cov)
	}
	h := g.Header()
	if h["generator"] != GenVersion || h["seed"] != "11" || len(h["pool"].([]string)) != 400 {
		t.Fatalf("header %v", h)
	}
}

func TestGeneratorRefusesABadConfig(t *testing.T) {
	for name, cfg := range map[string]GenConfig{
		"no ops":            {Pool: pool(2), Blocks: 4},
		"no pool":           {Ops: 5, Blocks: 4},
		"a repeated name":   {Ops: 5, Pool: []string{"a", "a"}, Blocks: 4},
		"a name with space": {Ops: 5, Pool: []string{"a b"}, Blocks: 4},
		"unknown weight":    {Ops: 5, Pool: pool(2), Blocks: 4, Weights: map[string]int{"v3_op_flush": 1}},
		"negative share":    {Ops: 5, Pool: pool(2), Blocks: 4, Negative: 0.9},
	} {
		if _, err := NewGenerator(cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	_ = strings.Join
}
