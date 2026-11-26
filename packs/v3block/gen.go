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
// deleted name is burnt), and an exhausted pool is a normal end.

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
)

// GenVersion is recorded in the history's first record; a replay of another
// version is refused.
const GenVersion = "m1-gen/1"

// MaxSnapshots is the per-volume snapshot limit the operator pages state; an
// eleventh is a negative operation.
const MaxSnapshots = 10

// Volume lifecycle states of the generator's shadow.
const (
	stUnborn  = "unborn"  // a pool name not yet created
	stCreated = "created" // created (or a restore target), no holder serving
	stServing = "serving" // attached: a holder serves it
	stStopped = "stopped" // its holder was stopped
	stMoved   = "moved"   // moved to the other host, not yet attached there
	stDeleted = "deleted" // deleted: the name is burnt
	stUnknown = "unknown" // an outcome the runner could not decide: no further op on it
)

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
}

type genVol struct {
	name    string
	state   string
	host    int               // the holder host it serves (or moved) on
	snaps   []string          // live snapshot names
	targets map[string]string // snapshot name -> restore target volume (live)
	source  string            // for a restore target: its source volume
	written bool              // a write was acknowledged (reads then have content)
	snapSeq int
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

// ordinary lists every operation whose precondition holds on the shadow.
func (g *Generator) ordinary() []candidate {
	var cs []candidate
	add := func(o GenOp) {
		if w := g.w(o.Action); w > 0 {
			cs = append(cs, candidate{o, w})
		}
	}
	unborn := ""
	for _, n := range g.order {
		if g.vols[n].state == stUnborn {
			unborn = n
			break
		}
	}
	if unborn != "" {
		add(op("v3_op_create", unborn))
	}
	for _, n := range g.order {
		v := g.vols[n]
		switch v.state {
		case stCreated, stStopped:
			add(op("v3_op_attach", n, "host", strconv.Itoa(v.host)))
			if len(v.targets) == 0 {
				add(op("v3_op_delete", n))
			}
		case stMoved:
			add(op("v3_op_attach", n, "host", strconv.Itoa(v.host)))
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
					if unborn != "" {
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
// limit it probes; they carry expect=refused.
func (g *Generator) negatives() []candidate {
	var cs []candidate
	add := func(o GenOp, limit string) {
		o.Params["expect"] = "refused"
		o.Negative = limit
		cs = append(cs, candidate{o, 1})
	}
	for _, n := range g.order {
		v := g.vols[n]
		switch v.state {
		case stDeleted:
			add(op("v3_op_create", n), "create a deleted name")
		case stServing:
			add(op("v3_op_delete", n), "delete while attached")
			if len(v.snaps) >= MaxSnapshots {
				add(op("v3_op_snapshot", n, "name", fmt.Sprintf("%s-s%d", n, v.snapSeq+1)), "an eleventh snapshot")
			}
			for s, t := range v.targets {
				if t != "" {
					add(op("v3_op_snapshot_delete", n, "name", s), "delete a snapshot that has a restore target")
				}
			}
		}
	}
	// Candidates came partly from map iteration: order them on a key that
	// identifies each one, so the same seed always picks the same operation.
	key := func(c candidate) string {
		return c.op.Action + "|" + c.op.Volume + "|" + c.op.Negative + "|" + c.op.Params["name"]
	}
	slices.SortFunc(cs, func(a, b candidate) int { return strings.Compare(key(a), key(b)) })
	return cs
}

// Learn applies an operation's outcome (ok, refused, unknown) to the shadow.
// A refused negative changes nothing; a refused ordinary operation changes
// nothing either (C0 reports it as unexpected); unknown takes the volume out
// of the run.
func (g *Generator) Learn(o GenOp, outcome string) {
	v := g.vols[o.Volume]
	if outcome == "unknown" && o.Action == "v3_op_read" {
		return // a read changes nothing, whatever its outcome
	}
	if outcome == "unknown" {
		v.state = stUnknown
		if t := o.Params["target_volume"]; t != "" {
			g.vols[t].state = stUnknown
		}
		return
	}
	if outcome != "ok" || o.Negative != "" {
		return
	}
	switch o.Action {
	case "v3_op_create":
		v.state = stCreated
	case "v3_op_attach":
		v.state = stServing
	case "v3_op_write":
		v.written = true
	case "v3_op_snapshot":
		v.snapSeq++
		v.snaps = append(v.snaps, o.Params["name"])
	case "v3_op_snapshot_delete":
		v.snaps = slices.DeleteFunc(v.snaps, func(s string) bool { return s == o.Params["name"] })
	case "v3_op_restore":
		t := g.vols[o.Params["target_volume"]]
		t.state, t.source, t.host, t.written = stCreated, o.Volume, v.host, v.written
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
	all := map[string][]string{
		"v3_op_create": {stUnborn, stDeleted}, "v3_op_attach": {stCreated, stStopped, stMoved},
		"v3_op_write": {stServing}, "v3_op_read": {stServing}, "v3_op_snapshot": {stServing},
		"v3_op_snapshot_delete": {stServing}, "v3_op_restore": {stServing}, "v3_op_stop": {stServing},
		"v3_op_move": {stServing}, "v3_op_delete": {stCreated, stStopped, stServing},
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
