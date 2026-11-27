package randomrun

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	tr "github.com/pingqiu/sw-test-runner"
)

// A test product: actions create, attach, write, read, snap, snapdel, stop;
// observe, kill, restart and healthy for the Driver. No product name.

type testDriver struct{}

func (testDriver) Version(faults bool) string     { return "test/1" }
func (testDriver) Calls(o Op) []map[string]string { return []map[string]string{o.Params} }
func (testDriver) Observe(s string) Op {
	return Op{Action: "observe", Volume: s, Params: map[string]string{"volume": s}}
}
func (testDriver) Fault(e FaultEvent) Op {
	return Op{Action: e.Action, Params: map[string]string{"process": e.Process}}
}
func (testDriver) ValidProcess(p string) bool { return testProcess(p) }
func (testDriver) Healthy(wait string) Op {
	return Op{Action: "healthy", Params: map[string]string{"wait": wait}}
}
// A loop schedules no master outage: these three are never called here.
func (testDriver) MasterOutage() (string, int, bool) { return "", 0, false }
func (testDriver) Look() Op {
	return Op{Action: "healthy", Params: map[string]string{"look": LookSingle}}
}
func (testDriver) OutageRestart() Op { return Op{Action: "restart"} }
func (testDriver) Resolution(o Op) (*Op, []string) {
	if o.Action == "write" {
		return &Op{Action: "read", Volume: o.Volume, Params: map[string]string{"volume": o.Volume, "lbas": o.Params["lbas"]}}, nil
	}
	return nil, []string{o.Volume}
}

// fakeLoopProduct answers ok, except: a write while a process is down is
// unknown, a write of a subject in refuse is refused, flip turns the n-th
// answer of an action (from 1) into refused.
type fakeLoopProduct struct {
	down   string
	refuse map[string]bool
	flip   map[string]int
	seen   map[string]int
}

func (f *fakeLoopProduct) handlers() map[string]tr.ActionHandlerFunc {
	f.seen = map[string]int{}
	h := map[string]tr.ActionHandlerFunc{}
	for _, name := range []string{"create", "attach", "write", "read", "snap", "snapdel", "stop", "kill", "restart"} {
		name := name
		h[name] = func(_ context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
			f.seen[name]++
			out := tr.OutcomeOK
			switch {
			case name == "kill":
				f.down = act.Params["process"]
			case name == "restart":
				f.down = ""
			case name == "write" && f.refuse[act.Params["volume"]]:
				out = tr.OutcomeRefused
			case name == "write" && f.down != "":
				out = tr.OutcomeUnknown
			}
			if f.flip[name] != 0 && f.seen[name] == f.flip[name] {
				out = tr.OutcomeRefused
			}
			seq, _ := actx.History.Invoke(tr.HistoryRecord{Op: "T." + name, Verb: name, Volume: act.Params["volume"], Args: map[string]any{"lbas": act.Params["lbas"]}})
			_ = actx.History.Complete(seq, out, nil)
			return map[string]string{"value": out}, nil
		}
	}
	h["observe"] = func(_ context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
		_ = actx.History.Observe(tr.HistoryObserve, tr.HistoryRecord{Verb: "observe", Volume: act.Params["volume"]})
		return map[string]string{"value": "observed"}, nil
	}
	h["healthy"] = func(_ context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
		ok := f.down == ""
		_ = actx.History.Observe(tr.HistoryObserve, tr.HistoryRecord{Verb: "healthy", Result: map[string]any{"healthy": ok}})
		return map[string]string{"value": "observed", "healthy": fmt.Sprint(ok)}, nil
	}
	return h
}

const testLoopJSON = `{
  "product": "test",
  "loops": {
    "t": {
      "lbas": 2,
      "setup": [{"action": "create", "params": {"volume": "{subject}"}}, {"action": "attach", "params": {"volume": "{subject}"}}],
      "cycle": [
        {"action": "write", "params": {"volume": "{subject}", "lbas": "{lbas}", "round": "i{iteration}"}, "writes_lbas": true},
        {"action": "read", "params": {"volume": "{subject}", "lbas": "{lbas}"}},
        {"action": "snap", "every": 3, "params": {"volume": "{subject}", "name": "{subject}-s{iteration}"}},
        {"action": "snapdel", "every": 3, "back": 6, "params": {"volume": "{subject}", "name": "{subject}-s{iteration}"}}
      ],
      "sweep": {"action": "read", "params": {"volume": "{subject}", "lbas": "{written}"}},
      "teardown": [{"action": "stop", "params": {"volume": "{subject}"}}]
    }
  }
}`

