package v3block

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/pingqiu/sw-test-runner/randomrun"
)

// pythonFor is the interpreter the tests run the checkers with: python3,
// else python (Windows); the test is skipped without either.
func pythonFor(t *testing.T) string {
	t.Helper()
	for _, p := range []string{"python3", "python"} {
		if _, err := exec.LookPath(p); err == nil {
			return p
		}
	}
	t.Skip("no python3 or python on PATH: the replay's checkers cannot run")
	return ""
}

// recordJudged records a short run whose header names checkers.
func recordJudged(t *testing.T, checkers string) string {
	t.Helper()
	actx, p := genRunCtx(t)
	act := genAct("30")
	if checkers != "" {
		act.Params["checkers"] = checkers
	}
	if _, err := v3GenRun(context.Background(), actx, act); err != nil {
		t.Fatal(err)
	}
	if err := actx.History.Close(); err != nil {
		t.Fatal(err)
	}
	return p
}

func replayJudged(t *testing.T, src, runnerSrc, python string) (map[string]string, string, error) {
	t.Helper()
	actx, p := genRunCtx(t)
	act := replayAct(src)
	act.Params["runner_src"], act.Params["python"] = runnerSrc, python
	out, err := v3GenReplay(context.Background(), actx, act)
	_ = actx.History.Close()
	return out, p, err
}

// The replay judges its own history with the SAME entry the scenarios'
// judge step runs: judgeArgs is that step's command (m2-faults.yaml's
// exec of history_check.py with --product packs/v3block/product and the
// scenario's one checker list), so the two cannot drift apart.
func TestReplayJudgeIsTheJudgeStepsEntry(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "scenarios", "v3block", "m2-faults.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`env\.CHECK: "\{\{ runner_src \}\}/(checkers/history_check\.py)"[\s\S]*?cmd: python3 -I "\$CHECK" "\$HIST" --json "\$OUT" --product "\{\{ runner_src \}\}/(packs/v3block/product)" --checkers "\{\{ checkers \}\}"`)
	m := re.FindStringSubmatch(string(b))
	if m == nil {
		t.Fatal("m2-faults.yaml's judge step is not the exec of history_check.py this test knows: judgeArgs must follow it")
	}
	got := judgeArgs("R", "H", "O", "C0,C5")
	want := []string{"-I", filepath.Join("R", filepath.FromSlash(m[1])), "H", "--json", "O", "--product", filepath.Join("R", filepath.FromSlash(m[2])), "--checkers", "C0,C5"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("judgeArgs %v, the judge step's %v", got, want)
	}
}

// Red control: a replay whose own history fails a checker answers
// "replay_checks_fail", naming each failing checker and its kind
// ("unknown" until the checkers report one), never "differs" and never a
// match. The real entry, on a fake product's history.
func TestReplayWhoseHistoryFailsACheckerAnswersReplayChecksFail(t *testing.T) {
	py := pythonFor(t)
	f := &fakeProduct{}
	f.install(t)
	src := recordJudged(t, "C0,C1,C5")
	root, _ := filepath.Abs(filepath.Join("..", ".."))
	out, p, err := replayJudged(t, src, root, py)
	if err == nil || out["value"] != "replay_checks_fail" || strings.Contains(err.Error(), "differ") || out["failing"] == "" ||
		!strings.Contains(out["kinds"], ":"+randomrun.CheckKindUnknown) {
		t.Fatalf("%v %v", out, err)
	}
	rec := lastVerb(t, p, "gen_replay_checks").Result
	if got := strings.Join(toStrings(rec["required"]), ","); got != "C0,C1,C5" {
		t.Fatalf("required %s, want the header's C0,C1,C5", got)
	}
}

