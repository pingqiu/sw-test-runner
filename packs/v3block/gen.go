package v3block

// The M1 seeded generator (runner spec S5): it proposes one typed operation at
// a time, each allowed by its precondition on a small lifecycle shadow of the
// volumes, and learns each outcome. It never judges: the checkers judge the
// history alone. Where the generator's shadow and the product disagree, C0's
// "refusal not expected" (or "declared refusal answered ok") shows it.
//
// One operation at a time, across several volumes on one host (two holder
// hosts for a move). A fixed share of operations are NEGATIVE: ones the
// operator pages say must be refused, issued with expect=refused. Names come
// from a finite pool declared in the cluster spec; each is used once (a
// deleted, delete-reserved or restore-bound name is burnt), and an exhausted
// pool is a normal end.

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
)

// GenVersion is recorded in the history's first record; a replay of another
// version is refused. m1-gen/2: the shadow follows the documented refusals
// (reserved and bound names, stale local roots, no attach after a stop) and
// proposes the negatives that follow from them.
const GenVersion = "m1-gen/2"

// MaxSnapshots is the per-volume snapshot limit. No operator page states it:
// it is the design invariant INV-SNAPK-HEAD-BOUND-001
// (docs/design/invariants.md:1092). An eleventh is still a negative
// operation; what the product answers to it (not a typed refusal, and HEAD
// advances) is a product result, not the generator's to settle.
const MaxSnapshots = 10

// wrongBlocks is the attach geometry of the "wrong geometry" negative. A
// volume is created at 16 MiB (v3_op_create's default) and attached with
// 4096 blocks of 4096 bytes (v3_op_attach's default); 2048 blocks disagrees
// with the volume record.
const wrongBlocks = "2048"

// Volume lifecycle states of the generator's shadow.
const (
	stUnborn   = "unborn"   // a pool name not yet created
	stCreated  = "created"  // created (or a restore target), no holder serving
	stServing  = "serving"  // attached: a holder serves it
	stStopped  = "stopped"  // its holder was stopped: HEAD is sealed for its epoch
	stMoved    = "moved"    // moved to the other host, not yet attached there
	stDeleted  = "deleted"  // deleted: the name is burnt
	stReserved = "reserved" // a delete was admitted (the name reserved) and then refused: never serves again, the name is burnt
	stBound    = "bound"    // a pool name bound by a refused restore: never created, the name is burnt
	stUnknown  = "unknown"  // an outcome the runner could not decide: no further op on it
)

// The limits a negative operation probes: each is a refusal an operator page
// documents (page:line on the m1 product branch; VL volume-lifecycle.md, SD
// standalone-deployment.md, OG operator-guide.md, all under docs/).
const (
	limCreateDeleted   = "create a deleted name"                               // VL:201-204
	limDeleteAttached  = "delete while attached"                               // VL:409-411, VL:527
	limDeleteSource    = "delete a source whose snapshot has a restore target" // VL:413-415, SD:653; side effect OG:193-196
	limDeleteSnapDep   = "delete a snapshot that has a restore target"         // SD:692
	limEleventhSnap    = "an eleventh snapshot"                                // design page only: docs/design/invariants.md:1092
	limRestoreExisting = "restore into an existing name"                       // VL:88-90, SD:672
	limRestoreDeleted  = "restore into a deleted name"                         // VL:90-91, VL:201-204
	limRestoreTarget   = "restore from a snapshot of a restore target"         // SD:688, SD:693 (side effect: the target name stays bound)
	limReleaseLive     = "release a live restore target"                       // SD:696
	limAttachOldHost   = "attach on the old host after a move"                 // VL:544
	limAttachDeleted   = "attach a deleted volume"                             // VL:545
	limAttachStale     = "attach on a stale local root"                        // VL:155-160, VL:190-192, VL:546
	limAttachGeometry  = "attach with the wrong geometry"                      // SD:682, OG:110-114
)

// burnt says a name can never be created or restored into again.
func burnt(state string) bool {
	return state == stDeleted || state == stReserved || state == stBound
}

// GenOp is one proposed operation: the typed action and its params.
type GenOp struct {
	No       int               // operation number in the run, from 1
	Action   string            // the typed action, v3_op_*
	Volume   string            // the volume it acts on
	Params   map[string]string // the action's params (volume included)
	Negative string            // the limit a negative operation probes; "" for an ordinary one
}

