package randomrun

// The seeded generator: it proposes one operation at a time, each allowed by
// the model's precondition, and learns each outcome. A fixed share of
// operations are NEGATIVE: ones the product must refuse (the model lists
// them per state). Names come from a finite pool; the model burns a name it
// can never use again, and an exhausted pool (no operation possible any more)
// is a normal end.
//
// The proposal probability of an operation is its weight divided by its
// cost, so weights are shares of run TIME: slow operations are not starved
// and fast ones do not swamp the run.

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
)

// Config sets a run.
type Config struct {
	Seed     uint64
	Ops      int            // the most operations to propose
	Pool     []string       // subject names, each a resource the model uses up
	Negative float64        // share of operations that must be refused (default 0.1)
	Weights  map[string]int // relative share of TIME per operation; default the table's

	// Process faults (faults.go); off when Faults is 0.
	Faults         float64  // chance of a kill after each operation while nothing is down
	FaultProcesses []string // the processes the schedule may kill
	RestartWithin  int      // a kill's restart comes within this many operations (default DefaultRestartWithin)
}

// Generator proposes operations; Learn feeds back each outcome.
type Generator struct {
	cfg    Config
	m      Model
	costs  map[string]float64
	rng    *rand.Rand
	no     int
	ended  string
	cells  map[string]int // "action|state" -> count proposed
	counts map[string]int // action -> count proposed
	down   string         // the process a fault holds down now ("" for none)
	fcells map[string]int // fault cells: "kill|p", "restart|p", "while_down|action"
}

// New validates the config and starts a run of model m.
func New(cfg Config, m Model) (*Generator, error) {
	if cfg.Ops <= 0 || len(cfg.Pool) == 0 {
		return nil, fmt.Errorf("generator: ops and a name pool are required")
	}
	if cfg.Negative == 0 {
		cfg.Negative = 0.1
	}
	if cfg.Negative < 0 || cfg.Negative > 0.5 {
		return nil, fmt.Errorf("generator: negative share %v outside 0..0.5", cfg.Negative)
	}
	table := m.Operations()
	costs := table.Costs()
	if cfg.Weights == nil {
		cfg.Weights = table.DefaultWeights()
	}
	for a, w := range cfg.Weights {
		if _, ok := costs[a]; !ok || w < 0 {
			return nil, fmt.Errorf("generator: weight for unknown operation %q or negative", a)
		}
	}
	if cfg.Faults != 0 {
		if cfg.RestartWithin == 0 {
			cfg.RestartWithin = DefaultRestartWithin
		}
		cfg.FaultProcesses = slices.Clone(cfg.FaultProcesses)
		slices.Sort(cfg.FaultProcesses)
		if _, err := NewFaultSchedule(cfg.Seed, cfg.Ops, cfg.FaultProcesses, cfg.Faults, cfg.RestartWithin, m.ValidProcess); err != nil {
			return nil, fmt.Errorf("generator: %w", err)
		}
	}
	g := &Generator{cfg: cfg, m: m, costs: costs, rng: rand.New(rand.NewPCG(cfg.Seed, cfg.Seed^0x9e3779b97f4a7c15)),
		cells: map[string]int{}, counts: map[string]int{}, fcells: map[string]int{}}
	seen := map[string]bool{}
	for _, n := range cfg.Pool {
		if n == "" || seen[n] || strings.ContainsAny(n, " \t") {
			return nil, fmt.Errorf("generator: pool name %q empty, repeated or not a token", n)
		}
		seen[n] = true
	}
	if err := m.Begin(slices.Clone(cfg.Pool), g.Faulty()); err != nil {
		return nil, fmt.Errorf("generator: %w", err)
	}
	return g, nil
}

// Header is the history's first record: everything a replay must match.
func (g *Generator) Header() map[string]any {
	w := map[string]any{}
	for k, v := range g.cfg.Weights {
		w[k] = v
	}
	h := map[string]any{"generator": g.m.Version(false), "seed": strconv.FormatUint(g.cfg.Seed, 10),
		"ops": g.cfg.Ops, "pool": slices.Clone(g.cfg.Pool), "negative": g.cfg.Negative, "weights": w}
	g.m.Header(h)
	if g.Faulty() {
		h["generator"] = g.m.Version(true)
		h["faults"] = g.cfg.Faults
		h["fault_processes"] = slices.Clone(g.cfg.FaultProcesses)
		h["fault_restart_within"] = g.cfg.RestartWithin
	}
	return h
}

// Faulty says a fault schedule is active.
func (g *Generator) Faulty() bool { return g.cfg.Faults != 0 }

// Proposed is the number of operations proposed so far.
func (g *Generator) Proposed() int { return g.no }

