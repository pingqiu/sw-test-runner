package randomrun

// The declared workload loop (tier 1): a fixed cycle of typed actions over a
// pool of subjects, read from a product's description (loops.json), with no
// model. The order is fixed, so each step's precondition holds by
// construction. It runs through the same handlers, the same Step and the same
// fault schedule as the random run (looprun.go).
//
// A loop declaration:
//
//	{"comment": "...",
//	 "lbas": 4,                       // random lbas drawn per subject per iteration: {lbas}
//	 "setup":    [step, ...],         // per subject, in pool order
//	 "cycle":    [step, ...],         // per iteration, per subject, in order
//	 "sweep":    step,                // after the cycle: read back what was written ({written})
//	 "teardown": [step, ...]}         // per subject, in pool order
//
// A step:
//
//	{"action": "v3_op_write",                         // a registered action
//	 "params": {"volume": "{subject}", "lbas": "{lbas}", "round": "i{iteration}"},
//	 "count": 1,          // repeats per iteration (cycle only; default 1)
//	 "every": 1,          // runs at iterations every, 2*every, ... (cycle only; default 1)
//	 "back": 0,           // {iteration} is the iteration back iterations earlier; the step
//	                      // runs only when that iteration is 1 or later (cycle only, a multiple of every)
//	 "writes_lbas": false // its {lbas} count as written for the sweep (cycle only)
//	}
//
// The fields a params template may name: {subject} everywhere; in the cycle
// also {iteration} (from 1), {repeat} (0..count-1) and {lbas} (only when the
// loop declares lbas: the iteration's draw for the subject, the same for every
// step of that iteration and subject); in the sweep {written} (every lba of a
// writes_lbas step whose outcome was not refused, sorted). Any other field, an
// unregistered action, or an unknown key is refused when the file is loaded.
// After the sweep's read of each subject that has written lbas, the run
// observes every subject.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// LoopDriverVersion is recorded in a loop history's header; a replay or a
// check of another version is refused.
const LoopDriverVersion = "loop/1"

// LoopFile is a product's loops.json.
type LoopFile struct {
	Product string          `json:"product"`
	Comment string          `json:"comment,omitempty"`
	Loops   map[string]Loop `json:"loops"`
}

// Loop is one declared workload loop.
type Loop struct {
	Comment  string     `json:"comment,omitempty"`
	LBAs     int        `json:"lbas,omitempty"`
	Setup    []LoopStep `json:"setup"`
	Cycle    []LoopStep `json:"cycle"`
	Sweep    *LoopStep  `json:"sweep,omitempty"`
	Teardown []LoopStep `json:"teardown"`
}

// LoopStep is one declared step.
type LoopStep struct {
	Action     string            `json:"action"`
	Params     map[string]string `json:"params"`
	Count      int               `json:"count,omitempty"`
	Every      int               `json:"every,omitempty"`
	Back       int               `json:"back,omitempty"`
	WritesLBAs bool              `json:"writes_lbas,omitempty"`
}

// Loop phases, in their order in a history.
const (
	PhaseSetup    = "setup"
	PhaseCycle    = "cycle"
	PhaseSweep    = "sweep"
	PhaseTeardown = "teardown"
)

var loopPhases = []string{PhaseSetup, PhaseCycle, PhaseSweep, PhaseTeardown}

var templateField = regexp.MustCompile(`\{([^{}]*)\}`)

// ParseLoops reads and checks a loops file. known says whether an action is
// registered (has a handler).
func ParseLoops(b []byte, known func(string) bool) (LoopFile, error) {
	var f LoopFile
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(&f); err != nil {
		return f, fmt.Errorf("loops: %w", err)
	}
	if f.Product == "" || len(f.Loops) == 0 {
		return f, fmt.Errorf("loops: product and at least one loop are required")
	}
	names := make([]string, 0, len(f.Loops))
	for n := range f.Loops {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if err := f.Loops[n].Check(known); err != nil {
			return f, fmt.Errorf("loops: %s: %w", n, err)
		}
	}
	return f, nil
}