// GenConfig sets a run.
type GenConfig struct {
	Seed     uint64
	Ops      int            // the most operations to propose
	Pool     []string       // volume names, each used once
	Blocks   int            // lbas 0..Blocks-1 are used for writes and reads
	Negative float64        // share of operations that must be refused (default 0.1)
	Weights  map[string]int // relative share of TIME per operation; default DefaultWeights
}

// DefaultWeights are shares of run time per operation; the proposal
// probability of an operation is its weight divided by its cost, so slow
// operations (a delete waits out the lease horizon) are not starved and fast
// ones do not swamp the run.
var DefaultWeights = map[string]int{
	"v3_op_create": 6, "v3_op_attach": 10, "v3_op_write": 25, "v3_op_read": 20,
	"v3_op_snapshot": 8, "v3_op_snapshot_delete": 5, "v3_op_restore": 6,
	"v3_op_stop": 6, "v3_op_move": 6, "v3_op_delete": 8,
}

// OpCost is an operation's rough duration in seconds on one host: the time
// half of the weighting, and the operations-per-hour estimate.
var OpCost = map[string]float64{
	"v3_op_create": 2, "v3_op_attach": 8, "v3_op_write": 1, "v3_op_read": 1,
	"v3_op_snapshot": 2, "v3_op_snapshot_delete": 2, "v3_op_restore": 3,
	"v3_op_stop": 3, "v3_op_move": 20, "v3_op_delete": 65, "v3_op_observe": 1,
	"v3_op_release": 2, // proposed only as a negative (SD:696: a repair step, not part of the procedure)
}

type genVol struct {
	name     string
	state    string
	host     int               // the holder host it serves (or moved) on
	snaps    []string          // live snapshot names
	targets  map[string]string // snapshot name -> restore target volume (live)
	source   string            // for a restore target (or a bound name): its source volume
	fromSnap string            // for a restore target (or a bound name): the snapshot it was restored from
	written  bool              // a write was acknowledged (reads then have content)
	snapSeq  int
	// held[h]: a holder on host h served this volume once, so h has a local
	// root for it. stale[h]: HEAD's position has since advanced past that root
	// (a write or a snapshot acknowledged while another host served it). The
	// local root is per host and the runner never moves it aside, so an attach
	// on a host that held the volume earlier is against the operator rule
	// (VL:190-192) and, once stale, refused (VL:155-160).
	held, stale [2]bool
}

// Generator proposes operations; Learn feeds back each outcome.
type Generator struct {
	cfg    GenConfig
	rng    *rand.Rand
	vols   map[string]*genVol
	order  []string // pool order
	no     int
	ended  string
	cells  map[string]int // "action|state" -> count proposed
	counts map[string]int // action -> count proposed
}

// NewGenerator validates the config and starts a run.
func NewGenerator(cfg GenConfig) (*Generator, error) {
	if cfg.Ops <= 0 || len(cfg.Pool) == 0 || cfg.Blocks <= 0 {
		return nil, fmt.Errorf("generator: ops, a name pool and blocks are required")
	}
	if cfg.Negative == 0 {
		cfg.Negative = 0.1
	}
	if cfg.Negative < 0 || cfg.Negative > 0.5 {
		return nil, fmt.Errorf("generator: negative share %v outside 0..0.5", cfg.Negative)
	}
	if cfg.Weights == nil {
		cfg.Weights = DefaultWeights
	}
	for a, w := range cfg.Weights {
		if _, ok := OpCost[a]; !ok || w < 0 {
			return nil, fmt.Errorf("generator: weight for unknown operation %q or negative", a)
		}
	}
	g := &Generator{cfg: cfg, rng: rand.New(rand.NewPCG(cfg.Seed, cfg.Seed^0x9e3779b97f4a7c15)),
		vols: map[string]*genVol{}, cells: map[string]int{}, counts: map[string]int{}}
	seen := map[string]bool{}
	for _, n := range cfg.Pool {
		if n == "" || seen[n] || strings.ContainsAny(n, " \t") {
			return nil, fmt.Errorf("generator: pool name %q empty, repeated or not a token", n)
		}
		seen[n] = true
		g.vols[n] = &genVol{name: n, state: stUnborn, targets: map[string]string{}}
		g.order = append(g.order, n)
	}
	return g, nil
}

// Header is the history's first record: everything a replay must match.
func (g *Generator) Header() map[string]any {
	w := map[string]any{}
	for k, v := range g.cfg.Weights {
		w[k] = v
	}
	return map[string]any{"generator": GenVersion, "seed": strconv.FormatUint(g.cfg.Seed, 10),
		"ops": g.cfg.Ops, "pool": slices.Clone(g.cfg.Pool), "blocks": g.cfg.Blocks,
		"negative": g.cfg.Negative, "weights": w}
}

