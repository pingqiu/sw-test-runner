package randomrun

// In-operation faults: a fault timed INTO an operation, so the operation's
// request ends unknown by construction and C6 must place it.
//
//   - kill_during (FaultKillDuring), armed after operation after_op, fires in
//     the first later operation of its target action whose subject a holder
//     serves (HolderNamer) and whose action has a measured duration: a
//     single look first (nothing down, as for any kill), then the operation,
//     and BESIDE it, at permille/1000 of the action's SHORTEST measured
//     call after the operation's own invocation record (the history's
//     OnInvoke), the kill of that holder.
//     It is the only concurrency of the runner: one fault beside one
//     operation, never two operations (opGuard refuses a second). After both
//     answered, an observe record "fault_during" {after_op, opno, action,
//     volume, scheduled, process, offset_ms, op_invoke, op_complete,
//     kill_invoke, kill_complete, hit, why} says when the kill ran against
//     the operation's invoke and completion: a kill invoked after the
//     operation completed is MISSED (hit false), and only a hit counts. Its
//     restart is the schedule's; a kill that never fired (no target before
//     the restart) has its restart skipped, recorded the same way.
//   - store_pause (FaultStorePause) fires in the first later operation of its
//     target action: after the driver's observation before the first attempt
//     the store is paused (StoreFaulter: a fault_apply with a bounded
//     detached continue, its bound in the record), the operation runs, the
//     store is continued (fault_restore); then a healthy point resolves the
//     unknown, an observe record "store_pause" {after_op, opno, action,
//     volume, process, bound_seconds, outcome, hit, why, followups} says
//     whether the operation ended unknown (hit) or answered anyway (MISSED),
//     and the model's follow-ups (StoreFollower: a later request on the same
//     subject, which a checker requires to succeed) run, learnt, outside any
//     step.
//
// A replay re-executes both from their records: the same operation number,
// process, offset and follow-ups.
//
// The handlers of an operation a kill is timed into are called with a
// context that says so (KillTimedInto): a product ignores it; an emulation
// of the product (a test's fake) can hold that operation's answer until the
// kill has landed, so a hit there follows from the operation's behaviour,
// not from the machine's timing. Nothing else changes: the offset, its
// timing and the records are the same with or without a reader of it.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	tr "github.com/pingqiu/sw-test-runner"
)

// HolderNamer is an optional part of a Model: the process that holds an
// operation's subject now (a kill_during kills it), ok false when none does.
type HolderNamer interface {
	HolderOf(o Op) (process string, ok bool)
}

// Targeter is an optional part of a Model: an operation of action its state
// allows now (on a subject a holder serves), forced so that an armed
// in-operation fault meets its target operation BY CONSTRUCTION, not by the
// luck of the draw before its window closes; ok false when none is possible.
type Targeter interface {
	TargetOp(action string) (Op, bool)
}

// ForcedFault is the Forced reason of an operation proposed for an armed
// in-operation fault.
const ForcedFault = "fault"

// forceTargets queues, for the first armed in-operation fault, an operation
// of its target action (Targeter), once: the next proposal is it. Run calls
// it after each step's fault events, the queue being dropped when an event
// ran.
func (f *faultRun) forceTargets(g *Generator) {
	if f == nil || f.gen == nil || f.forcing || len(f.armed) == 0 || f.down != "" {
		return
	}
	tg, ok := f.gen.m.(Targeter)
	if !ok {
		return
	}
	for _, e := range f.armed {
		if o, ok := tg.TargetOp(e.Target); ok {
			o.Forced = ForcedFault
			g.queue = append(g.queue, queued{op: o})
			f.forcing = true
			return
		}
	}
}

// StoreFaulter is an optional part of a Driver: the operations that pause
// the store process of a store_pause, for at most bound seconds (a detached
// continue ends the pause by then whatever happens), and continue it.
type StoreFaulter interface {
	StorePause(e FaultEvent, boundSeconds int) (pause, cont Op)
}

