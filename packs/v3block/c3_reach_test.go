package v3block

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/randomrun"
)

// blockStore is a correct product's blocks behind the fakes: each volume's
// content by lba, each snapshot's, recorded in the shapes the declared
// actions record (product/actions.json: a write's lba and sha256, one read
// record per lba with the sha256 it read, a restore under its target with
// its source and snapshot), so the data checkers can judge a fake run.
type blockStore struct {
	mu    sync.Mutex
	data  map[string]map[string]string // volume -> lba -> sha256
	snaps map[string]map[string]string // volume|snapshot -> its content
}

func newBlockStore() *blockStore {
	return &blockStore{data: map[string]map[string]string{}, snaps: map[string]map[string]string{}}
}

// zeroBlock is the sha256 of a block never written (4096 zero bytes, the
// checkers' ZERO).
var zeroBlock = func() string { s := sha256.Sum256(make([]byte, 4096)); return hex.EncodeToString(s[:]) }()

// blockSHA is the content a write of lba in round writes on vol: unique to
// the three, as the runner's repeated tag is.
func blockSHA(vol, round, lba string) string {
	s := sha256.Sum256([]byte(vol + "|" + round + "|" + lba + "|"))
	return hex.EncodeToString(s[:])
}

// args are the record args of one call (merged over base).
func (b *blockStore) args(name string, act tr.Action, base map[string]any) map[string]any {
	out := maps.Clone(base)
	if out == nil {
		out = map[string]any{}
	}
	p := act.Params
	switch name {
	case "v3_op_write":
		n, _ := strconv.Atoi(p["lbas"])
		out["lba"], out["sha256"], out["round"] = n, blockSHA(p["volume"], p["round"], p["lbas"]), p["round"]
	case "v3_op_snapshot":
		out["name"], out["request"] = p["name"], p["volume"]+"-"+p["name"]
	case "v3_op_restore":
		out["source"], out["snapshot"] = p["volume"], p["snapshot"]
	}
	return out
}

// apply is a call's effect on the blocks: an unknown write or snapshot is
// taken as having happened (a product may have applied it).
func (b *blockStore) apply(name string, act tr.Action, outcome string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	p := act.Params
	if outcome != tr.OutcomeOK && (outcome != tr.OutcomeUnknown || (name != "v3_op_write" && name != "v3_op_snapshot")) {
		return
	}
	switch name {
	case "v3_op_create":
		b.data[p["volume"]] = map[string]string{}
	case "v3_op_write":
		if b.data[p["volume"]] == nil {
			b.data[p["volume"]] = map[string]string{}
		}
		b.data[p["volume"]][p["lbas"]] = blockSHA(p["volume"], p["round"], p["lbas"])
	case "v3_op_snapshot":
		b.snaps[p["volume"]+"|"+p["name"]] = maps.Clone(b.data[p["volume"]])
	case "v3_op_restore":
		b.data[p["target_volume"]] = maps.Clone(b.snaps[p["volume"]+"|"+p["snapshot"]])
	case "v3_op_delete":
		delete(b.data, p["volume"])
	}
}

// record is one call's records with its answer out: a read is one
// invoke/complete per lba, each ok one with the sha256 the volume holds.
func (b *blockStore) record(actx *tr.ActionContext, name string, act tr.Action, out string) {
	verb := strings.TrimPrefix(name, "v3_op_")
	vol := act.Params["volume"]
	if name == "v3_op_read" {
		for _, l := range strings.Fields(act.Params["lbas"]) {
			n, _ := strconv.Atoi(l)
			seq, _ := actx.History.Invoke(tr.HistoryRecord{Op: "X." + verb, Verb: verb, Volume: vol, Args: map[string]any{"lba": n}})
			var res map[string]any
			if out == tr.OutcomeOK {
				b.mu.Lock()
				sha, ok := b.data[vol][l]
				b.mu.Unlock()
				if !ok {
					sha = zeroBlock
				}
				res = map[string]any{"sha256": sha}
			}
			_ = actx.History.Complete(seq, out, res)
		}
		return
	}
	if name == "v3_op_restore" {
		vol = act.Params["target_volume"]
	}
	seq, _ := actx.History.Invoke(tr.HistoryRecord{Op: "X." + verb, Verb: verb, Volume: vol, Args: b.args(name, act, nil)})
	b.apply(name, act, out)
	_ = actx.History.Complete(seq, out, nil)
}

