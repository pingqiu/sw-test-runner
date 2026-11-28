package randomrun

// The seeded fault schedule: which process is killed after which operation,
// and when it is started again. It is a pure function of (seed, ops,
// processes, rate, restart bound): no I/O, no clock, so the same inputs
// always give the same schedule and a replay can re-derive or check it.
//
// Rules of a schedule (CheckFaultSchedule states them as a checker):
//   - an event runs after operation after_op (1..ops) has completed;
//   - every kill is followed by the restart of the same process within
//     `within` operations, and no later than after operation ops;
//   - at most one process is down at a time;
//   - a process is killed only when it runs, restarted only when it is down;
//   - a short master outage (one event: kill of the master, writes and
//     single looks, restart; see faultRun) comes only while nothing is down,
//     and never inside a kill's interval;
//   - an IN-OPERATION fault (AddInOpFaults) is armed after operation after_op
//     and fires inside the first later operation of its target action: a
//     kill_during kills the holder of that operation's subject at a seeded
//     fraction of the action's measured duration, beside the operation (it
//     is a kill: its restart follows within `within`, one down at a time);
//     a store_pause pauses the store process around that operation, with a
//     bounded continue, and comes only while nothing is down.
//
// Which process names exist is the product's: valid says it.

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"strconv"
)

// DefaultRestartWithin is the default bound, in operations, between a kill
// and the restart of the same process.
const DefaultRestartWithin = 20

// Fault event actions.
const (
	FaultKill    = "kill"
	FaultRestart = "restart"
	// FaultMasterOutage is one short master outage: the master killed and
	// restarted within the product's outage bound, with one write on every
	// serving subject and single health looks in between.
	FaultMasterOutage = "master_outage"
	// FaultKillDuring is a kill timed INTO an operation of action Target: the
	// holder of its subject, at Permille/1000 of the action's measured
	// duration after the operation began. Its restart is an ordinary restart
	// event of the same scheduled process.
	FaultKillDuring = "kill_during"
	// FaultStorePause pauses the store process Process around the first
	// operation of action Target after it is armed: paused before the
	// operation, continued after it (and by a detached bounded continue
	// regardless), then a healthy point and the model's follow-ups.
	FaultStorePause = "store_pause"
)

// FaultEvent is one scheduled process fault.
type FaultEvent struct {
	AfterOp int    `json:"after_op"`
	Action  string `json:"action"` // FaultKill, FaultRestart, FaultMasterOutage, FaultKillDuring or FaultStorePause
	Process string `json:"process"`
	// Target and Permille are an in-operation fault's: the action it fires
	// in, and (kill_during) where in its measured duration, in thousandths.
	Target   string `json:"target,omitempty"`
	Permille int    `json:"permille,omitempty"`
}

// FaultSchedule is the run's fault schedule, events in execution order.
type FaultSchedule struct {
	Seed   uint64
	Events []FaultEvent
}

// NewFaultSchedule derives the schedule. rate is the chance, after each
// operation while no process is down, that a process is killed; its restart
// comes 1..within operations later, capped at ops. A rate of 0 gives an
// empty schedule. Every process is reached BY CONSTRUCTION, not by luck over
// seeds: the kills take the sorted processes in turn (round-robin), and the
// first len(processes) kills are forced, the i-th (from 1) no earlier than
// after operation i*ops/(len(processes)+1) (or the first operation after the
// restart before it), so each process is scheduled at least once whenever
// the operations leave room.
func NewFaultSchedule(seed uint64, ops int, processes []string, rate float64, within int, valid func(string) bool) (FaultSchedule, error) {
	s := FaultSchedule{Seed: seed, Events: []FaultEvent{}}
	if rate < 0 || rate > 1 {
		return s, fmt.Errorf("fault schedule: rate %v outside 0..1", rate)
	}
	if within < 1 {
		return s, fmt.Errorf("fault schedule: restart bound %d below 1", within)
	}
	if ops <= 0 {
		return s, fmt.Errorf("fault schedule: ops %d", ops)
	}
	procs := slices.Clone(processes)
	slices.Sort(procs)
	if rate > 0 && len(procs) == 0 {
		return s, fmt.Errorf("fault schedule: faults on but no processes")
	}
	for i, p := range procs {
		if !valid(p) || (i > 0 && procs[i-1] == p) {
			return s, fmt.Errorf("fault schedule: process %q unknown or repeated", p)
		}
	}
	if rate == 0 {
		return s, nil
	}
	s.Events = ordinaryKills(seed, ops, procs, rate, within, nil)
	return s, nil
}