// StoreFollower is an optional part of a Model: the requests that follow a
// store pause around operation o, on its subject (for a block volume: a
// later snapshot and a detach), run and learnt after the healthy point.
type StoreFollower interface {
	AfterStorePause(o Op) []Op
}

// AttemptHooks are run around the attempts of one call (AttemptsWith):
// BeforeFirst right before the first attempt is issued (after the driver's
// observation before it), AfterLast after the call's attempts ended. Took is
// set to the first attempt's own window in the history: from its invocation
// record to its completion record (recordWindow). A kill timed into an
// operation runs from that invocation record (killDuring), so its offset is
// a fraction of the same window: the handler's work before it records the
// invocation, and the pauses and later attempts of a rerun, are not in it.
type AttemptHooks struct {
	BeforeFirst, AfterLast func() error
	Took                   time.Duration
}

// AttemptsWith is Attempts with hooks around the attempts.
func AttemptsWith(ctx context.Context, actx *tr.ActionContext, h tr.ActionHandlerFunc, act tr.Action, rr Rerunner, subject string, observe func() (map[string]any, error), hk *AttemptHooks) (map[string]string, int, error) {
	if hk == nil {
		return Attempts(ctx, actx, h, act, rr, subject, observe)
	}
	var hookErr error
	issued := false
	var took time.Duration
	wrapped := func(ctx context.Context, actx *tr.ActionContext, a tr.Action) (map[string]string, error) {
		if issued {
			return h(ctx, actx, a)
		}
		issued = true
		if hk.BeforeFirst != nil {
			if err := hk.BeforeFirst(); err != nil {
				hookErr = err
				return nil, err
			}
		}
		if actx.History == nil {
			return h(ctx, actx, a) // no history: no window (Took 0, nothing measured)
		}
		from := historySize(actx.History)
		out, err := h(ctx, actx, a)
		if recs, rerr := historySince(actx.History, from); rerr == nil {
			took = recordWindow(recs)
		}
		return out, err
	}
	out, n, err := Attempts(ctx, actx, wrapped, act, rr, subject, observe)
	if hookErr != nil {
		return nil, n, hookErr
	}
	hk.Took = took
	if hk.AfterLast != nil {
		if aerr := hk.AfterLast(); aerr != nil && err == nil {
			return nil, n, aerr
		}
	}
	return out, n, err
}

// The concurrency guard: at most one operation at a time on a history, and
// at most one fault beside it.
var opGuard, faultGuard sync.Map // *tr.History -> *atomic.Int32

func enter(m *sync.Map, h *tr.History) (func(), bool) {
	v, _ := m.LoadOrStore(h, new(atomic.Int32))
	c := v.(*atomic.Int32)
	if !c.CompareAndSwap(0, 1) {
		return nil, false
	}
	return func() { c.Store(0) }, true
}

// errTwoOperations is the guard's refusal.
var errTwoOperations = fmt.Errorf("two operations at once: the runner runs one operation at a time (only a fault may run beside it)")

// inOpRecord is a recorded in-operation fault, as a replay reads it.
type inOpRecord struct {
	Action    string // FaultKillDuring or FaultStorePause
	AfterOp   int
	OpNo      int
	Process   string
	OffsetMs  int
	Followups []Op
}

// arm takes an in-operation event after its after_op: it waits for its
// target operation.
func (f *faultRun) arm(e FaultEvent) {
	f.armed = append(f.armed, e)
}

// inOpFor is the armed in-operation event operation o fires (nil for none):
// in a run, the first armed event whose target is o's action (not a negative:
// a negative probes a limit, it is not the request a fault interrupts); in a
// replay, the event recorded at o's number.
func (f *faultRun) inOpFor(o Op) *FaultEvent {
	if o.Negative != "" || o.Forced == "tail" {
		return nil
	}
	for i := range f.armed {
		e := f.armed[i]
		if e.Target != o.Action {
			continue
		}
		if f.gen == nil {
			rec, ok := f.inOpRecs[o.No]
			if !ok || rec.Action != e.Action || rec.AfterOp != e.AfterOp {
				continue
			}
		}
		return &f.armed[i]
	}
	return nil
}