// nightAct is the M2 fault run of scenarios/v3block/m2-faults.yaml (its
// defaults: 300 operations, 16 blocks, 32 names, the kill rate, the four
// holders, the master outage rate, the in-operation faults) at one seed.
func nightAct(seed uint64) tr.Action {
	a := inOpAct(strconv.FormatUint(seed, 10))
	a.Params["blocks"] = "16"
	a.Params["pool"] = strings.Join(pool(32), " ")
	a.Params["fault_restart_within"] = "20"
	a.Params["fault_master_outage"] = "0.02"
	return a
}

// c3Verdict is C3 as the checker (checkers/history_check.py) judges a
// history file.
type c3Verdict struct {
	Verdict  string   `json:"verdict"`
	Judged   int      `json:"judged"`
	Findings []string `json:"findings"`
}

func judgeC3(t *testing.T, history string) c3Verdict {
	t.Helper()
	py, err := exec.LookPath("python3")
	if err != nil {
		if py, err = exec.LookPath("python"); err != nil {
			t.Skip("no python: the checker cannot judge the fake run")
		}
	}
	out := filepath.Join(filepath.Dir(history), "checks.json")
	cmd := exec.Command(py, "-I", filepath.Join("..", "..", "checkers", "history_check.py"), history, "--json", out, "--checkers", "C3")
	if b, err := cmd.CombinedOutput(); err != nil {
		if _, ok := err.(*exec.ExitError); !ok {
			t.Fatalf("checker: %v\n%s", err, b)
		}
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var all map[string]c3Verdict
	if err := json.Unmarshal(b, &all); err != nil {
		t.Fatal(err)
	}
	return all["C3"]
}

// nightRun is one fake fault run over blocks a correct product keeps; it
// returns the history file.
func nightRun(t *testing.T, seed uint64) string {
	t.Helper()
	f := &faultFake{opDelay: fakeOpDelay}
	f.content, f.pool = newBlockStore(), pool(32)
	f.install(t)
	actx, p := genRunCtx(t)
	if _, err := v3GenRun(context.Background(), actx, nightAct(seed)); err != nil {
		t.Fatal(err)
	}
	_ = actx.History.Close()
	return p
}

// forcedOnly is the history with every read that the model did not force
// removed (the reads of drawn steps and of the end-of-run sweep): what C3
// judges on it is exercised BY CONSTRUCTION, not by the luck of a draw.
// Writes stay (a later write can only mask a direction, never exercise one).
func forcedOnly(t *testing.T, src string) string {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	drop := map[float64]bool{}
	forced := true // records outside any step (fault events) are kept
	for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		var r map[string]any
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatal(err)
		}
		switch {
		case r["verb"] == "gen_step":
			res, _ := r["result"].(map[string]any)
			forced = res["forced"] != nil
		case r["verb"] == "gen_sweep":
			forced = false
		case r["kind"] == "invoke" && r["verb"] == "read" && !forced:
			drop[r["seq"].(float64)] = true
			continue
		case r["kind"] == "complete" && drop[r["of"].(float64)]:
			continue
		}
		out = append(out, line)
	}
	// Sequence numbers stay consecutive (the checker reads them so): each
	// record and each completion's link are renumbered.
	renum := map[float64]float64{}
	for i, line := range out {
		var r map[string]any
		_ = json.Unmarshal([]byte(line), &r)
		renum[r["seq"].(float64)] = float64(i + 1)
		r["seq"] = i + 1
		if of, ok := r["of"].(float64); ok {
			r["of"] = renum[of]
		}
		b, _ := json.Marshal(r)
		out[i] = string(b)
	}
	p := filepath.Join(t.TempDir(), "forced-history.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(out, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// unexercised is C3's "not exercised" findings.
func unexercised(v c3Verdict) []string {
	var out []string
	for _, f := range v.Findings {
		if strings.Contains(f, "not exercised") {
			out = append(out, f)
		}
	}
	return out
}

// nightSeeds are the five runs of the night of 2026-11-27 as m1-nightly
// derives them.
var nightSeeds = randomrun.NightlySeeds(20261127, 5)

// Every restore pair of a fake M2 night run is exercised in both
// directions, and C3 judges, on every nightly seed: in the whole history,
// and BY CONSTRUCTION (only the reads the model forced kept). Red: the pair
// policy before (oldPairs) leaves a pair unexercised by construction on at
// least one of the same seeds.
func TestEveryRestorePairIsExercisedByConstruction(t *testing.T) {
	for _, seed := range nightSeeds {
		p := nightRun(t, seed)
		full, forced := judgeC3(t, p), judgeC3(t, forcedOnly(t, p))
		t.Logf("seed %d: C3 %s judged %d; forced reads only: %s judged %d %v", seed, full.Verdict, full.Judged, forced.Verdict, forced.Judged, forced.Findings)
		if full.Verdict != "PASS" || full.Judged == 0 {
			t.Errorf("seed %d: C3 %s judged %d: %v", seed, full.Verdict, full.Judged, full.Findings)
		}
		if bad := unexercised(forced); len(bad) != 0 || forced.Judged == 0 {
			t.Errorf("seed %d: by construction: judged %d, %v", seed, forced.Judged, bad)
		}
	}
	oldPairs = true
	defer func() { oldPairs = false }()
	red := 0
	for _, seed := range nightSeeds {
		p := nightRun(t, seed)
		full, forced := judgeC3(t, p), judgeC3(t, forcedOnly(t, p))
		t.Logf("old policy, seed %d: C3 %s judged %d; forced reads only: %s judged %d %v", seed, full.Verdict, full.Judged, forced.Verdict, forced.Judged, unexercised(forced))
		if len(unexercised(forced)) > 0 {
			red++
		}
	}
	if red == 0 {
		t.Fatal("red control: the old pair policy exercised every pair by construction on every nightly seed")
	}
}

// A run with forced C3 steps replays exactly: the replay re-executes the
// same steps, the pair windows and their retries among them, with the same
// outcomes.
func TestGenReplayOfARunWithForcedC3StepsMatches(t *testing.T) {
	f := &faultFake{opDelay: fakeOpDelay}
	f.content, f.pool = newBlockStore(), pool(32)
	f.install(t)
	actx, a := genRunCtx(t)
	if _, err := v3GenRun(context.Background(), actx, nightAct(nightSeeds[0])); err != nil {
		t.Fatal(err)
	}
	_ = actx.History.Close()
	pairSteps := 0
	for _, st := range stepsOf(t, a) {
		pairSteps += btoi(st["forced"] == "pair")
	}
	if pairSteps == 0 {
		t.Fatal("no forced pair step in the run")
	}
	f.content = newBlockStore()
	out, b, err := replayInto(t, a)
	if err != nil || out["value"] != "match" {
		t.Fatalf("replay: %v %v %v", out, err, lastVerb(t, b, "gen_replay").Result)
	}
	sa, sb := stepsOf(t, a), stepsOf(t, b)
	if len(sa) != len(sb) {
		t.Fatalf("%d steps recorded, %d replayed", len(sa), len(sb))
	}
	for n, st := range sa {
		if st["forced"] == "pair" && (sb[n]["action"] != st["action"] || sb[n]["forced"] != "pair" || !reflectEqual(sb[n]["params"], st["params"])) {
			t.Fatalf("step %d: recorded %v, replayed %v", n, st, sb[n])
		}
	}
}

func reflectEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// pairReady drives a generator to a restore pair g1|g2 whose target has
// served and read its inherited lbas: the source -> target window is due.
func pairReady(t *testing.T, faulty bool) *Generator {
	t.Helper()
	g, _ := NewGenerator(GenConfig{Seed: 1, Ops: 50, Pool: pool(4), Blocks: 8})
	g.m.faulty = faulty
	drive(g, op("v3_op_create", "g1"), op("v3_op_attach", "g1", "host", "0"), op("v3_op_write", "g1", "lbas", "2 6", "round", "r"),
		op("v3_op_snapshot", "g1", "name", "g1-s1"), op("v3_op_restore", "g1", "snapshot", "g1-s1", "target_volume", "g2"))
	drive(g, d0(g)) // the target's first attach
	drive(g, d0(g)) // its inherited lbas
	return g
}

// A direction whose forced write answered unknown (a fault cut it) is not
// exercised, as C3 judges it: the model forces it again with a new round;
// one whose read was not answered ok is forced again too. After pairTries
// windows it is given up and noted.
func TestAPairDirectionNotExercisedIsForcedAgain(t *testing.T) {
	g := pairReady(t, true)
	w := d0(g)
	if w.Forced != "pair" || w.Action != "v3_op_write" || w.Volume != "g1" || w.Params["round"] != "p-g1-g2-1" {
		t.Fatalf("first window step %+v", w)
	}
	g.Learn(w, tr.OutcomeUnknown)
	r := d0(g)
	if r.Action != "v3_op_read" || r.Volume != "g2" || r.Params["lbas"] != "2" {
		t.Fatalf("second window step %+v", r)
	}
	g.Learn(r, tr.OutcomeOK)
	if g.m.c3["g1|g2"].ex[0] {
		t.Fatal("an unknown write exercised the direction")
	}
	w2 := d0(g)
	if w2.Action != "v3_op_write" || w2.Volume != "g1" || w2.Params["lbas"] != "2" || w2.Params["round"] != "p-g1-g2-1b" {
		t.Fatalf("the retry %+v", w2)
	}
	g.Learn(w2, tr.OutcomeOK)
	r2 := d0(g)
	g.Learn(r2, tr.OutcomeUnknown) // the read did not answer ok
	w3 := d0(g)
	if w3.Params["round"] != "p-g1-g2-1c" {
		t.Fatalf("the second retry %+v", w3)
	}
	g.Learn(w3, tr.OutcomeOK)
	g.Learn(d0(g), tr.OutcomeOK)
	if !g.m.c3["g1|g2"].ex[0] {
		t.Fatal("an ok write read back ok did not exercise the direction")
	}
	if d := g.m.Due(); len(d) > 0 && d[0].Forced == "pair" {
		t.Fatalf("a window after the direction was exercised: %+v", d[0])
	}
	// A masking write: the reader's own write of the block between the two
	// steps leaves the direction not exercised (C3), and it is forced again.
	g2 := pairReady(t, false)
	g2.Learn(d0(g2), tr.OutcomeOK)
	g2.Learn(op("v3_op_write", "g2", "lbas", "2", "round", "m"), tr.OutcomeOK)
	g2.Learn(d0(g2), tr.OutcomeOK)
	if g2.m.c3["g1|g2"].ex[0] {
		t.Fatal("a masked write exercised the direction")
	}
	if d := d0(g2); d.Params["round"] != "p-g1-g2-1b" {
		t.Fatalf("no retry after a masked direction: %+v", d)
	}
	// Given up after pairTries windows, noted.
	g3 := pairReady(t, true)
	for i := 0; i < pairTries; i++ {
		g3.Learn(d0(g3), tr.OutcomeUnknown)
		g3.Learn(d0(g3), tr.OutcomeOK)
	}
	g3.m.Due()
	notes := strings.Join(g3.m.Notes(), "; ")
	if !strings.Contains(notes, "pair not exercised: g1|g2: source -> target after 3 windows") {
		t.Fatalf("notes %q", notes)
	}
}

// A side whose holder was killed, or fenced, is waited for: the pair is not
// dropped, no window step runs on it, and the window runs once it serves.
// A side out of the run (unknown) drops the pair, noted. Red: the old
// policy drops the pair at the kill.
func TestAPairWaitsForAKilledSide(t *testing.T) {
	for _, old := range []bool{false, true} {
		oldPairs = old
		g := pairReady(t, true)
		g.m.vols["g2"].killed = true
		if d := g.m.Due(); len(d) > 0 && d[0].Forced == "pair" && d[0].Volume == "g2" {
			t.Fatalf("a window step on a killed side: %+v", d[0])
		}
		if old {
			if !strings.Contains(strings.Join(g.m.Notes(), ";"), "g1|g2: g2: holder killed") {
				t.Fatalf("red control: the old policy kept the pair: %v", g.m.Notes())
			}
			continue
		}
		if len(g.m.Notes()) != 0 || g.m.vols["g1"].pairDone["g1|g2"] {
			t.Fatalf("the pair was dropped at the kill: %v", g.m.Notes())
		}
		g.m.vols["g2"].killed = false
		g.m.vols["g2"].fenced = true
		if d := g.m.Due(); len(d) > 0 && d[0].Forced == "pair" && d[0].Volume == "g2" {
			t.Fatalf("a window step on a fenced side: %+v", d[0])
		}
		g.m.vols["g2"].fenced = false
		var got []string
		for d := g.m.Due(); len(d) > 0 && d[0].Forced == "pair"; d = g.m.Due() {
			got = append(got, d[0].Action+" "+d[0].Volume)
			g.Learn(d[0], tr.OutcomeOK)
		}
		if strings.Join(got, ",") != "v3_op_write g1,v3_op_read g2" || !g.m.c3["g1|g2"].ex[0] {
			t.Fatalf("the window after the side came back: %v", got)
		}
		g.m.vols["g2"].state = stUnknown
		g.m.Due()
		if !strings.Contains(strings.Join(g.m.Notes(), ";"), "g1|g2: g2: out of the run") {
			t.Fatalf("a side out of the run: %v", g.m.Notes())
		}
	}
	oldPairs = false
}

// The cause on a hardware night (run 3 of the 2026-11-28 rc.3 nights): the
// source -> target window had started (seq 570, the pair write on the
// source) when the store pause's forced target, a snapshot, was proposed on
// the same source (seq 573) and its follow-up stopped it (seq 601), so the
// target -> source read on the source never ran. A pending pair's sides
// stay held while any window step is owed: no in-operation fault target on
// them, and no detach after a store pause. Red: the old policy released
// the hold once the windows were queued.
func TestAPairSideMidWindowIsNoFaultTarget(t *testing.T) {
	for _, old := range []bool{false, true} {
		oldPairs = old
		g := pairReady(t, true)
		for i := 0; i < pairDelay; i++ { // the T -> S window queued (both, under the old policy)
			g.Learn(op("v3_op_read", "g1", "lbas", "6"), tr.OutcomeOK)
		}
		w := d0(g)
		if w.Forced != "pair" || w.Volume != "g1" {
			t.Fatalf("old %v: first window step %+v", old, w)
		}
		g.Learn(w, tr.OutcomeOK) // the source's pair write ran
		tgt, ok := g.m.TargetOp("v3_op_snapshot")
		after := g.m.AfterStorePause(op("v3_op_snapshot", "g1", "name", "g1-s9"))
		stops := slices.ContainsFunc(after, func(o GenOp) bool { return o.Action == "v3_op_stop" })
		if old {
			if !ok || tgt.Volume != "g1" || !stops {
				t.Fatalf("red control: the old policy kept the hold mid-window (target %+v %v, stop %v)", tgt, ok, stops)
			}
			continue
		}
		if ok || stops {
			t.Fatalf("a side mid-window: fault target %+v %v, follow-ups %+v", tgt, ok, after)
		}
	}
	oldPairs = false
}