// Schedule is the run's fault schedule (empty when faults are off).
func (g *Generator) Schedule() FaultSchedule {
	if !g.Faulty() {
		return FaultSchedule{Seed: g.cfg.Seed, Events: []FaultEvent{}}
	}
	s, _ := NewFaultSchedule(g.cfg.Seed, g.cfg.Ops, g.cfg.FaultProcesses, g.cfg.Faults, g.cfg.RestartWithin, g.m.ValidProcess)
	return s
}

// Fault tells the generator a fault event ran: a kill puts the process down,
// a restart brings it back. The model's state does not change (a healthy
// point requires every subject back as it was); operations proposed while a
// process is down are counted as fault cells.
func (g *Generator) Fault(action, process string) {
	g.fcells[action+"|"+process]++
	if action == FaultKill {
		g.down = process
	} else if g.down == process {
		g.down = ""
	}
}

// Resolved tells the model that a subject's unknown operations were resolved
// by observation at a healthy point.
func (g *Generator) Resolved(subject string) { g.m.Resolved(subject) }

// Learn applies an operation's outcome to the model's state.
func (g *Generator) Learn(o Op, outcome string) { g.m.Learn(o, outcome) }

// Ended is why the run stopped proposing ("" while it goes on): "ops" when
// the operation budget is spent, "pool exhausted" when no operation is
// possible any more (every name used and nothing left to do with them).
func (g *Generator) Ended() string { return g.ended }

// Candidate is one possible next operation with its weight.
type Candidate struct {
	Op     Op
	Weight float64
}

// Candidates lists the possible next operations: the model's negatives (each
// weight 1), or its ordinary operations weighted by time (weight/cost; an
// operation of weight 0 is left out).
func (g *Generator) Candidates(negative bool) []Candidate {
	var cs []Candidate
	if negative {
		for _, o := range g.m.Negatives() {
			cs = append(cs, Candidate{o, 1})
		}
		return cs
	}
	for _, o := range g.m.Ordinary(g.rng, g.no+1) {
		if w := g.weight(o.Action); w > 0 {
			cs = append(cs, Candidate{o, w})
		}
	}
	return cs
}

func (g *Generator) weight(action string) float64 {
	return float64(g.cfg.Weights[action]) / g.costs[action]
}

// Next proposes the next operation, or false when the run ends.
func (g *Generator) Next() (Op, bool) {
	if g.ended != "" {
		return Op{}, false
	}
	if g.no >= g.cfg.Ops {
		g.ended = "ops"
		return Op{}, false
	}
	var cs []Candidate
	if g.rng.Float64() < g.cfg.Negative {
		cs = g.Candidates(true)
	}
	if len(cs) == 0 {
		cs = g.Candidates(false)
	}
	if len(cs) == 0 {
		g.ended = "pool exhausted"
		return Op{}, false
	}
	total := 0.0
	for _, c := range cs {
		total += c.Weight
	}
	pick := g.rng.Float64() * total
	chosen := cs[len(cs)-1]
	for _, c := range cs {
		if pick < c.Weight {
			chosen = c
			break
		}
		pick -= c.Weight
	}
	g.no++
	chosen.Op.No = g.no
	g.cells[chosen.Op.Action+"|"+g.m.State(chosen.Op.Volume)]++
	g.counts[chosen.Op.Action]++
	if g.down != "" {
		g.fcells["while_down|"+chosen.Op.Action]++
	}
	return chosen.Op, true
}

// Coverage is the bundle's coverage report: proposals per action, per
// (action, state) cell, and the cells that were possible in principle (the
// model's Cells, and in a fault run each process killed and restarted and
// each action proposed while a process was down) but never reached.
func (g *Generator) Coverage() map[string]any {
	all := g.m.Cells()
	var missed []string
	for a, sts := range all {
		for _, s := range sts {
			if g.cells[a+"|"+s] == 0 {
				missed = append(missed, a+"|"+s)
			}
		}
	}
	var fcells map[string]int
	if g.Faulty() {
		fcells = map[string]int{}
		for k, v := range g.fcells {
			fcells[k] = v
		}
		var want []string
		for _, p := range g.cfg.FaultProcesses {
			want = append(want, FaultKill+"|"+p, FaultRestart+"|"+p)
		}
		for a := range all {
			want = append(want, "while_down|"+a)
		}
		for _, c := range want {
			if g.fcells[c] == 0 {
				missed = append(missed, c)
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
		est += g.costs[a] * float64(n)
	}
	perHour := 0.0
	if est > 0 {
		perHour = float64(g.no) * 3600 / est
	}
	cov := map[string]any{"proposed": g.no, "ended": g.ended, "actions": counts, "cells": cells,
		"cells_never_reached": missed, "estimated_ops_per_hour": int(perHour)}
	if fcells != nil {
		cov["fault_cells"] = fcells
	}
	return cov
}