// expire drops, after operation opno, an armed store pause whose window
// (after_op + the restart bound) closed without its target operation: it is
// MISSED and recorded so (a kill_during's window is closed by its restart).
func (f *faultRun) expire(actx *tr.ActionContext, opno int) error {
	for _, a := range slices.Clone(f.armed) {
		if a.Action != FaultStorePause || opno <= a.AfterOp+f.within {
			continue
		}
		f.disarm(a)
		if f.gen != nil {
			f.gen.inOp(FaultStorePause, a.Target, false)
		}
		if err := actx.History.Observe(tr.HistoryObserve, tr.HistoryRecord{Client: "gen", Verb: "store_pause", Result: map[string]any{
			"after_op": a.AfterOp, "process": a.Process, "action": a.Target, "hit": false, "followups": []any{},
			"why": "MISSED: no target operation in its window"}}); err != nil {
			return err
		}
	}
	return nil
}

type killTimedKey struct{}

// KillTimedInto reports whether ctx is that of an operation a kill_during
// is timed into (killDuring): for an emulation of the product only.
func KillTimedInto(ctx context.Context) bool {
	v, _ := ctx.Value(killTimedKey{}).(bool)
	return v
}

// disarm drops an armed event once it fired.
func (f *faultRun) disarm(e FaultEvent) {
	f.armed = slices.DeleteFunc(f.armed, func(a FaultEvent) bool { return a == e })
}

// measure keeps the measured duration of one operation of action.
func (f *faultRun) measure(action string, took time.Duration) {
	if took <= 0 {
		return
	}
	if f.durations == nil {
		f.durations = map[string][]time.Duration{}
	}
	f.durations[action] = append(f.durations[action], took)
}

// shortest is the shortest measured duration of one call of action (0 for
// none): a kill at a fraction of it lands inside any call as long as the
// action took that long before.
func (f *faultRun) shortest(action string) time.Duration {
	if len(f.durations[action]) == 0 {
		return 0
	}
	return slices.Min(f.durations[action])
}

// stepInOp runs operation o with in-operation event e: it answers the
// operation's outcome and whether e fired (false: e stays armed, o ran as
// an ordinary step).
func (f *faultRun) stepInOp(ctx context.Context, actx *tr.ActionContext, o Op, e FaultEvent) (string, int, bool, error) {
	if e.Action == FaultKillDuring {
		return f.killDuring(ctx, actx, o, e)
	}
	return f.storePause(ctx, actx, o, e)
}

