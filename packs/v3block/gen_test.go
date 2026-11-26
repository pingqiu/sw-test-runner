package v3block

import (
	"fmt"
	"reflect"
	"strconv"
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
// apart from the generator's own shadow: it says whether an ordinary
// operation is allowed in the current state, and which documented refusal an
// operation meets (the limit names are spelled out here, not taken from the
// generator).
type oracle struct {
	state   map[string]string
	snaps   map[string]map[string]bool
	targets map[string]map[string]bool // vol -> snapshot names with a live restore target
	target  map[string]string          // restore target (or bound name) -> its source
	fromSn  map[string]string          // restore target (or bound name) -> the snapshot it was restored from
	host    map[string]int             // the holder host each volume serves (or moved) on
	held    map[string]map[int]bool    // hosts that served the volume once (a local root lies there)
	stale   map[string]map[int]bool    // held hosts whose root HEAD has advanced past since
}

func newOracle() *oracle {
	return &oracle{state: map[string]string{}, snaps: map[string]map[string]bool{},
		targets: map[string]map[string]bool{}, target: map[string]string{}, fromSn: map[string]string{}, host: map[string]int{},
		held: map[string]map[int]bool{}, stale: map[string]map[int]bool{}}
}

func (o *oracle) init(v string) {
	if o.snaps[v] == nil {
		o.snaps[v], o.targets[v], o.held[v], o.stale[v] = map[string]bool{}, map[string]bool{}, map[int]bool{}, map[int]bool{}
	}
}

func (o *oracle) allowed(op GenOp) bool {
	v := op.Volume
	o.init(v)
	st := o.state[v]
	switch op.Action {
	case "v3_op_create":
		return st == ""
	case "v3_op_attach":
		// Never after a stop (sealed HEAD, no re-grant), never where an
		// earlier root lies, always on the volume's own host and geometry.
		h, _ := strconv.Atoi(op.Params["host"])
		return (st == "created" || st == "moved") && op.Params["host"] == fmt.Sprint(o.host[v]) && !o.held[v][h] && op.Params["blocks"] == ""
	case "v3_op_write", "v3_op_read", "v3_op_stop":
		return st == "serving"
	case "v3_op_snapshot":
		return st == "serving" && len(o.snaps[v]) < MaxSnapshots
	case "v3_op_snapshot_delete":
		return st == "serving" && o.snaps[v][op.Params["name"]] && !o.targets[v][op.Params["name"]]
	case "v3_op_restore":
		return st == "serving" && o.target[v] == "" && o.snaps[v][op.Params["snapshot"]] && o.state[op.Params["target_volume"]] == ""
	case "v3_op_move":
		return st == "serving" && o.target[v] == "" && len(o.targets[v]) == 0
	case "v3_op_delete":
		return (st == "created" || st == "stopped" || st == "reserved" || st == "moved") && len(o.targets[v]) == 0
	}
	return false
}

// refusal is the documented refusal op meets now, "" for none. Where two
// apply, it is the one the product checks first.
func (o *oracle) refusal(op GenOp) string {
	v := op.Volume
	o.init(v)
	st := o.state[v]
	switch op.Action {
	case "v3_op_create":
		if st == "deleted" {
			return "create a deleted name"
		}
	case "v3_op_delete":
		if st == "serving" {
			return "delete while attached"
		}
		if st == "stopped" && len(o.targets[v]) > 0 {
			return "delete a source whose snapshot has a restore target"
		}
	case "v3_op_snapshot":
		if st == "serving" && len(o.snaps[v]) >= MaxSnapshots {
			return "an eleventh snapshot"
		}
	case "v3_op_snapshot_delete":
		if (st == "serving" || st == "stopped" || st == "reserved") && o.targets[v][op.Params["name"]] {
			return "delete a snapshot that has a restore target"
		}
	case "v3_op_restore":
		s, t := op.Params["snapshot"], op.Params["target_volume"]
		if st != "serving" || !o.snaps[v][s] || t == v {
			return ""
		}
		switch ts := o.state[t]; {
		case ts == "deleted" || ts == "reserved":
			return "restore into a deleted name"
		case ts != "" && ts != "unknown" && !(o.target[t] == v && o.fromSn[t] == s):
			return "restore into an existing name"
		case ts == "" && o.target[v] != "":
			return "restore from a snapshot of a restore target"
		}
	case "v3_op_release":
		t := op.Params["target_volume"]
		if ts := o.state[t]; o.target[t] == v && (ts == "created" || ts == "serving" || ts == "stopped") {
			return "release a live restore target"
		}
	case "v3_op_attach":
		h, _ := strconv.Atoi(op.Params["host"])
		switch {
		case st == "deleted":
			return "attach a deleted volume"
		case st == "moved" && h != o.host[v]:
			return "attach on the old host after a move"
		case st == "moved" && o.held[v][h] && o.stale[v][h]:
			return "attach on a stale local root"
		case st == "created" && !o.held[v][h] && op.Params["blocks"] != "" && op.Params["blocks"] != "4096":
			return "attach with the wrong geometry"
		}
	}
	return ""
}

// apply follows an ordinary operation answered ok.
func (o *oracle) apply(op GenOp) {
	v := op.Volume
	o.init(v)
	advanced := func() {
		for h := range o.held[v] {
			if h != o.host[v] {
				o.stale[v][h] = true
			}
		}
	}
	switch op.Action {
	case "v3_op_create":
		o.state[v] = "created"
	case "v3_op_attach":
		o.state[v] = "serving"
		o.held[v][o.host[v]] = true
	case "v3_op_write":
		advanced()
	case "v3_op_snapshot":
		o.snaps[v][op.Params["name"]] = true
		advanced()
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

// refused follows a negative operation answered refused: nothing changes but
// the documented side effects.
func (o *oracle) refused(op GenOp) {
	switch o.refusal(op) {
	case "delete a source whose snapshot has a restore target":
		o.state[op.Volume] = "reserved" // OG:193-195: the reservation committed first
	case "restore from a snapshot of a restore target":
		t := op.Params["target_volume"]
		o.state[t], o.target[t], o.fromSn[t] = "bound", op.Volume, op.Params["snapshot"] // SD:693
	}
}

// oracleCheck replays a run answered as run() answers it (ordinary ok,
// negative refused) and returns the first proposal the oracle disagrees with.
func oracleCheck(ops []GenOp) error {
	o := newOracle()
	for _, op := range ops {
		lim := o.refusal(op)
		if op.Negative == "" {
			if !o.allowed(op) || lim != "" {
				return fmt.Errorf("op %d: %s %v proposed as ordinary against its precondition (refusal %q)", op.No, op.Action, op.Params, lim)
			}
			o.apply(op)
			continue
		}
		if lim != op.Negative || op.Params["expect"] != "refused" {
			return fmt.Errorf("op %d: negative %q, the oracle expects %q (expect=%s): %s %v", op.No, op.Negative, lim, op.Params["expect"], op.Action, op.Params)
		}
		o.refused(op)
	}
	return nil
}

func TestGeneratorOrdinaryOpsMeetTheirPreconditionNegativesBreakOne(t *testing.T) {
	for seed := uint64(1); seed <= 20; seed++ {
		ops, _ := run(t, GenConfig{Seed: seed, Ops: 400, Pool: pool(10), Blocks: 8})
		if err := oracleCheck(ops); err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
	}
	// A high negative share reaches the states negatives lead to (reserved,
	// bound) more often.
	for seed := uint64(1); seed <= 20; seed++ {
		ops, _ := run(t, GenConfig{Seed: seed, Ops: 1500, Pool: pool(40), Blocks: 8, Negative: 0.4})
		if err := oracleCheck(ops); err != nil {
			t.Fatalf("seed %d (negative 0.4): %v", seed, err)
		}
	}
}

// allLimits are the documented refusals the generator probes.
var allLimits = []string{
	"create a deleted name", "delete while attached", "delete a source whose snapshot has a restore target",
	"delete a snapshot that has a restore target", "an eleventh snapshot", "restore into an existing name",
	"restore into a deleted name", "restore from a snapshot of a restore target", "release a live restore target",
	"attach on the old host after a move", "attach a deleted volume", "attach on a stale local root",
	"attach with the wrong geometry",
}

func TestGeneratorNegativeShareAndEveryLimitProbed(t *testing.T) {
	ops, _ := run(t, GenConfig{Seed: 7, Ops: 4000, Pool: pool(120), Blocks: 8})
	neg := 0
	for _, o := range ops {
		if o.Negative != "" {
			neg++
		}
	}
	share := float64(neg) / float64(len(ops))
	if share < 0.05 || share > 0.15 {
		t.Fatalf("negative share %.3f, want about 0.1", share)
	}
	// Every limit is reached within a few pinned runs.
	limits := map[string]int{}
	for seed := uint64(1); seed <= 4; seed++ {
		ops, _ := run(t, GenConfig{Seed: seed, Ops: 4000, Pool: pool(120), Blocks: 8, Negative: 0.3})
		for _, o := range ops {
			if o.Negative != "" {
				limits[o.Negative]++
			}
		}
	}
	// Ten live snapshots need a run that mostly takes snapshots; a stale root
	// needs moves there and back with writes between.
	for _, w := range []map[string]int{
		{"v3_op_create": 1, "v3_op_attach": 1, "v3_op_snapshot": 40},
		{"v3_op_create": 1, "v3_op_attach": 10, "v3_op_write": 10, "v3_op_move": 100},
	} {
		ops, _ = run(t, GenConfig{Seed: 1, Ops: 400, Pool: pool(4), Blocks: 8, Negative: 0.3, Weights: w})
		if err := oracleCheck(ops); err != nil {
			t.Fatalf("weights %v: %v", w, err)
		}
		for _, o := range ops {
			if o.Negative != "" {
				limits[o.Negative]++
			}
		}
	}
	for _, l := range allLimits {
		if limits[l] == 0 {
			t.Errorf("limit %q never probed: %v", l, limits)
		}
	}
	if len(limits) != len(allLimits) {
		t.Errorf("limits probed %v, the oracle knows %d", limits, len(allLimits))
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

// seq numbers a hand-built run.
func seq(ops ...GenOp) []GenOp {
	for i := range ops {
		ops[i].No = i + 1
	}
	return ops
}

// neg makes o a negative operation probing limit.
func neg(o GenOp, limit string) GenOp {
	o.Params["expect"], o.Negative = "refused", limit
	return o
}

// Red controls for the oracle: each red run is what a generator with one
// seeded defect would propose, and the oracle must reject it; its green twin
// (the correct proposal at the same point) must pass.
func TestGeneratorOracleRedControls(t *testing.T) {
	base := func() []GenOp {
		return []GenOp{op("v3_op_create", "g1"), op("v3_op_attach", "g1", "host", "0"), op("v3_op_snapshot", "g1", "name", "g1-s1")}
	}
	withTarget := func() []GenOp {
		return append(base(), op("v3_op_restore", "g1", "snapshot", "g1-s1", "target_volume", "g2"))
	}
	movedBack := func() []GenOp {
		return []GenOp{op("v3_op_create", "g1"), op("v3_op_attach", "g1", "host", "0"), op("v3_op_move", "g1"),
			op("v3_op_attach", "g1", "host", "1"), op("v3_op_write", "g1", "lbas", "1", "round", "r"), op("v3_op_move", "g1")}
	}
	targetServes := func() []GenOp {
		return append(withTarget(), op("v3_op_attach", "g2", "host", "0"), op("v3_op_snapshot", "g2", "name", "g2-s1"))
	}
	row3 := func() []GenOp {
		return append(withTarget(), op("v3_op_stop", "g1"), neg(op("v3_op_delete", "g1"), "delete a source whose snapshot has a restore target"))
	}
	row8 := func() []GenOp {
		return append(targetServes(), neg(op("v3_op_restore", "g2", "snapshot", "g2-s1", "target_volume", "g3"), "restore from a snapshot of a restore target"))
	}
	for name, c := range map[string]struct{ red, green []GenOp }{
		"attach proposed after a stop": {
			red:   append(base(), op("v3_op_stop", "g1"), op("v3_op_attach", "g1", "host", "0")),
			green: append(base(), op("v3_op_stop", "g1"), op("v3_op_delete", "g1")),
		},
		"restore offered from a restore target": {
			red:   append(targetServes(), op("v3_op_restore", "g2", "snapshot", "g2-s1", "target_volume", "g3")),
			green: row8(),
		},
		"a refused source delete does not reserve": {
			// Not reserved, the volume would still be "stopped with a target"
			// and the same negative proposed again.
			red:   append(row3(), neg(op("v3_op_delete", "g1"), "delete a source whose snapshot has a restore target")),
			green: append(row3(), op("v3_op_delete", "g2"), op("v3_op_delete", "g1")),
		},
		"a reserved volume attached": {
			red:   append(row3(), op("v3_op_attach", "g1", "host", "0")),
			green: append(row3(), neg(op("v3_op_snapshot_delete", "g1", "name", "g1-s1"), "delete a snapshot that has a restore target")),
		},
		"a refused restore does not bind its target": {
			red:   append(row8(), op("v3_op_create", "g3")),
			green: append(row8(), neg(op("v3_op_restore", "g1", "snapshot", "g1-s1", "target_volume", "g3"), "restore into an existing name")),
		},
		"attach on a stale local root as ordinary": {
			red:   append(movedBack(), op("v3_op_attach", "g1", "host", "0")),
			green: append(movedBack(), neg(op("v3_op_attach", "g1", "host", "0"), "attach on a stale local root")),
		},
		"attach on the old host as ordinary": {
			red:   append(base(), op("v3_op_move", "g1"), op("v3_op_attach", "g1", "host", "0")),
			green: append(base(), op("v3_op_move", "g1"), neg(op("v3_op_attach", "g1", "host", "0"), "attach on the old host after a move")),
		},
		"a negative under the wrong limit": {
			red:   append(base(), neg(op("v3_op_delete", "g1"), "create a deleted name")),
			green: append(base(), neg(op("v3_op_delete", "g1"), "delete while attached")),
		},
		"a live target released as ordinary": {
			red:   append(withTarget(), op("v3_op_release", "g1", "target_volume", "g2")),
			green: append(withTarget(), neg(op("v3_op_release", "g1", "target_volume", "g2"), "release a live restore target")),
		},
		"wrong geometry as ordinary": {
			red:   []GenOp{op("v3_op_create", "g1"), op("v3_op_attach", "g1", "host", "0", "blocks", "2048")},
			green: []GenOp{op("v3_op_create", "g1"), neg(op("v3_op_attach", "g1", "host", "0", "blocks", "2048"), "attach with the wrong geometry")},
		},
	} {
		if err := oracleCheck(seq(c.green...)); err != nil {
			t.Errorf("%s: green twin rejected: %v", name, err)
		}
		if err := oracleCheck(seq(c.red...)); err == nil {
			t.Errorf("%s: red control accepted", name)
		}
	}
}

// drive feeds a generator a hand-built run (ordinary ok, negative refused).
func drive(g *Generator, ops ...GenOp) {
	for _, o := range ops {
		if o.Negative != "" {
			g.Learn(o, "refused")
		} else {
			g.Learn(o, "ok")
		}
	}
}

// has says a candidate list holds this operation (kv: params it must carry).
func has(cs []candidate, action, vol, limit string, kv ...string) bool {
	for _, c := range cs {
		if c.op.Action != action || c.op.Volume != vol || c.op.Negative != limit {
			continue
		}
		match := true
		for i := 0; i+1 < len(kv); i += 2 {
			match = match && c.op.Params[kv[i]] == kv[i+1]
		}
		if match {
			return true
		}
	}
	return false
}

// The generator's own shadow: what it proposes in each documented state.
// Each check fails on the seeded defect its message names.
func TestGeneratorShadowFollowsTheDocumentedRefusals(t *testing.T) {
	newG := func() *Generator {
		g, err := NewGenerator(GenConfig{Seed: 1, Ops: 10, Pool: pool(4), Blocks: 4})
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	src := func() []GenOp {
		return []GenOp{op("v3_op_create", "g1"), op("v3_op_attach", "g1", "host", "0"), op("v3_op_snapshot", "g1", "name", "g1-s1"),
			op("v3_op_restore", "g1", "snapshot", "g1-s1", "target_volume", "g2")}
	}

	g := newG()
	drive(g, op("v3_op_create", "g1"), op("v3_op_attach", "g1", "host", "0"), op("v3_op_stop", "g1"))
	if has(g.ordinary(), "v3_op_attach", "g1", "") {
		t.Error("attach proposed after a stop")
	}
	if !has(g.ordinary(), "v3_op_delete", "g1", "") {
		t.Error("a stopped volume without targets is not deleted")
	}

	g = newG()
	drive(g, append(src(), op("v3_op_attach", "g2", "host", "0"), op("v3_op_snapshot", "g2", "name", "g2-s1"))...)
	if has(g.ordinary(), "v3_op_restore", "g2", "") {
		t.Error("a restore offered from a restore target's snapshot")
	}
	if !has(g.negatives(), "v3_op_restore", "g2", limRestoreTarget, "target_volume", "g3") {
		t.Error("no negative restore from a restore target's snapshot")
	}
	drive(g, neg(op("v3_op_restore", "g2", "snapshot", "g2-s1", "target_volume", "g3"), limRestoreTarget))
	if g.vols["g3"].state != stBound || has(g.ordinary(), "v3_op_create", "g3", "") {
		t.Errorf("a refused restore from a target's snapshot left its target %q, not bound", g.vols["g3"].state)
	}
	if !has(g.negatives(), "v3_op_restore", "g1", limRestoreExisting, "target_volume", "g3") {
		t.Error("a bound name is not probed with a restore into an existing name")
	}

	g = newG()
	drive(g, append(src(), op("v3_op_stop", "g1"))...)
	if !has(g.negatives(), "v3_op_delete", "g1", limDeleteSource) || !has(g.negatives(), "v3_op_snapshot_delete", "g1", limDeleteSnapDep) {
		t.Error("no negative source delete or store-direct snapshot delete for a stopped source with a target")
	}
	drive(g, neg(op("v3_op_delete", "g1"), limDeleteSource))
	if g.vols["g1"].state != stReserved {
		t.Errorf("a refused source delete left the source %q, not reserved", g.vols["g1"].state)
	}
	if has(g.ordinary(), "v3_op_attach", "g1", "") || has(g.ordinary(), "v3_op_delete", "g1", "") || has(g.negatives(), "v3_op_delete", "g1", limDeleteSource) {
		t.Error("a reserved volume with a live target offered an attach, a delete or the same negative again")
	}
	if !has(g.negatives(), "v3_op_release", "g1", limReleaseLive, "target_volume", "g2") {
		t.Error("no negative release of the live target")
	}
	drive(g, op("v3_op_delete", "g2"))
	if !has(g.ordinary(), "v3_op_delete", "g1", "") {
		t.Error("a reserved volume is not deleted again once its target is gone")
	}

	g = newG()
	drive(g, op("v3_op_create", "g1"), op("v3_op_attach", "g1", "host", "0"), op("v3_op_move", "g1"))
	if !has(g.ordinary(), "v3_op_attach", "g1", "", "host", "1") || !has(g.negatives(), "v3_op_attach", "g1", limAttachOldHost, "host", "0") {
		t.Error("a moved volume offers no attach on its new host, or no negative on its old one")
	}
	drive(g, op("v3_op_attach", "g1", "host", "1"), op("v3_op_move", "g1"))
	if has(g.ordinary(), "v3_op_attach", "g1", "") || has(g.negatives(), "v3_op_attach", "g1", limAttachStale) {
		t.Error("moved back with no advance: an attach proposed on the earlier root")
	}
	g = newG()
	drive(g, op("v3_op_create", "g1"), op("v3_op_attach", "g1", "host", "0"), op("v3_op_move", "g1"),
		op("v3_op_attach", "g1", "host", "1"), op("v3_op_write", "g1", "lbas", "1", "round", "r"), op("v3_op_move", "g1"))
	if has(g.ordinary(), "v3_op_attach", "g1", "") || !has(g.negatives(), "v3_op_attach", "g1", limAttachStale, "host", "0") {
		t.Error("moved back after a write: an ordinary attach on the stale root, or no negative")
	}
	if !has(g.ordinary(), "v3_op_delete", "g1", "") {
		t.Error("a moved volume is not deleted")
	}

	g = newG()
	drive(g, op("v3_op_create", "g1"), op("v3_op_attach", "g1", "host", "0"))
	g.Learn(neg(op("v3_op_delete", "g1"), limDeleteAttached), "ok")
	if g.vols["g1"].state != stUnknown {
		t.Error("a negative answered ok left the volume in the run")
	}
}