func toStrings(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

// The checker list is the pack's own: the replay holds none. It passes
// the run header's set to the entry and judges whatever the entry runs: a
// checker the entry adds ("ZZ", failing) is judged when the header names
// no set, and not required when the header's set leaves it out.
func TestReplayRunsThePacksCheckers(t *testing.T) {
	py := pythonFor(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "checkers"), 0o755); err != nil {
		t.Fatal(err)
	}
	entry := `import json, sys
a = sys.argv[1:]
out = a[a.index('--json') + 1]
open(out + '.argv', 'w').write(' '.join(a))
json.dump({'C0': {'verdict': 'PASS', 'findings': []}, 'ZZ': {'verdict': 'FAIL', 'findings': ['seeded']}}, open(out, 'w'))
req = a[a.index('--checkers') + 1].split(',') if '--checkers' in a else ['C0', 'ZZ']
sys.exit(1 if 'ZZ' in req else 0)
`
	if err := os.WriteFile(filepath.Join(root, "checkers", "history_check.py"), []byte(entry), 0o644); err != nil {
		t.Fatal(err)
	}
	f := &fakeProduct{}
	f.install(t)
	// The header names C0: ZZ is run but not required, the replay matches.
	out, p, err := replayJudged(t, recordJudged(t, "C0"), root, py)
	if err != nil || out["value"] != "match" || out["checks"] != "pass" {
		t.Fatalf("header C0: %v %v", out, err)
	}
	argv, _ := os.ReadFile(p + ".judged.jsonl.checks.json.argv")
	if !strings.Contains(string(argv), "--checkers C0") || !strings.Contains(string(argv), filepath.Join("packs", "v3block", "product")) {
		t.Fatalf("the entry was called with %q", argv)
	}
	// The header names no set: every checker the entry runs is required.
	out, _, err = replayJudged(t, recordJudged(t, ""), root, py)
	if err == nil || out["value"] != "replay_checks_fail" || out["failing"] != "ZZ FAIL" {
		t.Fatalf("no header set: %v %v", out, err)
	}
}

// A failing checker blocks match_inop_hit_differs too: that answer, like
// a plain match, needs every required checker PASS on the replay's
// history. (With the kind "unknown" any FAIL blocks; a coverage-only FAIL
// blocks it as well.)
func TestReplayChecksFailBlocksMatchInOpHitDiffers(t *testing.T) {
	_, src, _ := inOpRun(t, "1")
	f := &faultFake{opDelay: time.Millisecond, noHold: true}
	f.install(t)
	run := func(verdict string) (map[string]string, error) {
		actx, _ := genRunCtx(t)
		r := genRunner("v3_gen_replay", "host", &blockModel{})
		r.Judge = func(context.Context, string, string) (map[string]randomrun.CheckerVerdict, error) {
			return map[string]randomrun.CheckerVerdict{"C6": {Verdict: verdict, Findings: []string{"kill_during|v3_op_write|hit not reached"}}}, nil
		}
		defer actx.History.Close()
		return r.Replay(context.Background(), actx, src)
	}
	if out, err := run("PASS"); err != nil || out["value"] != "match_inop_hit_differs" || out["checks"] != "pass" {
		t.Fatalf("checkers pass: %v %v", out, err)
	}
	if out, err := run("FAIL"); err == nil || out["value"] != "replay_checks_fail" || out["failing"] != "C6 FAIL" {
		t.Fatalf("a coverage FAIL: %v %v", out, err)
	}
}

// Red control: a replay that judged nothing never answers a plain match.
// Without runner_src it answers match_unjudged, its checks "not run" with
// the reason, so "match" always means judged.
func TestReplayWithoutRunnerSrcNeverAnswersMatch(t *testing.T) {
	f := &fakeProduct{}
	f.install(t)
	out, _, err := replayInto(t, recordRun(t, "30"))
	if err != nil || out["value"] == "match" || out["value"] != "match_unjudged" || out["checks"] != "not run: no runner_src" {
		t.Fatalf("%v %v", out, err)
	}
}

// Every scenario that can run a replay sets runner_src, so its replays are
// judged: a scenario naming v3_gen_replay must define the variable.
func TestEveryScenarioThatReplaysSetsRunnerSrc(t *testing.T) {
	root := filepath.Join("..", "..", "scenarios")
	replaying := 0
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".yaml") {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if !strings.Contains(string(b), "v3_gen_replay") {
			return nil
		}
		replaying++
		// The value comes from the run's -env (the vars default it to ""; an
		// empty one leaves the replay _unjudged, never a match).
		if !regexp.MustCompile(`(?m)^\s+runner_src:`).Match(b) {
			t.Errorf("%s runs v3_gen_replay but sets no runner_src: its replays would answer _unjudged", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d scenario(s) run v3_gen_replay", replaying)
}