// killDuring: see the file's doc.
func (f *faultRun) killDuring(ctx context.Context, actx *tr.ActionContext, o Op, e FaultEvent) (string, int, bool, error) {
	var target string
	var offset time.Duration
	if f.gen != nil {
		hn, ok := f.gen.m.(HolderNamer)
		if !ok {
			return "", 0, false, fmt.Errorf("kill during %s: the model names no holder (HolderNamer)", o.Action)
		}
		p, held := hn.HolderOf(o)
		med := f.shortest(o.Action)
		if !held || med == 0 || f.down != "" {
			return "", 0, false, nil // not this one: no holder, no measured duration yet, or a process down
		}
		target, offset = p, med*time.Duration(e.Permille)/1000
	} else {
		rec := f.inOpRecs[o.No]
		target, offset = rec.Process, time.Duration(rec.OffsetMs)*time.Millisecond
	}
	// A single look first, as before any kill: nothing may be down.
	lo := f.r.Driver.Look()
	lh, ok := f.r.Handlers[lo.Action]
	if !ok {
		return "", 0, false, fmt.Errorf("no handler for %s", lo.Action)
	}
	look, err := lh(ctx, actx, tr.Action{Action: lo.Action, Node: f.r.Node, Params: lo.Params})
	if err != nil {
		return "", 0, false, fmt.Errorf("look before the kill during operation %d: %w", o.No, err)
	}
	if look[DownKey] != "" && f.gen != nil {
		return "", 0, false, nil // something is down: not now
	}
	kill := f.r.Driver.Fault(FaultEvent{AfterOp: e.AfterOp, Action: FaultKill, Process: target})
	kh, ok := f.r.Handlers[kill.Action]
	if !ok {
		return "", 0, false, fmt.Errorf("no handler for %s", kill.Action)
	}
	from := historySize(actx.History)
	// The fault runs on its own copy of the run's variables (the product's
	// records of its volumes and processes live there): no map is shared
	// between the two goroutines; its changes are merged back after both
	// answered (mergeVars), field by field over a record the operation
	// changed too. That is the lock on the volume record.
	kactx := *actx
	kactx.Vars = maps.Clone(actx.Vars)
	kbefore := maps.Clone(actx.Vars)
	var wg sync.WaitGroup
	var killOut map[string]string
	var killErr error
	// The offset runs from the operation's own invocation record, not from
	// the handler call (a handler checks and records before it issues the
	// command): the history tells of each invocation (OnInvoke), and the
	// first that is not the kill's starts the timer. A hook, not a poll of
	// the file: no window in which the record exists but is not yet seen.
	invoked := make(chan struct{})
	opDone := make(chan struct{})
	var once sync.Once
	actx.History.OnInvoke(func(r tr.HistoryRecord) {
		if r.Verb != VerbKill {
			once.Do(func() { close(invoked) })
		}
	})
	wg.Add(1)
	go func() {
		defer wg.Done()
		leave, ok := enter(&faultGuard, actx.History)
		if !ok {
			killErr = fmt.Errorf("two faults at once beside operation %d", o.No)
			return
		}
		defer leave()
		select {
		case <-ctx.Done():
			killErr = ctx.Err()
			return
		case <-opDone:
			return // the operation recorded no invocation: nothing to kill into
		case <-invoked:
		}
		t := time.NewTimer(offset)
		defer t.Stop()
		select {
		case <-ctx.Done():
			killErr = ctx.Err()
			return
		case <-t.C:
		}
		killOut, killErr = kh(ctx, &kactx, tr.Action{Action: kill.Action, Node: f.r.Node, Params: kill.Params})
	}()
	hk := &AttemptHooks{}
	outcome, n, took, err := f.r.stepWith(context.WithValue(ctx, killTimedKey{}, true), actx, o, hk)
	actx.History.OnInvoke(nil)
	close(opDone)
	wg.Wait()
	mergeVars(actx.Vars, kbefore, kactx.Vars)
	if err != nil {
		return "", n, true, err
	}
	if killErr != nil {
		return "", n, true, fmt.Errorf("kill %s during operation %d: %w", target, o.No, killErr)
	}
	f.measure(o.Action, took)
	f.disarm(e)
	got := killOut["value"]
	recs, err := historySince(actx.History, from)
	if err != nil {
		return "", n, true, err
	}
	res := map[string]any{"after_op": e.AfterOp, "opno": o.No, "action": o.Action, "volume": o.Volume, "scheduled": e.Process,
		"process": target, "offset_ms": offset.Milliseconds(), "kill_outcome": got}
	hit, why := duringTimes(recs, res)
	res["hit"], res["why"] = hit, why
	if err := actx.History.Observe(tr.HistoryObserve, tr.HistoryRecord{Client: "gen", Verb: "fault_during", Volume: o.Volume, Result: res}); err != nil {
		return "", n, true, err
	}
	if got != "" {
		f.fired[e.Process] = true // the kill ran (its restart follows)
	}
	if got == tr.OutcomeOK || got == tr.OutcomeUnknown {
		f.down, f.downScheduled = target, e.Process
	}
	if f.gen != nil {
		f.gen.Fault(FaultKillDuring, e.Process)
		f.gen.inOp(FaultKillDuring, o.Action, hit)
		f.gen.m.FaultRan(FaultKill, target, got)
	}
	return outcome, n, true, nil
}