// ordinaryKills are the rate-drawn and forced kills with their restarts,
// outside the reserved windows ([from, to] pairs, sorted): a kill never
// falls in one, and its restart comes before the next one begins.
func ordinaryKills(seed uint64, ops int, procs []string, rate float64, within int, reserved [][2]int) []FaultEvent {
	// Its own stream: the operation generator's stream (same seed) is untouched.
	rng := rand.New(rand.NewPCG(seed^0x6661756c74730000, seed+0x2545f4914f6cdd1d))
	var events []FaultEvent
	kills := 0
	for k := 1; k < ops; k++ {
		drawn := rng.Float64() < rate
		forced := kills < len(procs) && k >= (kills+1)*ops/(len(procs)+1)
		if !drawn && !forced {
			continue
		}
		next := ops + 1 // the next reserved window's start after k
		inside := false
		for _, w := range reserved {
			if k >= w[0] && k <= w[1] {
				inside = true
			}
			if w[0] > k && w[0] < next {
				next = w[0]
			}
		}
		r := min(k+1+rng.IntN(within), ops, next-1)
		if inside || r <= k {
			continue // the window is an in-operation fault's: no kill here
		}
		p := procs[kills%len(procs)]
		kills++
		events = append(events, FaultEvent{AfterOp: k, Action: FaultKill, Process: p}, FaultEvent{AfterOp: r, Action: FaultRestart, Process: p})
		k = r // the next kill comes after the restart's operation
	}
	return events
}

// AddMasterOutages adds short master outages to a schedule. rate is the
// chance, after each operation k (1..ops-1) that lies outside every kill's
// interval [kill, restart], of an outage of master after operation k. Its own
// stream: the schedule's kills and restarts are unchanged, and a rate of 0
// returns the schedule as it was.
func AddMasterOutages(s FaultSchedule, ops int, master string, rate float64, valid func(string) bool) (FaultSchedule, error) {
	if rate < 0 || rate > 1 {
		return s, fmt.Errorf("fault schedule: master outage rate %v outside 0..1", rate)
	}
	if rate == 0 {
		return s, nil
	}
	if master == "" || !valid(master) {
		return s, fmt.Errorf("fault schedule: master outages on, but the master %q is no process", master)
	}
	busy := map[int]bool{}
	start := 0
	for _, e := range s.Events {
		switch e.Action {
		case FaultKill, FaultKillDuring:
			start = e.AfterOp
		case FaultRestart:
			for k := start; k <= e.AfterOp; k++ {
				busy[k] = true
			}
		}
	}
	rng := rand.New(rand.NewPCG(s.Seed^0x6d6f757461676500, s.Seed+0x7f4a7c159e3779b9))
	events := slices.Clone(s.Events)
	for k := 1; k < ops; k++ {
		if rng.Float64() < rate && !busy[k] {
			events = append(events, FaultEvent{AfterOp: k, Action: FaultMasterOutage, Process: master})
		}
	}
	slices.SortStableFunc(events, func(a, b FaultEvent) int { return a.AfterOp - b.AfterOp })
	return FaultSchedule{Seed: s.Seed, Events: events}, nil
}

// InOpFaults sets the in-operation faults AddInOpFaults adds: one
// kill_during per During action, in turn over the processes, and, when
// Store names a store process, one store_pause around a StoreAction
// operation.
type InOpFaults struct {
	During      []string // the actions a kill is timed into, one event each
	Store       string   // the store process a store_pause pauses ("" for none)
	StoreAction string   // the action a store_pause wraps
}

// NewFaultScheduleIn is NewFaultSchedule with the in-operation faults
// placed FIRST, BY CONSTRUCTION, so every fault run holds them whatever its
// seed and rate: each takes, from a seeded start in the first third of the
// run (its own stream), the first window of within+1 operations no other
// in-operation fault holds, and the ordinary kills are drawn around those
// windows (none inside one, every restart before the next begins). A
// kill_during gets its restart at its window's end and a seeded Permille in
// 200..800; the scheduled process of each is the processes' in turn (the
// target is resolved at run time: the holder of the operation's subject).
// A window that does not fit is an error, not a silent drop.
func NewFaultScheduleIn(seed uint64, ops int, processes []string, rate float64, within int, in InOpFaults, valid func(string) bool) (FaultSchedule, error) {
	s, err := NewFaultSchedule(seed, ops, processes, 0, within, valid)
	if err != nil {
		return s, err
	}
	procs := slices.Clone(processes)
	slices.Sort(procs)
	if len(in.During) > 0 && len(procs) == 0 {
		return s, fmt.Errorf("fault schedule: kills during an operation, but no processes")
	}
	if in.Store != "" && in.StoreAction == "" {
		return s, fmt.Errorf("fault schedule: a store pause needs the action it wraps")
	}
	if rate < 0 || rate > 1 {
		return s, fmt.Errorf("fault schedule: rate %v outside 0..1", rate)
	}
	rng := rand.New(rand.NewPCG(seed^0x696e6f7066617574, seed+0x51afd7ed558ccd))
	var reserved [][2]int
	var events []FaultEvent
	place := func(what string) (int, error) {
		from := max(1, ops/10+rng.IntN(max(1, ops/3-ops/10)))
		for k := from; k+within < ops; k++ {
			free := true
			for _, w := range reserved {
				free = free && (k+within < w[0] || k > w[1])
			}
			if free {
				reserved = append(reserved, [2]int{k, k + within})
				return k, nil
			}
		}
		return 0, fmt.Errorf("fault schedule: no free window of %d operations for %s in %d operations", within+1, what, ops)
	}
	for i, a := range in.During {
		k, err := place(FaultKillDuring + " " + a)
		if err != nil {
			return s, err
		}
		p := procs[i%len(procs)]
		events = append(events, FaultEvent{AfterOp: k, Action: FaultKillDuring, Process: p, Target: a, Permille: 200 + rng.IntN(601)},
			FaultEvent{AfterOp: k + within, Action: FaultRestart, Process: p})
	}
	if in.Store != "" {
		k, err := place(FaultStorePause)
		if err != nil {
			return s, err
		}
		events = append(events, FaultEvent{AfterOp: k, Action: FaultStorePause, Process: in.Store, Target: in.StoreAction})
	}
	slices.SortFunc(reserved, func(a, b [2]int) int { return a[0] - b[0] })
	if rate > 0 {
		events = append(events, ordinaryKills(seed, ops, procs, rate, within, reserved)...)
	}
	slices.SortStableFunc(events, func(a, b FaultEvent) int { return a.AfterOp - b.AfterOp })
	return FaultSchedule{Seed: seed, Events: events}, nil
}