// Check refuses a declaration that cannot run: an unregistered action, a
// template field the step's place does not offer, a bad count, cadence or
// lag, a writes_lbas step without {lbas}, writes without a sweep reading
// {written}.
func (l Loop) Check(known func(string) bool) error {
	if l.LBAs < 0 {
		return fmt.Errorf("lbas %d", l.LBAs)
	}
	if len(l.Cycle) == 0 {
		return fmt.Errorf("an empty cycle")
	}
	check := func(where string, s LoopStep, fields map[string]bool, cycle bool) error {
		if s.Action == "" || !known(s.Action) {
			return fmt.Errorf("%s: action %q is not registered", where, s.Action)
		}
		if !cycle && (s.Count != 0 || s.Every != 0 || s.Back != 0 || s.WritesLBAs) {
			return fmt.Errorf("%s: count, every, back and writes_lbas belong to cycle steps", where)
		}
		if s.Count < 0 || s.Every < 0 || s.Back < 0 {
			return fmt.Errorf("%s: count %d, every %d, back %d", where, s.Count, s.Every, s.Back)
		}
		if s.Back%max(s.Every, 1) != 0 {
			return fmt.Errorf("%s: back %d is not a multiple of every %d", where, s.Back, max(s.Every, 1))
		}
		usesLBAs := false
		keys := make([]string, 0, len(s.Params))
		for k := range s.Params {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v := s.Params[k]
			if k == "" {
				return fmt.Errorf("%s: an empty param name", where)
			}
			rest := templateField.ReplaceAllString(v, "")
			if strings.ContainsAny(rest, "{}") {
				return fmt.Errorf("%s: param %s %q: an unmatched brace", where, k, v)
			}
			for _, m := range templateField.FindAllStringSubmatch(v, -1) {
				if !fields[m[1]] {
					return fmt.Errorf("%s: param %s names the unknown field {%s}", where, k, m[1])
				}
				usesLBAs = usesLBAs || m[1] == "lbas"
			}
		}
		if s.WritesLBAs && !usesLBAs {
			return fmt.Errorf("%s: writes_lbas without {lbas} in its params", where)
		}
		return nil
	}
	plain := map[string]bool{"subject": true}
	cyc := map[string]bool{"subject": true, "iteration": true, "repeat": true}
	if l.LBAs > 0 {
		cyc["lbas"] = true
	}
	writes := false
	for i, s := range l.Setup {
		if err := check(fmt.Sprintf("setup %d", i), s, plain, false); err != nil {
			return err
		}
	}
	for i, s := range l.Cycle {
		if err := check(fmt.Sprintf("cycle %d", i), s, cyc, true); err != nil {
			return err
		}
		writes = writes || s.WritesLBAs
	}
	for i, s := range l.Teardown {
		if err := check(fmt.Sprintf("teardown %d", i), s, plain, false); err != nil {
			return err
		}
	}
	if l.Sweep != nil {
		if err := check("sweep", *l.Sweep, map[string]bool{"subject": true, "written": true}, false); err != nil {
			return err
		}
		reads := false
		for _, v := range l.Sweep.Params {
			reads = reads || strings.Contains(v, "{written}")
		}
		if !reads {
			return fmt.Errorf("sweep: no param names {written}")
		}
	} else if writes {
		return fmt.Errorf("a cycle step writes lbas but the loop has no sweep to read them back")
	}
	return nil
}