// duringTimes reads, from the records of one kill_during step, the
// operation's invoke and completion and the kill's, into res, and says
// whether the kill ran inside the operation (invoked after the operation's
// first invoke and before its last completion): a hit, or MISSED and why.
func duringTimes(recs []tr.HistoryRecord, res map[string]any) (bool, string) {
	var opInv, opComp, killInv, killComp uint64
	kills := map[uint64]bool{}
	for _, r := range recs {
		switch r.Kind {
		case tr.HistoryInvoke:
			if r.Verb == VerbKill {
				killInv = r.Seq
				kills[r.Seq] = true
			} else if opInv == 0 {
				opInv = r.Seq
			}
		case tr.HistoryComplete:
			if kills[r.Of] {
				killComp = r.Seq
			} else if opInv != 0 && r.Of >= opInv {
				opComp = r.Seq
			}
		}
	}
	res["op_invoke"], res["op_complete"], res["kill_invoke"], res["kill_complete"] = opInv, opComp, killInv, killComp
	switch {
	case opInv == 0 || killInv == 0:
		return false, "MISSED: no operation or no kill recorded"
	case killInv < opInv:
		return false, "MISSED: the kill ran before the operation"
	case opComp != 0 && killInv > opComp:
		return false, "MISSED: the kill ran after the operation completed"
	}
	return true, ""
}

// storePause: see the file's doc.
func (f *faultRun) storePause(ctx context.Context, actx *tr.ActionContext, o Op, e FaultEvent) (string, int, bool, error) {
	sf, ok := f.r.Driver.(StoreFaulter)
	if !ok {
		return "", 0, false, fmt.Errorf("store pause: the driver has no store fault (StoreFaulter)")
	}
	if f.down != "" {
		return "", 0, false, nil // one fault at a time
	}
	pause, cont := sf.StorePause(e, f.storeBound)
	run := func(op Op) error {
		h, ok := f.r.Handlers[op.Action]
		if !ok {
			return fmt.Errorf("no handler for %s", op.Action)
		}
		leave, ok := enter(&faultGuard, actx.History)
		if !ok {
			return fmt.Errorf("two faults at once around operation %d", o.No)
		}
		defer leave()
		out, err := h(ctx, actx, tr.Action{Action: op.Action, Node: f.r.Node, Params: op.Params})
		if err != nil {
			return err
		}
		if out["value"] != tr.OutcomeOK {
			return fmt.Errorf("%s answered %s: the store fault is not known applied", op.Action, out["value"])
		}
		return nil
	}
	hk := &AttemptHooks{BeforeFirst: func() error { return run(pause) }, AfterLast: func() error { return run(cont) }}
	outcome, n, took, err := f.r.stepWith(ctx, actx, o, hk)
	if err != nil {
		return "", n, true, fmt.Errorf("store pause around operation %d: %w", o.No, err)
	}
	_ = took // a paused operation's duration is not a measure of the action
	f.disarm(e)
	var follow []Op
	if f.gen != nil {
		if sfl, ok := f.gen.m.(StoreFollower); ok {
			follow = sfl.AfterStorePause(o)
		}
	} else {
		follow = f.inOpRecs[o.No].Followups
	}
	hit, why := outcome == tr.OutcomeUnknown, ""
	if !hit {
		why = "MISSED: the operation answered " + outcome + " through the pause"
	}
	f.post = func(ctx context.Context, actx *tr.ActionContext) error {
		// The unknown is resolved at a healthy point first; then the record,
		// then the follow-ups, outside any step.
		if err := f.healthy(ctx, actx); err != nil {
			return err
		}
		res := map[string]any{"after_op": e.AfterOp, "opno": o.No, "action": o.Action, "volume": o.Volume, "process": e.Process,
			"bound_seconds": f.storeBound, "outcome": outcome, "hit": hit, "followups": opList(follow)}
		if why != "" {
			res["why"] = why
		}
		if err := actx.History.Observe(tr.HistoryObserve, tr.HistoryRecord{Client: "gen", Verb: "store_pause", Volume: o.Volume, Result: res}); err != nil {
			return err
		}
		if f.gen != nil {
			f.gen.Fault(FaultStorePause, e.Process)
			f.gen.inOp(FaultStorePause, o.Action, hit)
		}
		for _, w := range follow {
			got, err := f.r.Exec(ctx, actx, w)
			if err != nil {
				return fmt.Errorf("after the store pause around operation %d: %s %s: %w", o.No, w.Action, w.Volume, err)
			}
			if f.gen != nil {
				f.gen.Learn(w, got)
			}
			f.note(w, got)
		}
		return nil
	}
	return outcome, n, true, nil
}

