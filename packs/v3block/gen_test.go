package v3block

import (
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	tr "github.com/pingqiu/sw-test-runner"
)

func pool(n int) []string {
	var p []string
	for i := 1; i <= n; i++ {
		p = append(p, fmt.Sprintf("g%d", i))
	}
	return p
}

// answer is the outcome a product that follows its operator pages gives, as
// the typed actions map it: ok to every ordinary operation; refused to a
// negative one, except the three whose answer the actions cannot prove
// changed nothing, which come back unknown (the source delete refused for a
// snapshot dependent, the restore from a restore target's snapshot, the
// eleventh snapshot).
func answer(o GenOp) string {
	switch o.Negative {
	case "":
		return "ok"
	case "delete a source whose snapshot has a restore target", "restore from a snapshot of a restore target", "an eleventh snapshot":
		return "unknown"
	}
	return "refused"
}

// run drives a generator against that product and returns the proposals.
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
		g.Learn(o, answer(o))
		if o.Action == "v3_op_observe" {
			// The product past the lease horizon: an observation shows it.
			g.m.Observed(leaseRec(o.Volume, "expired"))
		}
	}
	return ops, g
}

// leaseRec is a volume observation whose workspace part shows its lease
// state.
func leaseRec(vol, state string) tr.HistoryRecord {
	return tr.HistoryRecord{Kind: tr.HistoryObserve, Verb: "observe", Volume: vol, Result: map[string]any{
		"workspace": map[string]any{"exit": 0, "text": []any{"volume: " + vol, "lease state: " + state}}}}
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
	state    map[string]string
	snaps    map[string]map[string]bool
	targets  map[string]map[string]bool // vol -> snapshot names with a live restore target
	target   map[string]string          // restore target (or bound name) -> its source
	fromSn   map[string]string          // restore target (or bound name) -> the snapshot it was restored from
	host     map[string]int             // the holder host each volume serves (or moved) on
	held     map[string]map[int]bool    // hosts that served the volume once (a local root lies there)
	content  map[string]map[string]bool // vol -> lbas with acknowledged content
	snapCont map[string]map[string]bool // "vol|snapshot" -> lbas with content when it was taken
	// A restore target names a source chunk while it has not rewritten it:
	// inherited (the source snapshot had content), wrote (an ok write since
	// its restore), snapWrote ("vol|snapshot": wrote when it was taken).
	inherited map[string]bool
	wrote     map[string]bool
	snapWrote map[string]bool
}

func newOracle() *oracle {
	return &oracle{state: map[string]string{}, snaps: map[string]map[string]bool{},
		targets: map[string]map[string]bool{}, target: map[string]string{}, fromSn: map[string]string{}, host: map[string]int{},
		held: map[string]map[int]bool{}, content: map[string]map[string]bool{}, snapCont: map[string]map[string]bool{},
		inherited: map[string]bool{}, wrote: map[string]bool{}, snapWrote: map[string]bool{}}
}

func (o *oracle) init(v string) {
	if o.snaps[v] == nil {
		o.snaps[v], o.targets[v], o.held[v], o.content[v] = map[string]bool{}, map[string]bool{}, map[int]bool{}, map[string]bool{}
	}
}

// readOK: a read names 1..4 distinct lbas explicitly; with content on the
// volume, only lbas that have it; without, exactly one.
func (o *oracle) readOK(v, lbas string) bool {
	f := strings.Fields(lbas)
	if len(f) == 0 || len(f) > 4 {
		return false
	}
	seen := map[string]bool{}
	for _, b := range f {
		if _, err := strconv.Atoi(b); err != nil || seen[b] {
			return false
		}
		seen[b] = true
		if len(o.content[v]) > 0 && !o.content[v][b] {
			return false
		}
	}
	return len(o.content[v]) > 0 || len(f) == 1
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
	case "v3_op_observe":
		return true // an observation changes nothing
	case "v3_op_read":
		if op.Forced != "" { // a forced read names the lbas its reason needs (C2, C3)
			return st == "serving"
		}
		return st == "serving" && o.readOK(v, op.Params["lbas"])
	case "v3_op_write", "v3_op_stop":
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
		// Never a serving volume: the product refuses it while the lease lives.
		return (st == "created" || st == "stopped" || st == "reserved" || st == "moved") && len(o.targets[v]) == 0
	}
	return false
}

// refusal is the documented refusal op meets now, "" for none. Where two
// apply, it is the one the product checks first. A volume taken out of the
// run (unknown) meets none: nothing may be proposed on it.
func (o *oracle) refusal(op GenOp) string {
	v := op.Volume
	o.init(v)
	st := o.state[v]
	if st == "unknown" {
		return ""
	}
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
		case ts == "" && o.target[v] != "" && o.inherited[v] && !o.snapWrote[v+"|"+s]:
			// Refused only while the snapshot names a source chunk
			// (layout/snapshots.go:102-107).
			return "restore from a snapshot of a restore target"
		}
	case "v3_op_release":
		t := op.Params["target_volume"]
		// Refused only while a root of the target names a source chunk
		// (layout/delete.go:250-257).
		if ts := o.state[t]; o.target[t] == v && (ts == "created" || ts == "serving" || ts == "stopped") && o.inherited[t] && !o.wrote[t] {
			return "release a live restore target"
		}
	}
	return ""
}

