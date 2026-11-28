package v3block

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	if err != nil || out["value"] != "match_unjudged" {
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

// tamper copies a recorded history, changing the outcome of the first
// complete record of operation opno (after its gen_step) to outcome.
func tamper(t *testing.T, src string, opno int, outcome string) string {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	cur, done := 0, false
	for i, l := range lines {
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) != nil {
			t.Fatal(l)
		}
		if m["verb"] == "gen_step" {
			res, _ := m["result"].(map[string]any)
			n, _ := res["opno"].(float64)
			cur = int(n)
			continue
		}
		if cur == opno && m["kind"] == "complete" && !done {
			m["outcome"] = outcome
			nb, _ := json.Marshal(m)
			lines[i], done = string(nb), true
		}
	}
	if !done {
		t.Fatalf("no completion of operation %d", opno)
	}
	p := filepath.Join(t.TempDir(), "tampered.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// replayMissing replays src with a fake whose operations do not hold for a
// kill and take 1 ms: every kill timed into an operation lands after it.
func replayMissing(t *testing.T, src string) (map[string]string, string, error) {
	t.Helper()
	f := &faultFake{opDelay: time.Millisecond, noHold: true}
	f.install(t)
	actx, p := genRunCtx(t)
	out, err := genRunner("v3_gen_replay", "host", &blockModel{}).Replay(context.Background(), actx, src)
	_ = actx.History.Close()
	return out, p, err
}

// inOpOps are the night's in-operation kills that hit: {opno: volume}.
func inOpOps(recs []tr.HistoryRecord) map[int]string {
	out := map[int]string{}
	for _, r := range recs {
		if r.Verb == "fault_during" && r.Result["hit"] == true {
			out[int(r.Result["opno"].(float64))] = r.Volume
		}
	}
	return out
}

// The replay rule for an in-operation fault whose hit differs: its own
// line ("in-op fault hit differs at op N (night: hit, replay: missed)",
// both outcomes), counted in the answer, never a silent match
// (match_inop_hit_differs, not match) and not a failure by itself.
func TestReplayReportsAnInOpHitThatDiffers(t *testing.T) {
	recs, src, _ := inOpRun(t, "1")
	ops := inOpOps(recs)
	if len(ops) != 2 {
		t.Fatalf("the night's in-op kills that hit: %v", ops)
	}
	out, p, err := replayMissing(t, src)
	if err != nil || out["value"] != "match_inop_hit_differs_unjudged" || out["inop_hit_differs"] != "2" {
		t.Fatalf("%v %v", out, err)
	}
	rep := lastVerb(t, p, "gen_replay").Result
	lines, _ := rep["inop_hit_differs"].([]any)
	if len(lines) != 2 || len(rep["differences"].([]any)) != 0 {
		t.Fatalf("%v", rep)
	}
	for _, l := range lines {
		m := l.(map[string]any)
		if _, ok := ops[int(m["opno"].(float64))]; !ok || m["night_outcome"] != tr.OutcomeUnknown || m["replay_outcome"] != tr.OutcomeOK ||
			!strings.HasPrefix(m["line"].(string), fmt.Sprintf("in-op fault hit differs at op %v (night: hit, replay: missed)", m["opno"])) {
			t.Errorf("line %v", m)
		}
	}
	// A replay whose hits match still answers match, with none.
	f := &faultFake{opDelay: fakeOpDelay}
	f.install(t)
	actx, _ := genRunCtx(t)
	if out, err := genRunner("v3_gen_replay", "host", &blockModel{}).Replay(context.Background(), actx, src); err != nil || out["value"] != "match_unjudged" || out["inop_hit_differs"] != "0" {
		t.Fatalf("plain replay: %v %v", out, err)
	}
	_ = actx.History.Close()
}

// Red controls of the replay rule: a difference that is not at an in-op
// fault and not downstream of one on the same volume fails the replay as
// before (another volume; the same volume before the fault); one
// downstream on the same volume is in the fault's line.
func TestReplayInOpHitDiffersOnlyExcusesItsOwnVolumeDownstream(t *testing.T) {
	recs, src, _ := inOpRun(t, "1")
	ops := inOpOps(recs)
	first, vol := 0, ""
	for no, v := range ops {
		if first == 0 || no < first {
			first, vol = no, v
		}
	}
	steps := map[int]string{}
	for _, r := range recs {
		if r.Verb == "gen_step" {
			steps[int(r.Result["opno"].(float64))], _ = r.Result["params"].(map[string]any)["volume"].(string)
		}
	}
	completes := map[int]bool{}
	cur := 0
	for _, r := range recs {
		if r.Verb == "gen_step" {
			cur = int(r.Result["opno"].(float64))
		} else if r.Kind == tr.HistoryComplete && r.Outcome == tr.OutcomeOK {
			completes[cur] = true
		}
	}
	pick := func(same, after bool) int {
		best := 0
		for no, v := range steps {
			if _, inop := ops[no]; inop || v == "" || !completes[no] || (v == vol) != same || (no > first) != after {
				continue
			}
			if best == 0 || (after && no < best) || (!after && no > best) {
				best = no
			}
		}
		if best == 0 {
			t.Fatalf("no step (same volume %v, after %v)", same, after)
		}
		return best
	}
	other := pick(false, true)
	if out, _, err := replayMissing(t, tamper(t, src, other, tr.OutcomeRefused)); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("operation %d ", other)) {
		t.Fatalf("another volume, op %d: %v %v", other, out, err)
	}
	before := pick(true, false)
	if out, _, err := replayMissing(t, tamper(t, src, before, tr.OutcomeRefused)); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("operation %d ", before)) {
		t.Fatalf("same volume before, op %d: %v %v", before, out, err)
	}
	after := pick(true, true)
	out, p, err := replayMissing(t, tamper(t, src, after, tr.OutcomeRefused))
	if err != nil || out["value"] != "match_inop_hit_differs_unjudged" {
		t.Fatalf("same volume after, op %d: %v %v", after, out, err)
	}
	found := false
	for _, l := range lastVerb(t, p, "gen_replay").Result["inop_hit_differs"].([]any) {
		for _, d := range l.(map[string]any)["downstream"].([]any) {
			found = found || int(d.(map[string]any)["opno"].(float64)) == after
		}
	}
	if !found {
		t.Fatalf("operation %d not downstream in the line", after)
	}
}