// afterStep runs what an in-operation fault leaves for after the step was
// learnt (a store pause's healthy point, record and follow-ups).
func (f *faultRun) afterStep(ctx context.Context, actx *tr.ActionContext) error {
	if f.post == nil {
		return nil
	}
	p := f.post
	f.post = nil
	return p(ctx, actx)
}

// mergeVars merges into vars the changes the fault made to its copy
// (before: the variables as the fault got them; after: as it left them). A
// key only the fault changed takes the fault's value; a key both changed,
// when both values are JSON objects (a record), takes the operation's
// record with the fields the fault changed overlaid; otherwise the
// operation's value stands.
func mergeVars(vars, before, after map[string]string) {
	for k, fv := range after {
		bv, had := before[k]
		if had && bv == fv {
			continue // the fault left it
		}
		ov, has := vars[k]
		if !has || ov == bv {
			vars[k] = fv // only the fault changed it
			continue
		}
		var o, b, a map[string]any
		if json.Unmarshal([]byte(ov), &o) != nil || json.Unmarshal([]byte(bv), &b) != nil || json.Unmarshal([]byte(fv), &a) != nil {
			continue
		}
		for f, x := range a {
			if y, ok := b[f]; !ok || fmt.Sprint(y) != fmt.Sprint(x) {
				o[f] = x
			}
		}
		if m, err := json.Marshal(o); err == nil {
			vars[k] = string(m)
		}
	}
}

// historySize is the history file's size now (where the records of what
// runs next begin).
func historySize(h *tr.History) int64 {
	st, err := os.Stat(h.Path())
	if err != nil {
		return 0
	}
	return st.Size()
}

// recordWindow is the window of the first operation recorded in recs (a
// kill's records aside): from its invocation record to its completion
// record, on the history's clock; 0 when it has no completion there.
func recordWindow(recs []tr.HistoryRecord) time.Duration {
	var inv *tr.HistoryRecord
	for i := range recs {
		r := &recs[i]
		switch {
		case inv == nil && r.Kind == tr.HistoryInvoke && r.Verb != VerbKill:
			inv = r
		case inv != nil && r.Kind == tr.HistoryComplete && r.Of == inv.Seq:
			return time.Duration(r.MonoNs - inv.MonoNs)
		}
	}
	return 0
}

// historySince reads the history's records from byte offset from.
func historySince(h *tr.History, from int64) ([]tr.HistoryRecord, error) {
	f, err := os.Open(h.Path())
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if _, err := f.Seek(from, 0); err != nil {
		return nil, err
	}
	var out []tr.HistoryRecord
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 1<<20), 64<<20)
	for s.Scan() {
		var r tr.HistoryRecord
		if json.Unmarshal(s.Bytes(), &r) == nil {
			out = append(out, r)
		}
	}
	return out, s.Err()
}
