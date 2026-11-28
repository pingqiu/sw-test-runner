package v3block

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/randomrun"
)

// inOpAct is a fault run with the in-operation faults of m2-faults.yaml.
func inOpAct(seed string) tr.Action {
	a := faultAct("300", "0.05")
	a.Params["seed"] = seed
	a.Params["fault_processes"] = "blockvolume:0 blockvolume:1 blockvolume:2 blockvolume:3"
	a.Params["fault_during"] = "v3_op_write v3_op_snapshot"
	a.Params["fault_store"] = "weed-filer"
	a.Params["fault_store_action"] = "v3_op_snapshot"
	a.Params["fault_store_bound"] = "75"
	return a
}

// fakeOpDelay is how long a fake write or snapshot takes between its invoke
// and its answer. An operation a kill is timed into (randomrun.KillTimedInto)
// holds its answer until the kill has landed, as the product's operation
// cut by its holder's death does, so the hit follows from the operation's
// behaviour and not from the machine's timing; killLandGuard bounds that
// hold (on expiry it answers ok, and the test sees the kill MISSED).
const (
	fakeOpDelay   = 50 * time.Millisecond
	killLandGuard = 10 * time.Second
)

// inOpRun runs a fault run with in-operation faults over the fake (each
// write and snapshot taking fakeOpDelay) and returns its records.
func inOpRun(t *testing.T, seed string) ([]tr.HistoryRecord, string, *faultFake) {
	t.Helper()
	f := &faultFake{opDelay: fakeOpDelay}
	f.install(t)
	actx, p := genRunCtx(t)
	if _, err := v3GenRun(context.Background(), actx, inOpAct(seed)); err != nil {
		t.Fatal(err)
	}
	_ = actx.History.Close()
	return records(t, p), p, f
}

// excerpt is the records of a history from seq from to seq to, one line each.
func excerpt(recs []tr.HistoryRecord, from, to uint64) []string {
	var out []string
	for _, r := range recs {
		if r.Seq < from || r.Seq > to {
			continue
		}
		line := fmt.Sprintf("%d %s %s %s", r.Seq, r.Kind, r.Verb, r.Volume)
		switch r.Kind {
		case tr.HistoryComplete:
			line += fmt.Sprintf(" of %d %s", r.Of, r.Outcome)
		case tr.HistoryObserve:
			if r.Verb == "fault_during" || r.Verb == "store_pause" {
				line += fmt.Sprintf(" %v", r.Result)
			}
		}
		out = append(out, line)
	}
	return out
}

// The in-operation faults HIT by construction, on the nightly's seeds (the
// night of 2026-11-27, five runs): every fault run holds a kill timed into
// a write and into a snapshot, and a store pause around a snapshot; each is
// a hit (recorded "fault_during" / "store_pause" hit true), the operation
// ended unknown, and the requests after the store pause succeeded. The
// first run's excerpts are logged.
func TestInOpFaultsHitByConstruction(t *testing.T) {
	for i, seed := range randomrun.NightlySeeds(20261127, 5) {
		recs, _, f := inOpRun(t, strconv.FormatUint(seed, 10))
		hits := map[string]int{}
		bySeq := map[uint64]tr.HistoryRecord{}
		for _, r := range recs {
			bySeq[r.Seq] = r
		}
		for j, r := range recs {
			switch r.Verb {
			case "fault_during":
				if r.Result["hit"] != true {
					t.Errorf("seed %d: %v", seed, r.Result)
					continue
				}
				hits["kill_during "+r.Result["action"].(string)]++
				opc := bySeq[uint64(r.Result["op_complete"].(float64))]
				ki, kc := uint64(r.Result["kill_invoke"].(float64)), uint64(r.Result["kill_complete"].(float64))
				oi := uint64(r.Result["op_invoke"].(float64))
				if !(oi < ki && ki < opc.Seq && kc > ki) || opc.Outcome != tr.OutcomeUnknown {
					t.Errorf("seed %d: the kill did not land inside its operation, or it did not end unknown: %v", seed, r.Result)
				}
				if i == 0 {
					t.Logf("kill during %s:\n%s", r.Result["action"], strings.Join(excerpt(recs, oi-1, r.Seq), "\n"))
				}
			case "store_pause":
				if r.Result["hit"] != true || r.Result["outcome"] != tr.OutcomeUnknown {
					t.Errorf("seed %d: %v", seed, r.Result)
					continue
				}
				hits["store_pause"]++
				// The follow-ups: up to the next step, each answered ok, and a stop among them.
				stop := false
				for _, x := range recs[j+1:] {
					if x.Verb == "gen_step" || x.Verb == "gen_sweep" {
						break
					}
					if x.Kind == tr.HistoryComplete && x.Outcome != tr.OutcomeOK {
						t.Errorf("seed %d: a follow-up answered %s: %+v", seed, x.Outcome, x)
					}
					stop = stop || (x.Kind == tr.HistoryInvoke && x.Verb == "stop")
				}
				if !stop {
					t.Errorf("seed %d: no detach after the store pause", seed)
				}
				if i == 0 {
					var from uint64
					for _, x := range recs[:j] {
						if x.Verb == "fault_apply" && x.Kind == tr.HistoryInvoke {
							from = x.Seq
						}
					}
					t.Logf("store pause:\n%s", strings.Join(excerpt(recs, from-2, r.Seq+12), "\n"))
				}
			}
		}
		if hits["kill_during v3_op_write"] != 1 || hits["kill_during v3_op_snapshot"] != 1 || hits["store_pause"] != 1 {
			t.Errorf("seed %d: hits %v", seed, hits)
		}
		if len(f.storeCalls) != 2 || !strings.HasPrefix(f.storeCalls[0], "fault_apply weed-filer 75") {
			t.Errorf("seed %d: store calls %v", seed, f.storeCalls)
		}
		var cov map[string]any
		for _, r := range recs {
			if r.Verb == "gen_coverage" {
				cov = r.Result
			}
		}
		cells, _ := cov["fault_cells"].(map[string]any)
		never := fmt.Sprint(cov["cells_never_reached"])
		for _, c := range []string{"kill_during|v3_op_write|hit", "kill_during|v3_op_snapshot|hit", "store_pause|v3_op_snapshot|hit"} {
			if cells[c] == nil || strings.Contains(never, c) {
				t.Errorf("seed %d: %s not reached: %v", seed, c, cells)
			}
		}
	}
}

