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
//     and never inside a kill's interval.
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
)

// FaultEvent is one scheduled process fault.
type FaultEvent struct {
	AfterOp int    `json:"after_op"`
	Action  string `json:"action"` // FaultKill or FaultRestart
	Process string `json:"process"`
}

// FaultSchedule is the run's fault schedule, events in execution order.
type FaultSchedule struct {
	Seed   uint64
	Events []FaultEvent
}

// NewFaultSchedule derives the schedule. rate is the chance, after each
// operation while no process is down, that one process (drawn uniformly from
// the sorted list) is killed; its restart comes 1..within operations later,
// capped at ops. A rate of 0 gives an empty schedule.
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
	// Its own stream: the operation generator's stream (same seed) is untouched.
	rng := rand.New(rand.NewPCG(seed^0x6661756c74730000, seed+0x2545f4914f6cdd1d))
	for k := 1; k < ops; k++ {
		if rng.Float64() >= rate {
			continue
		}
		p := procs[rng.IntN(len(procs))]
		r := min(k+1+rng.IntN(within), ops)
		s.Events = append(s.Events, FaultEvent{k, FaultKill, p}, FaultEvent{r, FaultRestart, p})
		k = r // the next kill comes after the restart's operation
	}
	return s, nil
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
		case FaultKill:
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
			events = append(events, FaultEvent{k, FaultMasterOutage, master})
		}
	}
	slices.SortStableFunc(events, func(a, b FaultEvent) int { return a.AfterOp - b.AfterOp })
	return FaultSchedule{Seed: s.Seed, Events: events}, nil
}

// CheckFaultSchedule judges a schedule against the rules above; it is used
// on every recorded schedule a replay reads, and its red controls are in the
// tests.
func CheckFaultSchedule(events []FaultEvent, ops, within int, valid func(string) bool) error {
	down, since, last := "", 0, 0
	for i, e := range events {
		if !valid(e.Process) {
			return fmt.Errorf("event %d: process %q", i, e.Process)
		}
		if e.AfterOp < 1 || e.AfterOp > ops || e.AfterOp < last {
			return fmt.Errorf("event %d: after_op %d out of order or outside 1..%d", i, e.AfterOp, ops)
		}
		last = e.AfterOp
		switch e.Action {
		case FaultKill:
			if down != "" {
				return fmt.Errorf("event %d: kill %s while %s is down (one process down at a time)", i, e.Process, down)
			}
			down, since = e.Process, e.AfterOp
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
		ev = append(ev, map[string]any{"after_op": e.AfterOp, "action": e.Action, "process": e.Process})
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
		out = append(out, FaultEvent{int(k), a, p})
	}
	return out, nil
}