// Hash is the declaration's identity: sha256 of its JSON encoding.
func (l Loop) Hash() string {
	b, _ := json.Marshal(l)
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// record is the declaration as a JSON object, for the header.
func (l Loop) record() map[string]any {
	b, _ := json.Marshal(l)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

// expand fills a step's params template.
func (s LoopStep) expand(fields map[string]string) map[string]string {
	p := make(map[string]string, len(s.Params))
	for k, v := range s.Params {
		p[k] = templateField.ReplaceAllStringFunc(v, func(m string) string { return fields[m[1:len(m)-1]] })
	}
	return p
}

// runsAt says whether a cycle step runs at iteration i, and the iteration its
// {iteration} names.
func (s LoopStep) runsAt(i int) (bool, int) {
	if i%max(s.Every, 1) != 0 || i-s.Back < 1 {
		return false, 0
	}
	return true, i - s.Back
}

// loopPlan expands a declaration over a pool: a pure function of the
// declaration, the pool, the lba range and the seed. The run and the checker
// both expand through it.
type loopPlan struct {
	loop   Loop
	pool   []string
	blocks int
	rng    *rand.Rand
}

func newLoopPlan(l Loop, pool []string, blocks int, seed uint64) (*loopPlan, error) {
	if len(pool) == 0 {
		return nil, fmt.Errorf("a subject pool is required")
	}
	seen := map[string]bool{}
	for _, n := range pool {
		if n == "" || seen[n] || strings.ContainsAny(n, " \t") {
			return nil, fmt.Errorf("pool name %q empty, repeated or not a token", n)
		}
		seen[n] = true
	}
	if l.LBAs > 0 && blocks < l.LBAs {
		return nil, fmt.Errorf("blocks %d: the loop draws %d lbas per iteration", blocks, l.LBAs)
	}
	// Its own stream, distinct from the generator's and the fault schedule's.
	return &loopPlan{loop: l, pool: slices.Clone(pool), blocks: blocks, rng: rand.New(rand.NewPCG(seed^0x6c6f6f7000000000, seed+0x51ed270b27f3c2a1))}, nil
}

func stepOp(s LoopStep, subject string, fields map[string]string) Op {
	fields["subject"] = subject
	return Op{Action: s.Action, Volume: subject, Params: s.expand(fields)}
}

// once expands setup or teardown steps over the pool.
func (p *loopPlan) once(steps []LoopStep) []Op {
	var out []Op
	for _, subj := range p.pool {
		for _, s := range steps {
			out = append(out, stepOp(s, subj, map[string]string{}))
		}
	}
	return out
}

// planned is a cycle operation with the lbas it writes (nil unless writes_lbas).
type planned struct {
	Op     Op
	Writes []string
}

// iteration expands iteration i (from 1): per subject in pool order, the
// lbas draw (when declared), then each step that runs at i, count times.
func (p *loopPlan) iteration(i int) []planned {
	var out []planned
	for _, subj := range p.pool {
		var lbas []string
		if p.loop.LBAs > 0 {
			pick := p.rng.Perm(p.blocks)[:p.loop.LBAs]
			slices.Sort(pick)
			for _, b := range pick {
				lbas = append(lbas, strconv.Itoa(b))
			}
		}
		for _, s := range p.loop.Cycle {
			ok, it := s.runsAt(i)
			if !ok {
				continue
			}
			for r := 0; r < max(s.Count, 1); r++ {
				o := stepOp(s, subj, map[string]string{"iteration": strconv.Itoa(it), "repeat": strconv.Itoa(r), "lbas": strings.Join(lbas, " ")})
				pl := planned{Op: o}
				if s.WritesLBAs {
					pl.Writes = lbas
				}
				out = append(out, pl)
			}
		}
	}
	return out
}

// cycleOps counts the cycle operations of iterations 1..n, without drawing.
func (l Loop) cycleOps(pool, n int) int {
	total := 0
	for i := 1; i <= n; i++ {
		for _, s := range l.Cycle {
			if ok, _ := s.runsAt(i); ok {
				total += pool * max(s.Count, 1)
			}
		}
	}
	return total
}

// sweep expands the sweep: per subject in pool order, a read of its written
// lbas (none: no read), then an observation of every subject.
func (p *loopPlan) sweep(written map[string]map[int]bool, observe func(string) Op) []Op {
	var out []Op
	if p.loop.Sweep != nil {
		for _, subj := range p.pool {
			if len(written[subj]) == 0 {
				continue
			}
			lbas := make([]string, 0, len(written[subj]))
			for _, b := range sortedInts(written[subj]) {
				lbas = append(lbas, strconv.Itoa(b))
			}
			out = append(out, stepOp(*p.loop.Sweep, subj, map[string]string{"written": strings.Join(lbas, " ")}))
		}
	}
	for _, subj := range p.pool {
		out = append(out, observe(subj))
	}
	return out
}

func sortedInts(set map[int]bool) []int {
	out := make([]int, 0, len(set))
	for b := range set {
		out = append(out, b)
	}
	slices.Sort(out)
	return out
}

// addWritten notes the lbas of a write that was not refused.
func addWritten(written map[string]map[int]bool, subject string, lbas []string) {
	for _, s := range lbas {
		n, err := strconv.Atoi(s)
		if err != nil {
			continue
		}
		if written[subject] == nil {
			written[subject] = map[int]bool{}
		}
		written[subject][n] = true
	}
}