func testLoop(t *testing.T) Loop {
	t.Helper()
	known := (&fakeLoopProduct{}).handlers()
	f, err := ParseLoops([]byte(testLoopJSON), func(a string) bool { _, ok := known[a]; return ok })
	if err != nil {
		t.Fatal(err)
	}
	return f.Loops["t"]
}

func loopCtx(t *testing.T) (*tr.ActionContext, string) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "history.jsonl")
	h, err := tr.OpenHistory(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return &tr.ActionContext{History: h, Bundle: &tr.RunBundle{Dir: dir}, Vars: map[string]string{}, Log: func(string, ...interface{}) {}}, p
}

func readRecs(t *testing.T, p string) []tr.HistoryRecord {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []tr.HistoryRecord
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 1<<20), 16<<20)
	for s.Scan() {
		var r tr.HistoryRecord
		if err := json.Unmarshal(s.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// runLoop runs the test loop and returns the closed history's path.
func runLoop(t *testing.T, f *fakeLoopProduct, c LoopConfig, mutant string) (string, map[string]string) {
	t.Helper()
	actx, p := loopCtx(t)
	r := LoopRunner{Runner: Runner{Name: "loop", Node: "n", Driver: testDriver{}, Handlers: f.handlers()}, mutant: mutant}
	out, err := r.RunLoop(context.Background(), actx, c)
	if err != nil {
		t.Fatal(err)
	}
	_ = actx.History.Close()
	return p, out
}

func baseConfig(t *testing.T) LoopConfig {
	return LoopConfig{Name: "t", Loop: testLoop(t), Seed: 11, Pool: []string{"a", "b"}, Blocks: 8, Iterations: 9}
}

func faultConfig(t *testing.T) LoopConfig {
	c := baseConfig(t)
	c.Iterations, c.Faults, c.FaultProcesses, c.RestartWithin = 20, 0.3, []string{"p1", "p0"}, 3
	return c
}

// stepLines lists the gen_step records as "opno action volume k=v ...".
func stepLines(recs []tr.HistoryRecord) []string {
	var out []string
	for _, r := range recs {
		if r.Verb != "gen_step" {
			continue
		}
		params, _ := r.Result["params"].(map[string]any)
		keys := make([]string, 0, len(params))
		for k := range params {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		s := fmt.Sprintf("%v %v %s", r.Result["opno"], r.Result["action"], r.Volume)
		for _, k := range keys {
			s += fmt.Sprintf(" %s=%v", k, params[k])
		}
		out = append(out, s)
	}
	return out
}

func digest(lines []string) string {
	s := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(s[:8])
}

func TestLoopRunSameSeedSameRecordsAndPinned(t *testing.T) {
	a, _ := runLoop(t, &fakeLoopProduct{}, faultConfig(t), "")
	b, _ := runLoop(t, &fakeLoopProduct{}, faultConfig(t), "")
	la, lb := stepLines(readRecs(t, a)), stepLines(readRecs(t, b))
	if !slices.Equal(la, lb) {
		t.Fatal("the same seed gave two step sequences")
	}
	// Everything but the clock is the same record for record.
	strip := func(rs []tr.HistoryRecord) []string {
		var out []string
		for _, r := range rs {
			r.MonoNs = 0
			b, _ := json.Marshal(r)
			out = append(out, string(b))
		}
		return out
	}
	if !slices.Equal(strip(readRecs(t, a)), strip(readRecs(t, b))) {
		t.Fatal("the same seed gave two histories")
	}
	c := faultConfig(t)
	c.Seed = 12
	o, _ := runLoop(t, &fakeLoopProduct{}, c, "")
	if slices.Equal(la, stepLines(readRecs(t, o))) {
		t.Fatal("another seed gave the same lbas")
	}
	// Pinned: a change to the expansion or the lbas draw changes every recorded
	// loop history, so it must come with a new LoopDriverVersion.
	if got := digest(la); got != "c6283e7105e1508d" || len(la) != 110 {
		t.Fatalf("seed 11: %d steps, digest %s; first steps:\n%s", len(la), got, strings.Join(la[:12], "\n"))
	}
}

func TestLoopRunFollowsTheDeclaredOrderAndCounts(t *testing.T) {
	p, out := runLoop(t, &fakeLoopProduct{}, baseConfig(t), "")
	recs := readRecs(t, p)
	if recs[0].Verb != "loop_header" || recs[0].Result["driver"] != LoopDriverVersion || recs[0].Result["declaration_sha256"] != testLoop(t).Hash() {
		t.Fatalf("header %+v", recs[0])
	}
	if out["value"] != "iterations" || out["iterations"] != "9" {
		t.Fatalf("out %v", out)
	}
	var phases []string
	counts := map[string]int{}
	cur := ""
	var cov map[string]any
	for _, r := range recs {
		switch r.Verb {
		case "loop_phase":
			cur = r.Result["phase"].(string)
			phases = append(phases, cur)
		case "gen_step":
			counts[cur+"/"+r.Result["action"].(string)]++
		case "loop_coverage":
			cov = r.Result
		}
	}
	if !slices.Equal(phases, loopPhases) {
		t.Fatalf("phases %v", phases)
	}
	// 2 subjects; snap at 3, 6, 9; snapdel at 9 only (back 6 names iteration 3).
	want := map[string]int{"setup/create": 2, "setup/attach": 2, "cycle/write": 18, "cycle/read": 18, "cycle/snap": 6,
		"cycle/snapdel": 2, "sweep/read": 2, "sweep/observe": 2, "teardown/stop": 2}
	if fmt.Sprint(counts) != fmt.Sprint(want) {
		t.Fatalf("counts %v\nwant   %v", counts, want)
	}
	// Iteration 9 of subject a, in the declared order.
	lines := stepLines(recs)
	var it9 []string
	for _, l := range lines {
		if strings.Contains(l, " a ") && (strings.Contains(l, "round=i9") || strings.Contains(l, "name=a-s")) {
			it9 = append(it9, l)
		}
	}
	tail := it9[len(it9)-3:]
	if !strings.Contains(tail[0], "write a") || !strings.Contains(tail[1], "snap a name=a-s9") || !strings.Contains(tail[2], "snapdel a name=a-s3") {
		t.Fatalf("iteration 9 of a: %v", it9)
	}
	if cov["iterations"] != float64(9) || cov["declared_never_run"] == nil {
		t.Fatalf("coverage %v", cov)
	}
	if err := CheckLoop(p, testDriver{}.Observe, testProcess); err != nil {
		t.Fatalf("green: %v", err)
	}
	// A shorter run never reaches snapdel: the coverage says so.
	c := baseConfig(t)
	c.Iterations = 5
	p2, _ := runLoop(t, &fakeLoopProduct{}, c, "")
	for _, r := range readRecs(t, p2) {
		if r.Verb == "loop_coverage" && fmt.Sprint(r.Result["declared_never_run"]) != "[snapdel]" {
			t.Fatalf("coverage %v", r.Result)
		}
	}
}

func TestLoopRunInjectsAndResolvesFaults(t *testing.T) {
	c := faultConfig(t)
	p, _ := runLoop(t, &fakeLoopProduct{}, c, "")
	recs := readRecs(t, p)
	if recs[1].Verb != "fault_schedule" {
		t.Fatalf("second record %+v", recs[1])
	}
	sched, err := NewFaultSchedule(c.Seed, c.faultOps(), []string{"p0", "p1"}, c.Faults, c.RestartWithin, testProcess)
	if err != nil || len(sched.Events) == 0 {
		t.Fatalf("schedule %v %v", sched, err)
	}
	if recs[0].Result["fault_ops"] != float64(c.faultOps()) || c.faultOps() != 2*(20*2+6+4) {
		t.Fatalf("fault_ops %v, %d", recs[0].Result["fault_ops"], c.faultOps())
	}
	kills, restarts, healthyAfterRestart, unknown, resolved := 0, 0, 0, 0, 0
	phase, inStep, lastRestart := "", false, false
	for _, r := range recs {
		switch {
		case r.Verb == "loop_phase":
			phase = r.Result["phase"].(string)
		case r.Verb == "gen_step":
			inStep = true
		case r.Kind == tr.HistoryInvoke && r.Verb == FaultKill:
			if phase != PhaseCycle {
				t.Fatalf("a kill in phase %s", phase)
			}
			kills++
			inStep = false
		case r.Kind == tr.HistoryInvoke && r.Verb == FaultRestart:
			restarts++
			inStep, lastRestart = false, true
		case r.Verb == "healthy":
			if lastRestart && r.Result["healthy"] == true {
				healthyAfterRestart++
			}
			inStep, lastRestart = false, false
		case r.Kind == tr.HistoryComplete && r.Outcome == tr.OutcomeUnknown:
			unknown++
		case r.Kind == tr.HistoryComplete && r.Verb == "read" && !inStep:
			resolved++ // a resolving read: after a healthy point, under no gen_step
		}
	}
	var wantKills int
	for _, e := range sched.Events {
		if e.Action == FaultKill {
			wantKills++
		}
	}
	if kills != wantKills || restarts != wantKills || healthyAfterRestart != wantKills {
		t.Fatalf("kills %d restarts %d healthy %d, schedule has %d kills", kills, restarts, healthyAfterRestart, wantKills)
	}
	if unknown == 0 || resolved != unknown {
		t.Fatalf("%d unknown writes, %d resolving reads", unknown, resolved)
	}
	if err := CheckLoop(p, testDriver{}.Observe, testProcess); err != nil {
		t.Fatalf("green: %v", err)
	}
}

func TestLoopRunSweepReadsEveryWrittenLBA(t *testing.T) {
	// b's writes are refused: nothing of b is written, so the sweep reads a only.
	f := &fakeLoopProduct{refuse: map[string]bool{"b": true}}
	p, _ := runLoop(t, f, baseConfig(t), "")
	recs := readRecs(t, p)
	written, swept := map[string]bool{}, map[string]bool{}
	observed := map[string]bool{}
	phase := ""
	for _, r := range recs {
		if r.Verb == "loop_phase" {
			phase = r.Result["phase"].(string)
		}
		if r.Verb != "gen_step" {
			continue
		}
		params := r.Result["params"].(map[string]any)
		switch {
		case phase == PhaseCycle && r.Result["action"] == "write" && r.Volume == "a":
			for _, l := range strings.Fields(params["lbas"].(string)) {
				written[r.Volume+"/"+l] = true
			}
		case phase == PhaseSweep && r.Result["action"] == "read":
			for _, l := range strings.Fields(params["lbas"].(string)) {
				swept[r.Volume+"/"+l] = true
			}
		case phase == PhaseSweep && r.Result["action"] == "observe":
			observed[r.Volume] = true
		}
	}
	if len(written) == 0 || fmt.Sprint(written) != fmt.Sprint(swept) {
		t.Fatalf("written %v\nswept   %v", written, swept)
	}
	if !observed["a"] || !observed["b"] {
		t.Fatalf("observed %v", observed)
	}
	if err := CheckLoop(p, testDriver{}.Observe, testProcess); err != nil {
		t.Fatalf("green: %v", err)
	}
}

func TestLoopReplayRoundTrip(t *testing.T) {
	src, _ := runLoop(t, &fakeLoopProduct{}, faultConfig(t), "")
	replay := func(f *fakeLoopProduct, src string) (string, error) {
		actx, p := loopCtx(t)
		r := LoopRunner{Runner: Runner{Name: "replay", Node: "n", Driver: testDriver{}, Handlers: f.handlers()}}
		_, err := r.ReplayLoop(context.Background(), actx, src)
		_ = actx.History.Close()
		return p, err
	}
	p, err := replay(&fakeLoopProduct{}, src)
	if err != nil {
		t.Fatal(err)
	}
	// The replayed history has the recorded one's steps, and is itself a loop
	// history that checks and replays.
	if !slices.Equal(stepLines(readRecs(t, src)), stepLines(readRecs(t, p))) {
		t.Fatal("the replay ran other steps")
	}
	if err := CheckLoop(p, testDriver{}.Observe, testProcess); err != nil {
		t.Fatalf("replayed history: %v", err)
	}
	if _, err := replay(&fakeLoopProduct{}, p); err != nil {
		t.Fatalf("replay of the replay: %v", err)
	}
	// Red control: a product that answers one read otherwise is a difference.
	if _, err := replay(&fakeLoopProduct{flip: map[string]int{"read": 5}}, src); err == nil || !strings.Contains(err.Error(), "differ") {
		t.Fatalf("a changed outcome replayed as a match: %v", err)
	}
	// Red control: a gen history is not a loop history.
	gen := filepath.Join(t.TempDir(), "gen.jsonl")
	_ = os.WriteFile(gen, []byte(`{"seq":1,"kind":"observe","verb":"gen_header","result":{"generator":"x"},"mono_ns":0}`+"\n"), 0o644)
	if _, err := replay(&fakeLoopProduct{}, gen); err == nil {
		t.Fatal("a gen history was replayed as a loop")
	}
}

// Red controls of the driver: each mutant's history must fail CheckLoop.
func TestLoopMutantsAreCaught(t *testing.T) {
	for mutant, want := range map[string]string{"skip-sweep": "sweep", "swap-cycle": "cycle iteration 1"} {
		p, _ := runLoop(t, &fakeLoopProduct{}, faultConfig(t), mutant)
		err := CheckLoop(p, testDriver{}.Observe, testProcess)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("mutant %s: %v", mutant, err)
		}
	}
	// A history whose declaration was edited after the run.
	p, _ := runLoop(t, &fakeLoopProduct{}, baseConfig(t), "")
	b, _ := os.ReadFile(p)
	edited := filepath.Join(t.TempDir(), "edited.jsonl")
	_ = os.WriteFile(edited, []byte(strings.Replace(string(b), `"every":3`, `"every":4`, 1)), 0o644)
	if err := CheckLoop(edited, testDriver{}.Observe, testProcess); err == nil || !strings.Contains(err.Error(), "hash") {
		t.Errorf("an edited declaration: %v", err)
	}
}

// Red controls of the declaration: each is refused at load.
func TestLoopDeclarationRefusedAtLoad(t *testing.T) {
	known := (&fakeLoopProduct{}).handlers()
	parse := func(s string) error {
		_, err := ParseLoops([]byte(s), func(a string) bool { _, ok := known[a]; return ok })
		return err
	}
	if err := parse(testLoopJSON); err != nil {
		t.Fatalf("green: %v", err)
	}
	for name, c := range map[string]struct{ old, new, want string }{
		"an unregistered action": {`"action": "snapdel"`, `"action": "snapshot_remove"`, "not registered"},
		"a template naming an unknown field": {`"snap", "every": 3, "params": {"volume": "{subject}", "name": "{subject}-s{iteration}"}`,
			`"snap", "every": 3, "params": {"volume": "{subject}", "name": "{subject}-s{iter}"}`, "unknown field {iter}"},
		"the sweep's field in the cycle": {`"lbas": "{lbas}"}}`, `"lbas": "{written}"}}`, "unknown field {written}"},
		"an unmatched brace":             {`"round": "i{iteration}"`, `"round": "i{iteration"`, "unmatched"},
		"an unknown key":                 {`"every": 3, "back": 6`, `"evry": 3, "back": 6`, "unknown field"},
		"back not a multiple of every":   {`"back": 6`, `"back": 4`, "multiple"},
		"lbas used but not declared":     {`"lbas": 2,`, ``, "unknown field {lbas}"},
		"writes without a sweep":         {`"sweep": {"action": "read", "params": {"volume": "{subject}", "lbas": "{written}"}},`, ``, "no sweep"},
		"cadence on a setup step":        {`{"action": "create", "params"`, `{"action": "create", "every": 2, "params"`, "cycle steps"},
	} {
		s := strings.Replace(testLoopJSON, c.old, c.new, 1)
		if s == testLoopJSON {
			t.Fatalf("%s: the mutation did not apply", name)
		}
		if err := parse(s); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestLoopConfigAndDuration(t *testing.T) {
	r := LoopRunner{Runner: Runner{Name: "loop", Node: "n", Driver: testDriver{}, Handlers: (&fakeLoopProduct{}).handlers()}}
	for name, mod := range map[string]func(*LoopConfig){
		"neither iterations nor duration": func(c *LoopConfig) { c.Iterations = 0 },
		"faults without iterations":       func(c *LoopConfig) { c.Iterations, c.Duration = 0, time.Hour },
		"no pool":                         func(c *LoopConfig) { c.Pool = nil },
		"blocks below the draw":           func(c *LoopConfig) { c.Blocks = 1 },
		"an unknown process":              func(c *LoopConfig) { c.FaultProcesses = []string{"q"} },
	} {
		c := faultConfig(t)
		mod(&c)
		actx, _ := loopCtx(t)
		if _, err := r.RunLoop(context.Background(), actx, c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := ParseLoopConfig("x", func(k string) string { return map[string]string{"seed": "1", "duration": "soon"}[k] }); err == nil {
		t.Error("a bad duration accepted")
	}
	// A duration ends the cycle at an iteration boundary; the check accepts it.
	clock := time.Unix(0, 0)
	r.Now = func() time.Time { clock = clock.Add(time.Second); return clock }
	c := baseConfig(t)
	c.Iterations, c.Duration = 0, 5*time.Second
	actx, p := loopCtx(t)
	out, err := r.RunLoop(context.Background(), actx, c)
	if err != nil {
		t.Fatal(err)
	}
	_ = actx.History.Close()
	if out["value"] != "duration" || out["iterations"] != "4" {
		t.Fatalf("out %v", out)
	}
	if err := CheckLoop(p, testDriver{}.Observe, testProcess); err != nil {
		t.Fatalf("duration run: %v", err)
	}
}