// Ended is why the run stopped proposing ("" while it goes on): "ops" when
// the operation budget is spent, "pool exhausted" when no operation is
// possible any more (every name used and nothing left to do with them).
func (g *Generator) Ended() string { return g.ended }

// candidate is one possible next operation with its precondition met.
type candidate struct {
	op     GenOp
	weight float64
}

// Next proposes the next operation, or false when the run ends.
func (g *Generator) Next() (GenOp, bool) {
	if g.ended != "" {
		return GenOp{}, false
	}
	if g.no >= g.cfg.Ops {
		g.ended = "ops"
		return GenOp{}, false
	}
	var cs []candidate
	if g.rng.Float64() < g.cfg.Negative {
		cs = g.negatives()
	}
	if len(cs) == 0 {
		cs = g.ordinary()
	}
	if len(cs) == 0 {
		g.ended = "pool exhausted"
		return GenOp{}, false
	}
	total := 0.0
	for _, c := range cs {
		total += c.weight
	}
	pick := g.rng.Float64() * total
	chosen := cs[len(cs)-1]
	for _, c := range cs {
		if pick < c.weight {
			chosen = c
			break
		}
		pick -= c.weight
	}
	g.no++
	chosen.op.No = g.no
	v := g.vols[chosen.op.Volume]
	g.cells[chosen.op.Action+"|"+v.state]++
	g.counts[chosen.op.Action]++
	return chosen.op, true
}

func (g *Generator) w(action string) float64 {
	return float64(g.cfg.Weights[action]) / OpCost[action]
}

func op(action, vol string, kv ...string) GenOp {
	p := map[string]string{"volume": vol}
	for i := 0; i+1 < len(kv); i += 2 {
		p[kv[i]] = kv[i+1]
	}
	return GenOp{Action: action, Volume: vol, Params: p}
}

// lbas picks 1..4 distinct lbas, space separated (parseLBAs' form).
func (g *Generator) lbas() string {
	n := 1 + g.rng.IntN(min(4, g.cfg.Blocks))
	set := map[int]bool{}
	var out []string
	for len(out) < n {
		b := g.rng.IntN(g.cfg.Blocks)
		if !set[b] {
			set[b] = true
			out = append(out, strconv.Itoa(b))
		}
	}
	return strings.Join(out, " ")
}

// firstName is the first pool name (in pool order) that ok accepts, or "".
func (g *Generator) firstName(ok func(v *genVol) bool) string {
	for _, n := range g.order {
		if ok(g.vols[n]) {
			return n
		}
	}
	return ""
}

// ordinary lists every operation whose precondition holds on the shadow and
// that the operator pages do not say is refused.
func (g *Generator) ordinary() []candidate {
	var cs []candidate
	add := func(o GenOp) {
		if w := g.w(o.Action); w > 0 {
			cs = append(cs, candidate{o, w})
		}
	}
	unborn := g.firstName(func(v *genVol) bool { return v.state == stUnborn })
	if unborn != "" {
		add(op("v3_op_create", unborn))
	}
	for _, n := range g.order {
		v := g.vols[n]
		switch v.state {
		case stCreated:
			if !v.held[v.host] {
				add(op("v3_op_attach", n, "host", strconv.Itoa(v.host)))
			}
			if len(v.targets) == 0 {
				add(op("v3_op_delete", n))
			}
		case stStopped, stReserved:
			// No attach after a stop: a sealed HEAD stays sealed for its own
			// epoch and is not re-granted (VL:336-337, VL:381); the way back is
			// a clean move to the same host, which v3_op_move cannot issue. A
			// reserved volume never serves again (OG:193-195); its delete is
			// rerun with the same request id once its dependents are gone.
			if len(v.targets) == 0 {
				add(op("v3_op_delete", n))
			}
		case stMoved:
			// Attach only where no earlier local root lies (VL:190-192). A
			// clean move to a host that never started is deleted without a
			// seal (VL:486-494).
			if !v.held[v.host] {
				add(op("v3_op_attach", n, "host", strconv.Itoa(v.host)))
			}
			add(op("v3_op_delete", n))
		case stServing:
			add(op("v3_op_write", n, "lbas", g.lbas(), "round", fmt.Sprintf("g%d", g.no+1)))
			if v.written {
				add(op("v3_op_read", n, "lbas", "written"))
			}
			if len(v.snaps) < MaxSnapshots {
				add(op("v3_op_snapshot", n, "name", fmt.Sprintf("%s-s%d", n, v.snapSeq+1)))
			}
			for _, s := range v.snaps {
				if v.targets[s] == "" {
					add(op("v3_op_snapshot_delete", n, "name", s))
					// A snapshot of a restore target names its source's chunks
					// and cannot be restored (SD:688, SD:693).
					if unborn != "" && v.source == "" {
						add(op("v3_op_restore", n, "snapshot", s, "target_volume", unborn))
					}
					break
				}
			}
			add(op("v3_op_stop", n))
			if v.source == "" && len(v.targets) == 0 {
				add(op("v3_op_move", n))
			}
		}
	}
	return cs
}