// apply follows an ordinary operation answered ok.
func (o *oracle) apply(op GenOp) {
	v := op.Volume
	o.init(v)
	switch op.Action {
	case "v3_op_create":
		o.state[v] = "created"
	case "v3_op_attach":
		o.state[v] = "serving"
		o.held[v][o.host[v]] = true
	case "v3_op_write":
		for _, b := range strings.Fields(op.Params["lbas"]) {
			o.content[v][b] = true
		}
		o.wrote[v] = true
	case "v3_op_snapshot":
		o.snaps[v][op.Params["name"]] = true
		o.snapWrote[v+"|"+op.Params["name"]] = o.wrote[v]
		c := map[string]bool{}
		for b := range o.content[v] {
			c[b] = true
		}
		o.snapCont[v+"|"+op.Params["name"]] = c
	case "v3_op_snapshot_delete":
		delete(o.snaps[v], op.Params["name"])
	case "v3_op_restore":
		t := op.Params["target_volume"]
		o.init(t)
		o.state[t], o.target[t], o.fromSn[t], o.host[t] = "created", v, op.Params["snapshot"], 0 // its placement host
		o.targets[v][op.Params["snapshot"]] = true
		for b := range o.snapCont[v+"|"+op.Params["snapshot"]] {
			o.content[t][b] = true
		}
		o.inherited[t], o.wrote[t] = len(o.snapCont[v+"|"+op.Params["snapshot"]]) > 0, false
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

// answered follows a negative operation and its answer: nothing changes but
// the documented side effects; an unknown answer without one takes the
// volume out.
func (o *oracle) answered(op GenOp, outcome string) {
	switch lim := o.refusal(op); {
	case lim == "delete a source whose snapshot has a restore target":
		if outcome == "unknown" {
			o.state[op.Volume] = "reserved" // OG:193-195: the reservation committed first
		}
	case lim == "restore from a snapshot of a restore target":
		t := op.Params["target_volume"]
		o.state[t], o.target[t], o.fromSn[t] = "bound", op.Volume, op.Params["snapshot"] // SD:693
	case outcome == "unknown":
		o.state[op.Volume] = "unknown"
	}
}

// oracleCheck replays a run answered as answer() answers it and returns the
// first proposal the oracle disagrees with.
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
		o.answered(op, answer(op))
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

// allLimits are the documented refusals the generator probes: those the
// typed actions answer refused, and the two whose unknown answer has a
// documented side effect, and the eleventh snapshot (a product result).
var allLimits = []string{
	"create a deleted name", "delete while attached", "delete a source whose snapshot has a restore target",
	"delete a snapshot that has a restore target", "an eleventh snapshot", "restore into an existing name",
	"restore from a snapshot of a restore target", "release a live restore target",
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
	// Ten live snapshots need a run that mostly takes snapshots.
	ops, _ = run(t, GenConfig{Seed: 1, Ops: 400, Pool: pool(4), Blocks: 8, Negative: 0.3,
		Weights: map[string]int{"v3_op_create": 1, "v3_op_attach": 1, "v3_op_snapshot": 40}})
	if err := oracleCheck(ops); err != nil {
		t.Fatal(err)
	}
	for _, o := range ops {
		if o.Negative != "" {
			limits[o.Negative]++
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

// Reads always name their lbas: never "written" (the handler's own set does
// not know a restore target's inherited content), never empty.
func TestGeneratorReadsNameTheirLBAs(t *testing.T) {
	reads, targetReads := 0, 0
	for seed := uint64(1); seed <= 10; seed++ {
		ops, _ := run(t, GenConfig{Seed: seed, Ops: 1000, Pool: pool(30), Blocks: 8})
		target := map[string]bool{}
		for _, o := range ops {
			if o.Action == "v3_op_restore" && o.Negative == "" {
				target[o.Params["target_volume"]] = true
			}
			if o.Action != "v3_op_read" {
				continue
			}
			reads++
			if target[o.Volume] {
				targetReads++
			}
			if l := o.Params["lbas"]; l == "written" || strings.TrimSpace(l) == "" {
				t.Fatalf("seed %d op %d: read lbas %q", seed, o.No, l)
			}
		}
	}
	if reads == 0 || targetReads == 0 {
		t.Fatalf("%d reads, %d of restore targets", reads, targetReads)
	}
}

func TestGeneratorEndsOnTheBudgetOrAnExhaustedPool(t *testing.T) {
	ops, g := run(t, GenConfig{Seed: 3, Ops: 50, Pool: pool(40), Blocks: 4})
	drawn := 0
	for _, o := range ops {
		drawn += btoi(o.Forced == "")
	}
	if drawn != 50 || g.Ended() != "ops" {
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
	if cov["drawn"] != 10000 || cov["estimated_ops_per_hour"].(int) <= 0 {
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
		return []GenOp{op("v3_op_create", "g1"), op("v3_op_attach", "g1", "host", "0"),
			op("v3_op_write", "g1", "lbas", "3 5", "round", "r"), op("v3_op_snapshot", "g1", "name", "g1-s1")}
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
		"an ordinary delete of a serving volume": {
			red:   append(base(), op("v3_op_delete", "g1")),
			green: append(base(), neg(op("v3_op_delete", "g1"), "delete while attached")),
		},
		"restore offered from a restore target": {
			red:   append(targetServes(), op("v3_op_restore", "g2", "snapshot", "g2-s1", "target_volume", "g3")),
			green: row8(),
		},
		"a source delete answered unknown does not reserve": {
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
		"a bound name attached": {
			// The bound name never serves; the source still does.
			red:   append(row8(), op("v3_op_attach", "g3", "host", "0")),
			green: append(row8(), op("v3_op_write", "g2", "lbas", "1", "round", "r2")),
		},
		"attach on an earlier local root": {
			red:   append(movedBack(), op("v3_op_attach", "g1", "host", "0")),
			green: append(movedBack(), op("v3_op_delete", "g1")),
		},
		"attach on the old host after a move": {
			red:   append(base(), op("v3_op_move", "g1"), op("v3_op_attach", "g1", "host", "0")),
			green: append(base(), op("v3_op_move", "g1"), op("v3_op_attach", "g1", "host", "1")),
		},
		"attach with other geometry": {
			red:   []GenOp{op("v3_op_create", "g1"), op("v3_op_attach", "g1", "host", "0", "blocks", "2048")},
			green: []GenOp{op("v3_op_create", "g1"), op("v3_op_attach", "g1", "host", "0")},
		},
		"a negative under the wrong limit": {
			red:   append(base(), neg(op("v3_op_delete", "g1"), "create a deleted name")),
			green: append(base(), neg(op("v3_op_delete", "g1"), "delete while attached")),
		},
		"a live target released as ordinary": {
			red:   append(withTarget(), op("v3_op_release", "g1", "target_volume", "g2")),
			green: append(withTarget(), neg(op("v3_op_release", "g1", "target_volume", "g2"), "release a live restore target")),
		},
		"a target released after it wrote": {
			// v11 at run 3: a target that rewrote the source's chunk no longer
			// names it, and its release is admitted.
			red: append(withTarget(), op("v3_op_attach", "g2", "host", "0"), op("v3_op_write", "g2", "lbas", "1", "round", "r2"),
				neg(op("v3_op_release", "g1", "target_volume", "g2"), "release a live restore target")),
			green: append(withTarget(), op("v3_op_attach", "g2", "host", "0"), op("v3_op_read", "g2", "lbas", "3"),
				neg(op("v3_op_release", "g1", "target_volume", "g2"), "release a live restore target")),
		},
		"a restore from a target's snapshot taken after it wrote": {
			// v09 at run 3: its snapshot named only its own chunk, and the
			// restore was admitted.
			red: append(withTarget(), op("v3_op_attach", "g2", "host", "0"), op("v3_op_write", "g2", "lbas", "1", "round", "r2"),
				op("v3_op_snapshot", "g2", "name", "g2-s1"),
				neg(op("v3_op_restore", "g2", "snapshot", "g2-s1", "target_volume", "g3"), "restore from a snapshot of a restore target")),
			green: append(targetServes(), op("v3_op_write", "g2", "lbas", "1", "round", "r2"),
				neg(op("v3_op_restore", "g2", "snapshot", "g2-s1", "target_volume", "g3"), "restore from a snapshot of a restore target")),
		},
		"a restore from a target that inherited nothing": {
			red: []GenOp{op("v3_op_create", "g1"), op("v3_op_attach", "g1", "host", "0"), op("v3_op_snapshot", "g1", "name", "g1-s1"),
				op("v3_op_restore", "g1", "snapshot", "g1-s1", "target_volume", "g2"), op("v3_op_attach", "g2", "host", "0"),
				op("v3_op_snapshot", "g2", "name", "g2-s1"),
				neg(op("v3_op_restore", "g2", "snapshot", "g2-s1", "target_volume", "g3"), "restore from a snapshot of a restore target")},
			green: row8(),
		},
		"a restore target attached on its moved source's host": {
			red: append(base(), op("v3_op_move", "g1"), op("v3_op_attach", "g1", "host", "1"),
				op("v3_op_restore", "g1", "snapshot", "g1-s1", "target_volume", "g2"), op("v3_op_attach", "g2", "host", "1")),
			green: append(base(), op("v3_op_move", "g1"), op("v3_op_attach", "g1", "host", "1"),
				op("v3_op_restore", "g1", "snapshot", "g1-s1", "target_volume", "g2"), op("v3_op_attach", "g2", "host", "0")),
		},
		"a read of the written set": {
			red:   append(base(), op("v3_op_read", "g1", "lbas", "written")),
			green: append(base(), op("v3_op_read", "g1", "lbas", "3 5")),
		},
		"a read of no lba": {
			red:   append(base(), op("v3_op_read", "g1", "lbas", "")),
			green: append(base(), op("v3_op_read", "g1", "lbas", "5")),
		},
		"a read beside the content": {
			red:   append(base(), op("v3_op_read", "g1", "lbas", "3 4")),
			green: append(base(), op("v3_op_read", "g1", "lbas", "3")),
		},
		"a restore target read without its inherited content": {
			// g2 inherits lbas 3 and 5 from g1-s1; a read of 6 claims none.
			red:   append(withTarget(), op("v3_op_attach", "g2", "host", "0"), op("v3_op_read", "g2", "lbas", "6")),
			green: append(withTarget(), op("v3_op_attach", "g2", "host", "0"), op("v3_op_read", "g2", "lbas", "5")),
		},
		"a never-written volume read on several lbas": {
			red:   []GenOp{op("v3_op_create", "g1"), op("v3_op_attach", "g1", "host", "0"), op("v3_op_read", "g1", "lbas", "1 2")},
			green: []GenOp{op("v3_op_create", "g1"), op("v3_op_attach", "g1", "host", "0"), op("v3_op_read", "g1", "lbas", "2")},
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

// The end-of-run sweep: every acknowledged lba of a serving volume read back
// (its own writes and a restore target's inherited content), in reads of at
// most four, one observation of every volume that exists or was deleted, and
// nothing on a name never used.
func TestGeneratorSweepReadsEveryAcknowledgedBlockAndObservesEachVolume(t *testing.T) {
	g, err := NewGenerator(GenConfig{Seed: 1, Ops: 10, Pool: pool(5), Blocks: 8})
	if err != nil {
		t.Fatal(err)
	}
	drive(g, op("v3_op_create", "g1"), op("v3_op_attach", "g1", "host", "0"), op("v3_op_write", "g1", "lbas", "7 1 3", "round", "r"),
		op("v3_op_snapshot", "g1", "name", "g1-s1"), op("v3_op_write", "g1", "lbas", "5 6", "round", "r2"),
		op("v3_op_restore", "g1", "snapshot", "g1-s1", "target_volume", "g2"), op("v3_op_attach", "g2", "host", "0"),
		op("v3_op_create", "g3"), op("v3_op_delete", "g3"), op("v3_op_create", "g4"))
	var got []string
	for _, o := range g.m.Sweep() {
		got = append(got, o.Action+" "+o.Volume+" "+o.Params["lbas"])
	}
	want := []string{"v3_op_read g1 1 3 5 6", "v3_op_read g1 7", "v3_op_observe g1 ", "v3_op_read g2 1 3 7", "v3_op_observe g2 ",
		"v3_op_observe g3 ", "v3_op_observe g4 "}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sweep\n%q\nwant\n%q", got, want)
	}
}

// drive feeds a generator a hand-built run, answered as answer() answers.
func drive(g *Generator, ops ...GenOp) {
	for _, o := range ops {
		g.Learn(o, answer(o))
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
		g, err := NewGenerator(GenConfig{Seed: 1, Ops: 10, Pool: pool(4), Blocks: 8})
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	src := func() []GenOp {
		return []GenOp{op("v3_op_create", "g1"), op("v3_op_attach", "g1", "host", "0"), op("v3_op_write", "g1", "lbas", "2 6", "round", "r"),
			op("v3_op_snapshot", "g1", "name", "g1-s1"), op("v3_op_write", "g1", "lbas", "7", "round", "r2"),
			op("v3_op_restore", "g1", "snapshot", "g1-s1", "target_volume", "g2")}
	}

	g := newG()
	drive(g, op("v3_op_create", "g1"), op("v3_op_attach", "g1", "host", "0"))
	if has(g.ordinary(), "v3_op_delete", "g1", "") || !has(g.negatives(), "v3_op_delete", "g1", limDeleteAttached) {
		t.Error("a serving volume offered an ordinary delete, or no negative one")
	}
	drive(g, op("v3_op_stop", "g1"))
	if has(g.ordinary(), "v3_op_attach", "g1", "") || has(g.negatives(), "v3_op_attach", "g1", "") {
		t.Error("attach proposed after a stop")
	}
	if has(g.ordinary(), "v3_op_delete", "g1", "") {
		t.Error("a stopped volume is deleted within its lease horizon (no observation shows it expired)")
	}
	g.m.Observed(leaseRec("g1", "revoking"))
	if has(g.ordinary(), "v3_op_delete", "g1", "") {
		t.Error("a stopped volume is deleted while its lease is revoking")
	}
	g.m.Observed(leaseRec("g1", "expired"))
	if !has(g.ordinary(), "v3_op_delete", "g1", "") {
		t.Error("a stopped volume without targets past its lease is not deleted")
	}

	// Reads: the target inherits the snapshot's lbas (2, 6), not the later 7.
	g = newG()
	drive(g, append(src(), op("v3_op_attach", "g2", "host", "0"))...)
	for i := 0; i < 50; i++ {
		for _, c := range g.ordinary() {
			if c.op.Action != "v3_op_read" {
				continue
			}
			for _, b := range strings.Fields(c.op.Params["lbas"]) {
				if (c.op.Volume == "g2" && b != "2" && b != "6") || (c.op.Volume == "g1" && b != "2" && b != "6" && b != "7") {
					t.Fatalf("read of %s names lba %s outside its content", c.op.Volume, b)
				}
			}
		}
	}
	g3 := newG()
	drive(g3, op("v3_op_create", "g1"), op("v3_op_attach", "g1", "host", "0"))
	for _, c := range g3.ordinary() {
		if c.op.Action == "v3_op_read" && len(strings.Fields(c.op.Params["lbas"])) != 1 {
			t.Errorf("a never-written volume read on %q", c.op.Params["lbas"])
		}
	}

	drive(g, op("v3_op_snapshot", "g2", "name", "g2-s1"))
	if has(g.ordinary(), "v3_op_restore", "g2", "") {
		t.Error("a restore offered from a restore target's snapshot")
	}
	if !has(g.negatives(), "v3_op_restore", "g2", limRestoreTarget, "target_volume", "g3") {
		t.Error("no negative restore from a restore target's snapshot")
	}
	drive(g, neg(op("v3_op_restore", "g2", "snapshot", "g2-s1", "target_volume", "g3"), limRestoreTarget))
	if g.vols["g3"].state != stBound || g.vols["g2"].state != stServing || has(g.ordinary(), "v3_op_create", "g3", "") {
		t.Errorf("a restore from a target's snapshot answered unknown: target %q (want bound), source %q (want serving)",
			g.vols["g3"].state, g.vols["g2"].state)
	}
	if !has(g.negatives(), "v3_op_restore", "g1", limRestoreExisting, "target_volume", "g3") {
		t.Error("a bound name is not probed with a restore into an existing name")
	}

	g = newG()
	drive(g, append(src(), op("v3_op_stop", "g1"))...)
	if has(g.negatives(), "v3_op_delete", "g1", limDeleteSource) {
		t.Error("a source delete probed within the lease horizon (the horizon refuses first)")
	}
	g.m.Observed(leaseRec("g1", "expired"))
	if !has(g.negatives(), "v3_op_delete", "g1", limDeleteSource) || !has(g.negatives(), "v3_op_snapshot_delete", "g1", limDeleteSnapDep) {
		t.Error("no negative source delete or store-direct snapshot delete for a stopped source with a target")
	}
	// Refused (the lease horizon not passed): nothing reserved.
	g.Learn(neg(op("v3_op_delete", "g1"), limDeleteSource), "refused")
	if g.vols["g1"].state != stStopped {
		t.Errorf("a source delete refused before persistence left the source %q, not stopped", g.vols["g1"].state)
	}
	g.Learn(neg(op("v3_op_delete", "g1"), limDeleteSource), "unknown")
	if g.vols["g1"].state != stReserved {
		t.Errorf("a source delete answered unknown left the source %q, not reserved", g.vols["g1"].state)
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

	// No attach negatives: the typed attach cannot answer refused.
	g = newG()
	drive(g, op("v3_op_create", "g1"))
	for _, c := range g.negatives() {
		if c.op.Action == "v3_op_attach" {
			t.Errorf("an attach negative: %v", c.op)
		}
	}
	drive(g, op("v3_op_attach", "g1", "host", "0"), op("v3_op_move", "g1"))
	if !has(g.ordinary(), "v3_op_attach", "g1", "", "host", "1") || has(g.ordinary(), "v3_op_attach", "g1", "", "host", "0") {
		t.Error("a moved volume offers no attach on its new host, or one on its old host")
	}
	drive(g, op("v3_op_attach", "g1", "host", "1"), op("v3_op_write", "g1", "lbas", "1", "round", "r"), op("v3_op_move", "g1"))
	if has(g.ordinary(), "v3_op_attach", "g1", "") {
		t.Error("moved back: an attach proposed on the earlier root")
	}
	if !has(g.ordinary(), "v3_op_delete", "g1", "") {
		t.Error("a moved volume is not deleted")
	}
	for _, c := range append(g.negatives(), g.ordinary()...) {
		if c.op.Action == "v3_op_attach" {
			t.Errorf("an attach proposed on a volume moved back: %v", c.op)
		}
	}

	// A target names the source's chunk until it writes: then its release is
	// no negative, and only its snapshots taken before the write are.
	g = newG()
	drive(g, append(src(), op("v3_op_attach", "g2", "host", "0"), op("v3_op_snapshot", "g2", "name", "g2-s1"))...)
	if !has(g.negatives(), "v3_op_release", "g1", limReleaseLive, "target_volume", "g2") {
		t.Error("no negative release of a target that never wrote")
	}
	drive(g, op("v3_op_write", "g2", "lbas", "4", "round", "r3"), op("v3_op_snapshot", "g2", "name", "g2-s2"))
	if has(g.negatives(), "v3_op_release", "g1", limReleaseLive) {
		t.Error("a target that wrote since its restore is released as a negative")
	}
	if !has(g.negatives(), "v3_op_restore", "g2", limRestoreTarget, "snapshot", "g2-s1") || has(g.negatives(), "v3_op_restore", "g2", limRestoreTarget, "snapshot", "g2-s2") {
		t.Error("the restore negative does not use the target's snapshot taken before its write")
	}
	drive(g, op("v3_op_snapshot_delete", "g2", "name", "g2-s1"))
	if has(g.negatives(), "v3_op_restore", "g2", limRestoreTarget) {
		t.Error("a restore negative from a snapshot taken after the target wrote")
	}
	// A restore target starts on its placement host, not its source's (v16 at
	// run 3: its source had moved to host 1, the target was started there and
	// never got an assignment).
	g = newG()
	drive(g, op("v3_op_create", "g1"), op("v3_op_attach", "g1", "host", "0"), op("v3_op_write", "g1", "lbas", "1", "round", "r"),
		op("v3_op_snapshot", "g1", "name", "g1-s1"), op("v3_op_move", "g1"), op("v3_op_attach", "g1", "host", "1"),
		op("v3_op_restore", "g1", "snapshot", "g1-s1", "target_volume", "g2"))
	if !has(g.ordinary(), "v3_op_attach", "g2", "", "host", "0") || has(g.ordinary(), "v3_op_attach", "g2", "", "host", "1") {
		t.Error("a restore target of a moved source is not attached on its placement host 0")
	}

	// An unknown write in a fault run may have persisted: no release negative.
	gf, _ := NewGenerator(GenConfig{Seed: 1, Ops: 10, Pool: pool(4), Blocks: 8, Faults: 0.1, FaultProcesses: []string{"blockvolume:0"}})
	drive(gf, append(src(), op("v3_op_attach", "g2", "host", "0"))...)
	gf.Learn(op("v3_op_write", "g2", "lbas", "4", "round", "r3"), "unknown")
	if has(gf.negatives(), "v3_op_release", "g1", limReleaseLive) {
		t.Error("a target whose write was unknown is released as a negative")
	}

	g = newG()
	drive(g, op("v3_op_create", "g1"), op("v3_op_attach", "g1", "host", "0"))
	g.Learn(neg(op("v3_op_delete", "g1"), limDeleteAttached), "ok")
	if g.vols["g1"].state != stUnknown {
		t.Error("a negative answered ok left the volume in the run")
	}
}

// settledRec and healthyRec are the records the faults write: the model
// learns from them, not from the invokes.
func settledRec(vol, state string) tr.HistoryRecord {
	return tr.HistoryRecord{Kind: tr.HistoryObserve, Verb: "settled", Result: map[string]any{"volume": vol, "state": state, "evidence": "test"}}
}

func healthyRec(notServing []string, third ...string) tr.HistoryRecord {
	ns := []any{}
	for _, s := range notServing {
		ns = append(ns, s)
	}
	res := map[string]any{"healthy": len(notServing) == 0, "down": []any{}, "not_serving": ns}
	if len(third) > 0 {
		var ts []any
		for _, v := range third {
			ts = append(ts, map[string]any{"volume": v, "why": "test"})
		}
		res["third_state"] = ts
	}
	return tr.HistoryRecord{Kind: tr.HistoryObserve, Verb: "healthy", Result: res}
}

// offered lists the actions offered for vol, ordinary and negative.
func offered(g *Generator, vol string) map[string]bool {
	out := map[string]bool{}
	for _, c := range append(g.ordinary(), g.negatives()...) {
		if c.op.Volume == vol {
			out[c.op.Action] = true
		}
	}
	return out
}

// F9 and the settled record: an unknown attach or stop leaves the volume
// unsettled, offered nothing but observation, until a settled record places
// it: held serves again, not_held is stopped. A healthy record's not_serving
// fences a volume from reads and writes until a later one leaves it out;
// third_state takes it out of the run.
func TestModelLearnsFromSettledAndHealthyRecords(t *testing.T) {
	newG := func() *Generator {
		g, err := NewGenerator(GenConfig{Seed: 1, Ops: 10, Pool: pool(4), Blocks: 8, Faults: 0.1, FaultProcesses: []string{"blockvolume:0"}})
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	// An unknown attach: unsettled, nothing offered, observed by the sweep.
	g := newG()
	drive(g, op("v3_op_create", "g1"))
	g.Learn(op("v3_op_attach", "g1", "host", "0"), "unknown")
	if g.vols["g1"].state != stUnsettled || len(offered(g, "g1")) != 0 {
		t.Fatalf("an unknown attach: state %q, offered %v", g.vols["g1"].state, offered(g, "g1"))
	}
	var sw []string
	for _, o := range g.m.Sweep() {
		if o.Volume == "g1" {
			sw = append(sw, o.Action)
		}
	}
	if !slices.Equal(sw, []string{"v3_op_observe"}) {
		t.Fatalf("sweep of an unsettled volume %v", sw)
	}
	// The invoke alone settles nothing; another volume's record neither.
	g.m.Observed(settledRec("g2", "held"))
	if g.vols["g1"].state != stUnsettled || g.vols["g2"].state != stUnborn {
		t.Fatal("a settled record of another volume moved one")
	}
	g.m.Observed(settledRec("g1", "held"))
	if g.vols["g1"].state != stServing || !offered(g, "g1")["v3_op_write"] || !offered(g, "g1")["v3_op_read"] {
		t.Fatalf("settled held: state %q, offered %v", g.vols["g1"].state, offered(g, "g1"))
	}
	// An unknown stop settled not_held is stopped.
	g.Learn(op("v3_op_stop", "g1"), "unknown")
	if g.vols["g1"].state != stUnsettled {
		t.Fatal("an unknown stop is not unsettled")
	}
	g.m.Observed(settledRec("g1", "not_held"))
	g.m.Observed(leaseRec("g1", "expired"))
	if g.vols["g1"].state != stStopped || !offered(g, "g1")["v3_op_delete"] || offered(g, "g1")["v3_op_write"] {
		t.Fatalf("settled not_held: state %q, offered %v", g.vols["g1"].state, offered(g, "g1"))
	}
	// A settled record for a volume that is not unsettled changes nothing.
	g.m.Observed(settledRec("g1", "held"))
	if g.vols["g1"].state != stStopped {
		t.Fatal("a settled record moved a stopped volume")
	}

	// not_serving fences reads and writes until a healthy record leaves it out.
	g = newG()
	drive(g, op("v3_op_create", "g1"), op("v3_op_attach", "g1", "host", "0"), op("v3_op_create", "g2"), op("v3_op_attach", "g2", "host", "0"))
	g.m.Observed(healthyRec([]string{"g1"}))
	if a := offered(g, "g1"); a["v3_op_write"] || a["v3_op_read"] || !offered(g, "g2")["v3_op_write"] {
		t.Fatalf("fenced g1 offered %v, g2 %v", a, offered(g, "g2"))
	}
	for _, o := range g.m.Sweep() {
		if o.Volume == "g1" && o.Action == "v3_op_read" {
			t.Fatal("the sweep reads a fenced volume")
		}
	}
	g.m.Observed(tr.HistoryRecord{Kind: tr.HistoryObserve, Verb: "observe", Result: map[string]any{"not_serving": []any{}}})
	if offered(g, "g1")["v3_op_write"] {
		t.Fatal("a record that is not a healthy record unfenced g1")
	}
	g.m.Observed(healthyRec(nil))
	if !offered(g, "g1")["v3_op_write"] {
		t.Fatal("g1 still fenced after a healthy record without it")
	}
	// third_state: out of the run.
	g.m.Observed(healthyRec(nil, "g2"))
	if g.vols["g2"].state != stUnknown || len(offered(g, "g2")) != 0 {
		t.Fatalf("third state: %q %v", g.vols["g2"].state, offered(g, "g2"))
	}
}

// unreadAtRelease states C1's need on a proposal list answered as answer()
// answers: at every ordinary stop or move, no lba of the volume written ok
// since its last ok read; after an attach of a moved volume, its whole
// content read before any other operation on it. It returns the violations.
func unreadAtRelease(ops []GenOp) []string {
	dirty := map[string]map[string]bool{}
	content := map[string]map[string]bool{}
	moved := map[string]bool{}
	owed := map[string]map[string]bool{} // lbas still to read after an attach on a new host
	var bad []string
	for _, o := range ops {
		v := o.Volume
		if dirty[v] == nil {
			dirty[v], content[v] = map[string]bool{}, map[string]bool{}
		}
		if o.Negative != "" || answer(o) != "ok" {
			continue
		}
		if len(owed[v]) > 0 && o.Action != "v3_op_read" {
			bad = append(bad, fmt.Sprintf("op %d %s %s before its content was read after the attach on a new host", o.No, o.Action, v))
			delete(owed, v)
		}
		switch o.Action {
		case "v3_op_write":
			for _, b := range strings.Fields(o.Params["lbas"]) {
				dirty[v][b], content[v][b] = true, true
			}
		case "v3_op_read":
			for _, b := range strings.Fields(o.Params["lbas"]) {
				delete(dirty[v], b)
				delete(owed[v], b)
			}
		case "v3_op_stop", "v3_op_move":
			if len(dirty[v]) > 0 {
				bad = append(bad, fmt.Sprintf("op %d %s %s with %d lbas unread", o.No, o.Action, v, len(dirty[v])))
			}
			moved[v] = o.Action == "v3_op_move"
		case "v3_op_attach":
			if moved[v] {
				owed[v] = maps.Clone(content[v])
				moved[v] = false
			}
		case "v3_op_restore":
			t := o.Params["target_volume"]
			if content[t] == nil {
				dirty[t], content[t] = map[string]bool{}, map[string]bool{}
			}
		}
	}
	return bad
}

// C1: before an ordinary stop or move, the volume's dirty lbas are read
// back (forced reads, outside the budget); after an attach on a new host,
// its whole content is read once.
func TestGeneratorReadsBackBeforeAStopOrMove(t *testing.T) {
	forced := 0
	for seed := uint64(1); seed <= 20; seed++ {
		ops, _ := run(t, GenConfig{Seed: seed, Ops: 400, Pool: pool(20), Blocks: 8})
		if bad := unreadAtRelease(ops); len(bad) != 0 {
			t.Fatalf("seed %d: %v", seed, bad)
		}
		if err := oracleCheck(ops); err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		for _, o := range ops {
			if o.Forced == "readback" || o.Forced == "attach-check" {
				forced++
				if o.Action != "v3_op_read" || len(strings.Fields(o.Params["lbas"])) > 4 {
					t.Fatalf("forced %+v", o)
				}
			}
		}
	}
	if forced == 0 {
		t.Fatal("no read was forced")
	}
	// Red controls: a stop after an unread write, and an attach on a new host
	// followed by another operation, are caught; their green twins pass.
	w := func(lbas string) GenOp { return op("v3_op_write", "g1", "lbas", lbas, "round", "r") }
	base := []GenOp{op("v3_op_create", "g1"), op("v3_op_attach", "g1", "host", "0"), w("1 2")}
	for name, c := range map[string]struct{ red, green []GenOp }{
		"stop with an unread write": {
			red:   append(slices.Clone(base), op("v3_op_stop", "g1")),
			green: append(slices.Clone(base), op("v3_op_read", "g1", "lbas", "1 2"), op("v3_op_stop", "g1")),
		},
		"move, attach, write before the content is read": {
			red: append(slices.Clone(base), op("v3_op_read", "g1", "lbas", "1 2"), op("v3_op_move", "g1"),
				op("v3_op_attach", "g1", "host", "1"), w("3")),
			green: append(slices.Clone(base), op("v3_op_read", "g1", "lbas", "1 2"), op("v3_op_move", "g1"),
				op("v3_op_attach", "g1", "host", "1"), op("v3_op_read", "g1", "lbas", "1 2"), w("3")),
		},
	} {
		if bad := unreadAtRelease(seq(c.green...)); len(bad) != 0 {
			t.Errorf("%s: green twin rejected: %v", name, bad)
		}
		if bad := unreadAtRelease(seq(c.red...)); len(bad) == 0 {
			t.Errorf("%s: red control accepted", name)
		}
	}
}

// pairGaps states C2's and C3's needs on a proposal list answered as
// answer() answers: a restore target's first operations after its first
// attach read every inherited lba; and before an ordinary stop or move of a
// source or target whose pair both serve, both sides read the same lbas
// (the union of their contents) since the restore. It returns the gaps.
func pairGaps(ops []GenOp) []string {
	content := map[string]map[string]bool{}
	serving := map[string]bool{}
	source := map[string]string{}    // target -> source (live pair)
	inherit := map[string][]string{} // target -> inherited lbas, until read
	attached := map[string]bool{}
	read := map[string]map[string]bool{}  // "src|tgt|vol" -> lbas read on vol since the restore
	snapc := map[string]map[string]bool{} // "vol|snapshot" -> its content
	// crossW: "src|tgt|side" -> lba -> the op number of side's latest write
	// of it since the restore; crossed: "src|tgt|side" -> such a write was
	// then read on the other side with no write of the reader's own to that
	// lba after it (C3's rule: the reader need not hold the block).
	crossW, crossed := map[string]map[string]int{}, map[string]bool{}
	var bad []string
	set := func(v string) map[string]bool {
		if content[v] == nil {
			content[v] = map[string]bool{}
		}
		return content[v]
	}
	for _, o := range ops {
		v := o.Volume
		if o.Negative != "" || answer(o) != "ok" {
			continue
		}
		if in := inherit[v]; len(in) > 0 && attached[v] && o.Action != "v3_op_read" {
			bad = append(bad, fmt.Sprintf("op %d %s %s before its inherited lbas %v were read", o.No, o.Action, v, in))
			delete(inherit, v)
		}
		switch o.Action {
		case "v3_op_write":
			for _, b := range strings.Fields(o.Params["lbas"]) {
				for t, s := range source {
					if v == s || v == t {
						k := s + "|" + t + "|" + v
						if crossW[k] == nil {
							crossW[k] = map[string]int{}
						}
						crossW[k][b] = o.No
					}
				}
				set(v)[b] = true
			}
		case "v3_op_read":
			for _, b := range strings.Fields(o.Params["lbas"]) {
				for t, s := range source {
					if other := map[string]string{s: t, t: s}[v]; other != "" && crossW[s+"|"+t+"|"+other][b] > crossW[s+"|"+t+"|"+v][b] {
						crossed[s+"|"+t+"|"+other] = true
					}
				}
				inherit[v] = slices.DeleteFunc(inherit[v], func(x string) bool { return x == b })
				for t, s := range source {
					if v == t || v == s {
						k := s + "|" + t + "|" + v
						if read[k] == nil {
							read[k] = map[string]bool{}
						}
						read[k][b] = true
					}
				}
			}
		case "v3_op_attach":
			serving[v] = true
			attached[v] = true
		case "v3_op_restore":
			t := o.Params["target_volume"]
			source[t] = v
			for b := range snapc[v+"|"+o.Params["snapshot"]] {
				set(t)[b] = true
				inherit[t] = append(inherit[t], b)
			}
		case "v3_op_snapshot":
			snapc[v+"|"+o.Params["name"]] = maps.Clone(set(v))
		case "v3_op_stop", "v3_op_move":
			for t, s := range source {
				if (v == s || v == t) && serving[s] && serving[t] {
					for _, side := range []string{s, t} {
						common := false
						for b := range content[t] {
							common = common || content[s][b]
						}
						if common && !crossed[s+"|"+t+"|"+side] {
							bad = append(bad, fmt.Sprintf("op %d %s %s: no write on %s read back across", o.No, o.Action, v, side))
						}

					}
				}
			}
			serving[v] = false
		case "v3_op_delete":
			delete(source, v)
		}
	}
	return bad
}

// C2 and C3: a restore target's first reads are its inherited lbas; before
// a source or target of a live pair leaves while both serve, both sides
// read the same lbas.
func TestGeneratorReadsTheInheritedLBAsAndGivesAPairWindow(t *testing.T) {
	var inherited, pairs int
	for seed := uint64(1); seed <= 30; seed++ {
		ops, _ := run(t, GenConfig{Seed: seed, Ops: 400, Pool: pool(20), Blocks: 8})
		if err := oracleCheck(ops); err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		// The pair needs every content lba read on both sides; content is
		// tracked here from the shadow's view at restore (the target starts
		// with the source snapshot's lbas, known to the generator only), so
		// check inherited reads through the proposals' reasons and the pair
		// reads through pairGaps on the reads' coverage of written lbas.
		for _, o := range ops {
			inherited += btoi(o.Forced == "inherited")
			pairs += btoi(o.Forced == "pair")
		}
		if bad := pairGaps(ops); len(bad) != 0 {
			t.Fatalf("seed %d: %v", seed, bad)
		}
	}
	if inherited == 0 || pairs == 0 {
		t.Fatalf("%d inherited reads, %d pair reads", inherited, pairs)
	}
	// The generator's own state: after the restore the target's first attach
	// is due (its placement host), then its inherited lbas, then at once the
	// source -> target window (C3: a write on the source read on the
	// target); neither side is stopped, moved or deleted before the target ->
	// source window, which comes pairDelay drawn operations after both serve.
	g, _ := NewGenerator(GenConfig{Seed: 1, Ops: 10, Pool: pool(4), Blocks: 8})
	drive(g, op("v3_op_create", "g1"), op("v3_op_attach", "g1", "host", "0"), op("v3_op_write", "g1", "lbas", "2 6", "round", "r"),
		op("v3_op_snapshot", "g1", "name", "g1-s1"), op("v3_op_move", "g1"), op("v3_op_attach", "g1", "host", "1"))
	drive(g, g.m.Due()...) // the move's attach-check reads
	drive(g, g.m.Due()...)
	drive(g, op("v3_op_restore", "g1", "snapshot", "g1-s1", "target_volume", "g2"))
	if d := g.m.Due(); len(d) != 1 || d[0].Action != "v3_op_attach" || d[0].Volume != "g2" || d[0].Params["host"] != "0" || d[0].Forced != "pair-attach" {
		t.Fatalf("due after the restore: %+v", d)
	}
	drive(g, d0(g))
	if d := g.m.Due(); len(d) != 1 || d[0].Volume != "g2" || d[0].Params["lbas"] != "2 6" || d[0].Forced != "inherited" {
		t.Fatalf("due after the target's first attach: %+v", d)
	}
	drive(g, d0(g))
	window := func() []string {
		var got []string
		for d := g.m.Due(); len(d) > 0 && d[0].Forced == "pair"; d = g.m.Due() {
			o := d[0]
			got = append(got, o.Forced+" "+strings.TrimPrefix(o.Action, "v3_op_")+" "+o.Volume+" "+o.Params["lbas"])
			drive(g, o)
		}
		return got
	}
	// Source -> target: an inherited lba written on the source, read on the target.
	if got, want := window(), []string{"pair write g1 2", "pair read g2 2"}; !slices.Equal(got, want) {
		t.Fatalf("the source -> target window:\n%q\nwant\n%q", got, want)
	}
	if c := g.m.c3["g1|g2"]; c == nil || !c.ex[0] || c.ex[1] {
		t.Fatalf("C3 facts after the source -> target window: %+v", c)
	}
	drive(g, op("v3_op_write", "g2", "lbas", "5", "round", "r2"))
	for i := 0; i < pairDelay-1; i++ { // the write above was the first drawn one
		if has(g.ordinary(), "v3_op_stop", "g1", "") || has(g.ordinary(), "v3_op_stop", "g2", "") || has(g.ordinary(), "v3_op_move", "g1", "") {
			t.Fatal("a side of a pending pair offered a stop or move")
		}
		if !has(g.negatives(), "v3_op_delete", "g1", limDeleteAttached) {
			t.Fatal("negatives on a pending pair are held too")
		}
		if d := g.m.Due(); len(d) > 0 && d[0].Forced == "pair" {
			t.Fatalf("the target -> source window before pairDelay: %d", i)
		}
		drive(g, op("v3_op_read", "g1", "lbas", "2"))
	}
	// Target -> source (another inherited lba written on the target, read on
	// the source), then the union of both contents read on both.
	want := []string{"pair write g2 6", "pair read g1 6", "pair read g1 2 5 6", "pair read g2 2 5 6"}
	if got := window(); !slices.Equal(got, want) {
		t.Fatalf("the target -> source window:\n%q\nwant\n%q", got, want)
	}
	if !has(g.ordinary(), "v3_op_stop", "g1", "") || !has(g.ordinary(), "v3_op_stop", "g2", "") {
		t.Fatal("the hold not released after the window")
	} // Red controls for the checker: a target written before its inherited
	// read, a source stopped with its target unread at the source's write.
	w := func(v, lbas string) GenOp { return op("v3_op_write", v, "lbas", lbas, "round", "r") }
	r := func(v, lbas string) GenOp { return op("v3_op_read", v, "lbas", lbas) }
	pairBase := []GenOp{op("v3_op_create", "g1"), op("v3_op_attach", "g1", "host", "0"), w("g1", "1"),
		op("v3_op_snapshot", "g1", "name", "g1-s1"), op("v3_op_restore", "g1", "snapshot", "g1-s1", "target_volume", "g2"),
		op("v3_op_attach", "g2", "host", "0")}
	for name, c := range map[string]struct{ red, green []GenOp }{
		"a target written before its inherited read": {
			red:   append(slices.Clone(pairBase), w("g2", "1")),
			green: append(slices.Clone(pairBase), r("g2", "1"), w("g2", "1")),
		},
		"a source stopped before the pair read": {
			red: append(slices.Clone(pairBase), r("g2", "1"), w("g1", "3"), r("g1", "1 3"), op("v3_op_stop", "g1")),
			green: append(slices.Clone(pairBase), r("g2", "1"), w("g1", "1"), r("g2", "1"), w("g2", "1"), r("g1", "1"),
				w("g1", "3"), r("g1", "1 3"), r("g2", "1 3"), op("v3_op_stop", "g1")),
		},
		"a pair split with no write read across": {
			red:   append(slices.Clone(pairBase), r("g2", "1"), r("g1", "1"), r("g2", "1"), op("v3_op_stop", "g1")),
			green: append(slices.Clone(pairBase), r("g2", "1"), w("g1", "1"), r("g2", "1"), w("g2", "1"), r("g1", "1"), op("v3_op_stop", "g1")),
		},
	} {
		if bad := pairGaps(seq(c.green...)); len(bad) != 0 {
			t.Errorf("%s: green twin rejected: %v", name, bad)
		}
		if bad := pairGaps(seq(c.red...)); len(bad) == 0 {
			t.Errorf("%s: red control accepted", name)
		}
	}
	_ = want
}

// d0 is the first due operation of g.
func d0(g *Generator) GenOp { return g.m.Due()[0] }

// horizonGaps: an ordinary delete of a stopped volume, or the negative
// "delete a source whose snapshot has a restore target", proposed with no
// observation of the volume since its stop (the lease horizon is learnt
// from observations only).
func horizonGaps(ops []GenOp) []string {
	stoppedUnseen := map[string]bool{}
	var bad []string
	for _, o := range ops {
		switch {
		case o.Action == "v3_op_stop" && o.Negative == "" && answer(o) == "ok":
			stoppedUnseen[o.Volume] = true
		case o.Action == "v3_op_observe":
			delete(stoppedUnseen, o.Volume)
		case o.Action == "v3_op_delete" && (o.Negative == "" || o.Negative == "delete a source whose snapshot has a restore target") && stoppedUnseen[o.Volume]:
			bad = append(bad, fmt.Sprintf("op %d delete %s (%q) with no observation since its stop", o.No, o.Volume, o.Negative))
		}
	}
	return bad
}

// The lease horizon: a delete after a stop (ordinary, or the source
// negative) waits for an observation that shows the lease expired; the
// model observes a stopped volume every leaseObserveEvery drawn operations.
func TestGeneratorWaitsForTheLeaseHorizonFromObservations(t *testing.T) {
	deletes, observed := 0, 0
	for seed := uint64(1); seed <= 20; seed++ {
		ops, _ := run(t, GenConfig{Seed: seed, Ops: 600, Pool: pool(30), Blocks: 8, Negative: 0.3})
		if bad := horizonGaps(ops); len(bad) != 0 {
			t.Fatalf("seed %d: %v", seed, bad)
		}
		if err := oracleCheck(ops); err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		for _, o := range ops {
			observed += btoi(o.Forced == "lease")
			deletes += btoi(o.Action == "v3_op_delete" && o.Negative == "")
		}
	}
	if observed == 0 || deletes == 0 {
		t.Fatalf("%d lease observations, %d ordinary deletes", observed, deletes)
	}
	red := seq(op("v3_op_create", "g1"), op("v3_op_attach", "g1", "host", "0"), op("v3_op_stop", "g1"), op("v3_op_delete", "g1"))
	green := seq(op("v3_op_create", "g1"), op("v3_op_attach", "g1", "host", "0"), op("v3_op_stop", "g1"), op("v3_op_observe", "g1"), op("v3_op_delete", "g1"))
	if len(horizonGaps(red)) == 0 || len(horizonGaps(green)) != 0 {
		t.Fatal("the horizon check's red control")
	}
}

// C7's coverage tail: a created volume never attached deleted, a stopped
// volume past its lease deleted, both observed, then a create of a deleted
// name, refused. Deterministic from the state.
func TestCoverageTailDeletesObservesAndProbesADeletedName(t *testing.T) {
	g, _ := NewGenerator(GenConfig{Seed: 1, Ops: 10, Pool: pool(5), Blocks: 8})
	drive(g, op("v3_op_create", "g1"), op("v3_op_create", "g2"), op("v3_op_attach", "g2", "host", "0"), op("v3_op_stop", "g2"),
		op("v3_op_create", "g3"), op("v3_op_attach", "g3", "host", "0"))
	var got []string
	for {
		o, ok := g.Tail()
		if !ok {
			break
		}
		got = append(got, fmt.Sprintf("%s %s %s %s", o.Forced, o.Action, o.Volume, o.Negative))
		g.Learn(o, answer(o))
		if o.Action == "v3_op_observe" {
			g.m.Observed(leaseRec(o.Volume, "expired"))
		}
	}
	want := []string{"tail v3_op_delete g1 ", "tail v3_op_observe g2 ", "tail v3_op_delete g2 ",
		"tail v3_op_observe g1 ", "tail v3_op_observe g2 ", "tail v3_op_create g1 create a deleted name"}
	if !slices.Equal(got, want) {
		t.Fatalf("tail\n%q\nwant\n%q", got, want)
	}

	// A stopped volume whose observation does not show the lease expired is
	// not deleted; nothing deleted, no create probe.
	g, _ = NewGenerator(GenConfig{Seed: 1, Ops: 10, Pool: pool(2), Blocks: 8})
	drive(g, op("v3_op_create", "g1"), op("v3_op_attach", "g1", "host", "0"), op("v3_op_stop", "g1"))
	got = nil
	for {
		o, ok := g.Tail()
		if !ok {
			break
		}
		got = append(got, o.Action+" "+o.Volume)
		g.Learn(o, answer(o))
		if o.Action == "v3_op_observe" {
			g.m.Observed(leaseRec(o.Volume, "revoking"))
		}
	}
	if !slices.Equal(got, []string{"v3_op_observe g1"}) {
		t.Fatalf("tail with the lease live: %q", got)
	}
	// Coverage names the ordinary actions that never ran.
	ops, g2 := run(t, GenConfig{Seed: 3, Ops: 30, Pool: pool(10), Blocks: 4, Weights: map[string]int{"v3_op_create": 1, "v3_op_move": 1}})
	_ = ops
	if nr, _ := g2.Coverage()["ordinary_never_ran"].([]string); !slices.Equal(nr, []string{"v3_op_move"}) {
		t.Fatalf("ordinary_never_ran %v", g2.Coverage()["ordinary_never_ran"])
	}
}

// refused_effect: "delete a source whose snapshot has a restore target"
// refused with its documented effect reserves the volume (as unknown does
// while the outcome map keeps it unknown); a plain refusal reserves
// nothing; a refused_effect the shadow does not model takes the volume out.
func TestModelLearnsARefusedEffect(t *testing.T) {
	stoppedSource := func() *Generator {
		g, _ := NewGenerator(GenConfig{Seed: 1, Ops: 10, Pool: pool(4), Blocks: 8})
		drive(g, op("v3_op_create", "g1"), op("v3_op_attach", "g1", "host", "0"), op("v3_op_write", "g1", "lbas", "1", "round", "r"),
			op("v3_op_snapshot", "g1", "name", "g1-s1"), op("v3_op_restore", "g1", "snapshot", "g1-s1", "target_volume", "g2"),
			op("v3_op_stop", "g1"))
		return g
	}
	for outcome, want := range map[string]string{tr.OutcomeRefusedEffect: stReserved, tr.OutcomeUnknown: stReserved, tr.OutcomeRefused: stStopped} {
		g := stoppedSource()
		g.Learn(neg(op("v3_op_delete", "g1"), limDeleteSource), outcome)
		if got := g.vols["g1"].state; got != want {
			t.Errorf("delete source answered %s: %q, want %q", outcome, got, want)
		}
	}
	g := stoppedSource()
	g.Learn(op("v3_op_snapshot", "g2", "name", "g2-s1"), tr.OutcomeRefusedEffect)
	if g.vols["g2"].state != stUnknown {
		t.Errorf("an unmodelled refused_effect left the volume %q", g.vols["g2"].state)
	}
}

// retry_horizon rides on the source-delete negative only: the declared
// delete retries past the lease horizon so the limit is what answers.
// "Delete while attached" expects the horizon's refusal and never has it.
func TestRetryHorizonOnlyOnTheSourceDeleteNegative(t *testing.T) {
	src, attached := 0, 0
	for seed := uint64(1); seed <= 10; seed++ {
		ops, _ := run(t, GenConfig{Seed: seed, Ops: 1500, Pool: pool(40), Blocks: 8, Negative: 0.4})
		for _, o := range ops {
			has := o.Params["retry_horizon"] == "true"
			switch {
			case o.Negative == limDeleteSource:
				src++
				if !has {
					t.Fatalf("seed %d op %d: source delete without retry_horizon: %v", seed, o.No, o.Params)
				}
			case o.Negative == limDeleteAttached:
				attached++
				fallthrough
			default:
				if _, ok := o.Params["retry_horizon"]; ok {
					t.Fatalf("seed %d op %d: retry_horizon on %s (%q)", seed, o.No, o.Action, o.Negative)
				}
			}
		}
	}
	if src == 0 || attached == 0 {
		t.Fatalf("%d source deletes, %d attached deletes", src, attached)
	}
}

// pairsUnexercised lists the restore pairs of a proposal list (answered as
// answer() answers) that never got, in each direction, a write on one side
// after the restore to a block the other side holds and then a read of it on
// the other side, and that the notes do not excuse: a note is accepted only
// for a side lost to the run (unknown, unsettled, killed, reserved), never
// for a side that simply left (stopped, moved, deleted).
func pairsUnexercised(ops []GenOp, notes []string) []string {
	type pair struct{ s, t string }
	holds := map[string]map[string]bool{}
	snapc := map[string]map[string]bool{}
	set := func(v string) map[string]bool {
		if holds[v] == nil {
			holds[v] = map[string]bool{}
		}
		return holds[v]
	}
	var pairs []pair
	wrote := map[string]map[string]int{} // "s|t|side" -> lba -> op number of side's latest write since the restore
	crossed := map[string]bool{}
	for _, o := range ops {
		if o.Negative != "" || answer(o) != "ok" {
			continue
		}
		v := o.Volume
		switch o.Action {
		case "v3_op_snapshot":
			snapc[v+"|"+o.Params["name"]] = maps.Clone(set(v))
		case "v3_op_restore":
			t := o.Params["target_volume"]
			holds[t] = maps.Clone(snapc[v+"|"+o.Params["snapshot"]])
			pairs = append(pairs, pair{v, t})
		case "v3_op_write":
			for _, b := range strings.Fields(o.Params["lbas"]) {
				for _, p := range pairs {
					if v == p.s || v == p.t {
						k := p.s + "|" + p.t + "|" + v
						if wrote[k] == nil {
							wrote[k] = map[string]int{}
						}
						wrote[k][b] = o.No
					}
				}
				set(v)[b] = true
			}
		case "v3_op_read":
			for _, b := range strings.Fields(o.Params["lbas"]) {
				for _, p := range pairs {
					if other := map[string]string{p.s: p.t, p.t: p.s}[v]; other != "" && wrote[p.s+"|"+p.t+"|"+other][b] > wrote[p.s+"|"+p.t+"|"+v][b] {
						crossed[p.s+"|"+p.t+"|"+other] = true
					}
				}
			}
		}
	}
	var bad []string
	for _, p := range pairs {
		key := p.s + "|" + p.t
		if crossed[key+"|"+p.s] && crossed[key+"|"+p.t] {
			continue
		}
		excused := false
		for _, n := range notes {
			if strings.Contains(n, key+":") {
				excused = strings.Contains(n, "unknown") || strings.Contains(n, "unsettled") || strings.Contains(n, "killed") || strings.Contains(n, "reserved")
			}
		}
		if !excused {
			bad = append(bad, key)
		}
	}
	return bad
}

// Every restore pair whose sides stay in the run gets its window: cross
// writes and reads in both directions; a pair whose side was lost is noted
// in coverage with why.
func TestEveryRestorePairGetsItsWindow(t *testing.T) {
	pairs, noted := 0, 0
	for seed := uint64(1); seed <= 30; seed++ {
		ops, g := run(t, GenConfig{Seed: seed, Ops: 400, Pool: pool(20), Blocks: 8})
		for o, ok := g.Tail(); ok; o, ok = g.Tail() { // the run's tail: pending windows run there
			ops = append(ops, o)
			g.Learn(o, answer(o))
			if o.Action == "v3_op_observe" {
				g.m.Observed(leaseRec(o.Volume, "expired"))
			}
		}
		notes, _ := g.Coverage()["notes"].([]string)
		if bad := pairsUnexercised(ops, notes); len(bad) != 0 {
			t.Fatalf("seed %d: pairs not exercised %v (notes %v)", seed, bad, notes)
		}
		for _, o := range ops {
			pairs += btoi(o.Action == "v3_op_restore" && o.Negative == "")
		}
		noted += len(notes)
	}
	if pairs == 0 {
		t.Fatal("no restore pair in the sweep")
	}
	t.Logf("%d pairs, %d noted not exercised", pairs, noted)
	// Red control for the check: a source stopped before any cross write.
	red := seq(op("v3_op_create", "g1"), op("v3_op_attach", "g1", "host", "0"), op("v3_op_write", "g1", "lbas", "1", "round", "r"),
		op("v3_op_snapshot", "g1", "name", "g1-s1"), op("v3_op_restore", "g1", "snapshot", "g1-s1", "target_volume", "g2"),
		op("v3_op_stop", "g1"), op("v3_op_attach", "g2", "host", "0"))
	if len(pairsUnexercised(red, []string{"pair not exercised: g1|g2: g1: stopped"})) == 0 {
		t.Fatal("red control: a source stopped before its window passed")
	}
}

// C2 in fault runs: a target whose first attach was unknown gets its
// inherited reads once a settled record places it held; a target killed
// after its attach keeps them owed and gets them once it serves again.
func TestInheritedReadsSurviveAnUnsettledOrKilledTarget(t *testing.T) {
	restored := func() *Generator {
		g, _ := NewGenerator(GenConfig{Seed: 1, Ops: 10, Pool: pool(4), Blocks: 8, Faults: 0.1, FaultProcesses: []string{"blockvolume:0"}})
		drive(g, op("v3_op_create", "g1"), op("v3_op_attach", "g1", "host", "0"), op("v3_op_write", "g1", "lbas", "3", "round", "r"),
			op("v3_op_snapshot", "g1", "name", "g1-s1"), op("v3_op_restore", "g1", "snapshot", "g1-s1", "target_volume", "g2"))
		return g
	}
	inheritedDue := func(g *Generator) bool {
		d := g.m.Due()
		return len(d) == 1 && d[0].Volume == "g2" && d[0].Forced == "inherited" && d[0].Params["lbas"] == "3"
	}
	// Unsettled first attach.
	g := restored()
	at := d0(g)
	g.Learn(at, tr.OutcomeUnknown)
	if inheritedDue(g) {
		t.Fatal("inherited reads due while the target is unsettled")
	}
	g.m.Observed(settledRec("g2", "held"))
	if !inheritedDue(g) {
		t.Fatalf("no inherited read after the attach settled held: %+v", g.m.Due())
	}
	// Killed after its attach (g2 is blockvolume:1, the pool's second name).
	g = restored()
	drive(g, d0(g))
	g.m.FaultRan(FaultKill, "blockvolume:1", tr.OutcomeOK)
	if d := g.m.Due(); len(d) > 0 && d[0].Volume == "g2" {
		t.Fatalf("an owed read of a killed target: %+v", d)
	}
	g.m.FaultRan(FaultRestart, "blockvolume:1", tr.OutcomeOK)
	g.m.Observed(healthyRec(nil))
	if !inheritedDue(g) {
		t.Fatalf("no inherited read once the target serves again: %+v", g.m.Due())
	}
}

// obsRec is a volume observation: its lease state and HEAD state.
func obsRec(vol, lease, head string) tr.HistoryRecord {
	return tr.HistoryRecord{Kind: tr.HistoryObserve, Verb: "observe", Volume: vol, Result: map[string]any{
		"head":      map[string]any{"state": head},
		"workspace": map[string]any{"exit": 0, "text": []any{"volume: " + vol, "lease state: " + lease}}}}
}

// M1 run 7: a volume taken out of the run (a snapshot answered unknown) is
// observed at the end (the sweep, and the tail), and its acknowledged content
// is read when that observation shows it still serving; never written.
func TestOutOfRunVolumesAreObservedAndReadIfServing(t *testing.T) {
	for name, c := range map[string]struct {
		lease, head string
		reads       bool
	}{
		"still serving": {"granted", "", true},
		"detached":      {"granted", "detached", false},
		"lease expired": {"expired", "", false},
	} {
		g, _ := NewGenerator(GenConfig{Seed: 1, Ops: 10, Pool: pool(3), Blocks: 8})
		drive(g, op("v3_op_create", "g1"), op("v3_op_attach", "g1", "host", "0"), op("v3_op_write", "g1", "lbas", "2 5", "round", "r"))
		g.Learn(op("v3_op_snapshot", "g1", "name", "g1-s1"), tr.OutcomeUnknown)
		if g.vols["g1"].state != stUnknown {
			t.Fatal("not out of the run")
		}
		observed := false
		for _, o := range g.m.Sweep() {
			observed = observed || (o.Volume == "g1" && o.Action == "v3_op_observe")
		}
		if !observed {
			t.Fatalf("%s: the sweep does not observe a volume out of the run", name)
		}
		var got []string
		for o, ok := g.Tail(); ok; o, ok = g.Tail() {
			if o.Volume == "g1" {
				got = append(got, strings.TrimPrefix(o.Action, "v3_op_")+" "+o.Params["lbas"])
				if o.Action == "v3_op_write" {
					t.Fatalf("%s: a write on a volume out of the run", name)
				}
			}
			g.Learn(o, answer(o))
			if o.Action == "v3_op_observe" && o.Volume == "g1" {
				g.m.Observed(obsRec("g1", c.lease, c.head))
			}
		}
		want := []string{"observe "}
		if c.reads {
			want = append(want, "read 2 5")
		}
		if len(got) < len(want) || !slices.Equal(got[:len(want)], want) {
			t.Fatalf("%s: tail on g1 %q, want first %q", name, got, want)
		}
		if !c.reads && slices.Contains(got, "read 2 5") {
			t.Fatalf("%s: read a volume that does not serve: %q", name, got)
		}
	}
}