// CheckFaultSchedule judges a schedule against the rules above; it is used
// on every recorded schedule a replay reads, and its red controls are in the
// tests.
func CheckFaultSchedule(events []FaultEvent, ops, within int, valid func(string) bool) error {
	down, since, last := "", 0, 0
	for i, e := range events {
		if e.Action == FaultStorePause {
			// The store process is not a kill target: the product's store
			// pause names it (its own validation).
			if e.Process == "" || e.Target == "" {
				return fmt.Errorf("event %d: a store pause needs its process and target", i)
			}
		} else if !valid(e.Process) {
			return fmt.Errorf("event %d: process %q", i, e.Process)
		}
		if e.AfterOp < 1 || e.AfterOp > ops || e.AfterOp < last {
			return fmt.Errorf("event %d: after_op %d out of order or outside 1..%d", i, e.AfterOp, ops)
		}
		last = e.AfterOp
		switch e.Action {
		case FaultKill, FaultKillDuring:
			if down != "" {
				return fmt.Errorf("event %d: kill %s while %s is down (one process down at a time)", i, e.Process, down)
			}
			if e.Action == FaultKillDuring && (e.Target == "" || e.Permille < 1 || e.Permille > 999) {
				return fmt.Errorf("event %d: a kill during an operation needs its target and a permille in 1..999", i)
			}
			down, since = e.Process, e.AfterOp
		case FaultStorePause:
			if down != "" {
				return fmt.Errorf("event %d: store pause while %s is down (one fault at a time)", i, down)
			}
		case FaultRestart:
			if down != e.Process {
				return fmt.Errorf("event %d: restart %s, which is not down", i, e.Process)
			}
			if e.AfterOp-since > within || e.AfterOp == since {
				return fmt.Errorf("event %d: %s restarted %d operations after its kill, bound 1..%d", i, e.Process, e.AfterOp-since, within)
			}
			down = ""
		case FaultMasterOutage:
			if down != "" {
				return fmt.Errorf("event %d: master outage of %s while %s is down (one process down at a time)", i, e.Process, down)
			}
		default:
			return fmt.Errorf("event %d: action %q", i, e.Action)
		}
	}
	if down != "" {
		return fmt.Errorf("%s is killed and never restarted", down)
	}
	return nil
}

// Record is the fault_schedule observe record's result.
func (s FaultSchedule) Record() map[string]any {
	ev := make([]any, 0, len(s.Events))
	for _, e := range s.Events {
		m := map[string]any{"after_op": e.AfterOp, "action": e.Action, "process": e.Process}
		if e.Target != "" {
			m["target"] = e.Target
		}
		if e.Permille != 0 {
			m["permille"] = e.Permille
		}
		ev = append(ev, m)
	}
	return map[string]any{"seed": strconv.FormatUint(s.Seed, 10), "events": ev}
}

// parseFaultEvents reads the events of a recorded fault_schedule result.
func parseFaultEvents(res map[string]any) ([]FaultEvent, error) {
	raw, ok := res["events"].([]any)
	if !ok {
		return nil, fmt.Errorf("fault_schedule: events %v", res["events"])
	}
	out := make([]FaultEvent, 0, len(raw))
	for i, r := range raw {
		m, ok := r.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("fault_schedule: event %d is %T", i, r)
		}
		k, ok := m["after_op"].(float64)
		a, ok2 := m["action"].(string)
		p, ok3 := m["process"].(string)
		if !ok || !ok2 || !ok3 || k != float64(int(k)) {
			return nil, fmt.Errorf("fault_schedule: event %d %v", i, m)
		}
		e := FaultEvent{AfterOp: int(k), Action: a, Process: p}
		if t, ok := m["target"].(string); ok {
			e.Target = t
		}
		if pm, ok := m["permille"].(float64); ok {
			e.Permille = int(pm)
		}
		out = append(out, e)
	}
	return out, nil
}