// negatives lists operations that must be refused now, each tagged with the
// limit it probes; they carry expect=refused. Each is proposed only where no
// other documented refusal would answer first, so the limit names the cause.
func (g *Generator) negatives() []candidate {
	var cs []candidate
	add := func(o GenOp, limit string) {
		o.Params["expect"] = "refused"
		o.Negative = limit
		cs = append(cs, candidate{o, 1})
	}
	unborn := g.firstName(func(v *genVol) bool { return v.state == stUnborn })
	for _, n := range g.order {
		v := g.vols[n]
		if v.state == stUnknown {
			continue
		}
		host := strconv.Itoa(v.host)
		switch v.state {
		case stDeleted:
			add(op("v3_op_create", n), limCreateDeleted)
			add(op("v3_op_attach", n, "host", host), limAttachDeleted)
		case stCreated:
			// The first start of a volume with a record (a created volume or
			// a restore target) with other geometry: refused before any local
			// store is created.
			if !v.held[v.host] {
				add(op("v3_op_attach", n, "host", host, "blocks", wrongBlocks), limAttachGeometry)
			}
		case stServing:
			add(op("v3_op_delete", n), limDeleteAttached)
			if len(v.snaps) >= MaxSnapshots {
				add(op("v3_op_snapshot", n, "name", fmt.Sprintf("%s-s%d", n, v.snapSeq+1)), limEleventhSnap)
			}
			if len(v.snaps) > 0 {
				s := v.snaps[0]
				if v.source == "" {
					// Into a name that exists (not this restore's own replay)
					// or one that is burnt. The target's binding is checked
					// before the source snapshot.
					if t := g.firstName(func(t *genVol) bool {
						return t.name != n && (t.state == stCreated || t.state == stServing || t.state == stStopped ||
							t.state == stMoved || t.state == stBound) && !(t.source == n && t.fromSnap == s)
					}); t != "" {
						add(op("v3_op_restore", n, "snapshot", s, "target_volume", t), limRestoreExisting)
					}
					if t := g.firstName(func(t *genVol) bool { return t.state == stDeleted || t.state == stReserved }); t != "" {
						add(op("v3_op_restore", n, "snapshot", s, "target_volume", t), limRestoreDeleted)
					}
				} else if unborn != "" {
					add(op("v3_op_restore", n, "snapshot", s, "target_volume", unborn), limRestoreTarget)
				}
			}
		case stStopped:
			if len(v.targets) > 0 {
				add(op("v3_op_delete", n), limDeleteSource)
			}
		case stMoved:
			// The host it moved from holds a root at HEAD's position, but the
			// master refuses its lease: the volume moved at a newer epoch.
			add(op("v3_op_attach", n, "host", strconv.Itoa(1-v.host)), limAttachOldHost)
			if v.held[v.host] && v.stale[v.host] {
				add(op("v3_op_attach", n, "host", host), limAttachStale)
			}
		}
		// Snapshots with a live restore target: their delete is refused, through
		// the holder while it serves, store direct otherwise; a live target is
		// not released.
		if v.state == stServing || v.state == stStopped || v.state == stReserved {
			for s, t := range v.targets {
				add(op("v3_op_snapshot_delete", n, "name", s), limDeleteSnapDep)
				if ts := g.vols[t].state; ts == stCreated || ts == stServing || ts == stStopped {
					add(op("v3_op_release", n, "target_volume", t), limReleaseLive)
				}
			}
		}
	}
	// Candidates came partly from map iteration: order them on a key that
	// identifies each one, so the same seed always picks the same operation.
	key := func(c candidate) string {
		p := c.op.Params
		return strings.Join([]string{c.op.Action, c.op.Volume, c.op.Negative, p["name"], p["snapshot"], p["target_volume"], p["host"], p["blocks"]}, "|")
	}
	slices.SortFunc(cs, func(a, b candidate) int { return strings.Compare(key(a), key(b)) })
	return cs
}