// A MISSED fault never counts: a kill timed into an action no operation of
// the run ever is (no target: none is proposed, none can be forced) never
// fires before its restart: recorded MISSED, counted apart ("kill_during|v3_op_none|missed"), and
// its hit cell is reported never reached (red control of the coverage
// rule). Deterministic: no timing decides it (a kill landing after the
// operation's completion is MISSED too: randomrun's
// TestKillDuringLandsInsideTheOperation).
func TestMissedInOpFaultsDoNotCount(t *testing.T) {
	f := &faultFake{opDelay: fakeOpDelay}
	f.install(t)
	actx, p := genRunCtx(t)
	act := inOpAct("1")
	act.Params["fault_during"] = "v3_op_none"
	if _, err := v3GenRun(context.Background(), actx, act); err != nil {
		t.Fatal(err)
	}
	_ = actx.History.Close()
	var cov map[string]any
	missed := 0
	for _, r := range records(t, p) {
		if r.Verb == "fault_during" && r.Result["hit"] == false && strings.HasPrefix(fmt.Sprint(r.Result["why"]), "MISSED") {
			missed++
		}
		if r.Verb == "gen_coverage" {
			cov = r.Result
		}
	}
	never := fmt.Sprint(cov["cells_never_reached"])
	cells, _ := cov["fault_cells"].(map[string]any)
	if missed != 1 || !strings.Contains(never, "kill_during|v3_op_none|hit") || cells["kill_during|v3_op_none|missed"] == nil {
		t.Fatalf("%d missed, never reached %s, cells %v", missed, never, cells)
	}
}

// A replay re-executes the in-operation faults from their records: the same
// operations, the kill at the same offset of the same holder, the same
// store pause and follow-ups; every step matches.
func TestInOpFaultsReplay(t *testing.T) {
	_, src, _ := inOpRun(t, "1")
	f := &faultFake{opDelay: fakeOpDelay}
	f.install(t)
	actx, p := genRunCtx(t)
	out, err := genRunner("v3_gen_replay", "host", &blockModel{}).Replay(context.Background(), actx, src)
	if err != nil || out["value"] != "match" {
		t.Fatalf("%v %v", out, err)
	}
	_ = actx.History.Close()
	pick := func(recs []tr.HistoryRecord) []string {
		var o []string
		for _, r := range recs {
			if r.Verb == "fault_during" {
				o = append(o, fmt.Sprintf("during %v %v %v %v", r.Result["opno"], r.Result["process"], r.Result["offset_ms"], r.Result["hit"]))
			}
			if r.Verb == "store_pause" {
				o = append(o, fmt.Sprintf("store %v %v %v", r.Result["opno"], r.Result["hit"], r.Result["followups"]))
			}
		}
		return o
	}
	want, got := pick(records(t, src)), pick(records(t, p))
	if strings.Join(want, "|") != strings.Join(got, "|") {
		t.Fatalf("recorded\n%s\nreplayed\n%s", strings.Join(want, "\n"), strings.Join(got, "\n"))
	}
}