// Learn applies an operation's outcome (ok, refused, unknown) to the shadow.
// A refused ordinary operation changes nothing (C0 reports it as unexpected).
// A refused negative changes nothing either, except the side effects the
// operator pages document: a delete admitted and then refused for a snapshot
// dependent leaves the volume reserved (OG:193-195), and a restore refused
// for a snapshot of a restore target leaves the target name bound (SD:693).
// A negative answered ok, and any unknown outcome, take the volumes out of
// the run: the shadow cannot say what changed.
func (g *Generator) Learn(o GenOp, outcome string) {
	v := g.vols[o.Volume]
	if outcome == "unknown" && o.Action == "v3_op_read" {
		return // a read changes nothing, whatever its outcome
	}
	if outcome == "unknown" || (outcome == "ok" && o.Negative != "") {
		v.state = stUnknown
		if t := o.Params["target_volume"]; t != "" {
			g.vols[t].state = stUnknown
		}
		return
	}
	if o.Negative != "" {
		if outcome != "refused" {
			return
		}
		switch o.Negative {
		case limDeleteSource:
			// The reservation commits before the dependent check refuses: the
			// volume cannot serve again and its name is burnt. (A refusal for a
			// lease still inside its horizon reserves nothing; the shadow takes
			// the stricter reading, under which every later proposal on this
			// volume is refused or ok either way.)
			v.state = stReserved
		case limRestoreTarget:
			t := g.vols[o.Params["target_volume"]]
			t.state, t.source, t.fromSnap = stBound, o.Volume, o.Params["snapshot"]
		}
		return
	}
	if outcome != "ok" {
		return
	}
	switch o.Action {
	case "v3_op_create":
		v.state = stCreated
	case "v3_op_attach":
		v.state = stServing
		v.held[v.host] = true
	case "v3_op_write", "v3_op_snapshot":
		if o.Action == "v3_op_write" {
			v.written = true
		} else {
			v.snapSeq++
			v.snaps = append(v.snaps, o.Params["name"])
		}
		// HEAD's position advances past every other host's earlier root.
		for h := range v.held {
			if h != v.host && v.held[h] {
				v.stale[h] = true
			}
		}
	case "v3_op_snapshot_delete":
		v.snaps = slices.DeleteFunc(v.snaps, func(s string) bool { return s == o.Params["name"] })
	case "v3_op_restore":
		t := g.vols[o.Params["target_volume"]]
		t.state, t.source, t.fromSnap, t.host, t.written = stCreated, o.Volume, o.Params["snapshot"], v.host, v.written
		v.targets[o.Params["snapshot"]] = t.name
	case "v3_op_stop":
		v.state = stStopped
	case "v3_op_move":
		v.state, v.host = stMoved, 1-v.host
	case "v3_op_delete":
		v.state = stDeleted
		if v.source != "" {
			src := g.vols[v.source]
			for s, t := range src.targets {
				if t == v.name {
					delete(src.targets, s)
				}
			}
		}
	}
}

// Coverage is the bundle's coverage report: proposals per action, per
// (action, state) cell, and the cells that were possible in principle but
// never reached.
func (g *Generator) Coverage() map[string]any {
	// Ordinary and negative cells alike, by the state of the operation's volume.
	all := map[string][]string{
		"v3_op_create": {stUnborn, stDeleted}, "v3_op_attach": {stCreated, stMoved, stDeleted},
		"v3_op_write": {stServing}, "v3_op_read": {stServing}, "v3_op_snapshot": {stServing},
		"v3_op_snapshot_delete": {stServing, stStopped}, "v3_op_restore": {stServing}, "v3_op_stop": {stServing},
		"v3_op_move": {stServing}, "v3_op_delete": {stCreated, stStopped, stMoved, stReserved, stServing},
		"v3_op_release": {stServing, stStopped},
	}
	var missed []string
	for a, sts := range all {
		for _, s := range sts {
			if g.cells[a+"|"+s] == 0 {
				missed = append(missed, a+"|"+s)
			}
		}
	}
	slices.Sort(missed)
	cells := map[string]int{}
	for k, v := range g.cells {
		cells[k] = v
	}
	counts := map[string]int{}
	for k, v := range g.counts {
		counts[k] = v
	}
	est := 0.0
	for a, n := range g.counts {
		est += OpCost[a] * float64(n)
	}
	perHour := 0.0
	if est > 0 {
		perHour = float64(g.no) * 3600 / est
	}
	return map[string]any{"proposed": g.no, "ended": g.ended, "actions": counts, "cells": cells,
		"cells_never_reached": missed, "estimated_ops_per_hour": int(perHour)}
}
