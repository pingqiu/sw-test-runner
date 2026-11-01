//go:build linux

package seaweed

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	tr "github.com/pingqiu/sw-test-runner"
)

func TestReviewerFreezeResumeAfterLoadEndCannotPass(t *testing.T) {
	f := freezeFixture(t, "positive")
	var c SmokeConfig
	if err := json.Unmarshal([]byte(f.policy.Jobs["read-dc"].Argv[3]), &c); err != nil {
		t.Fatal(err)
	}
	row := windowBody(t, c)
	row["exact"], row["faults"] = 6, []any{}
	times := [][2]float64{{1, 2}, {20, 21}, {40, 41}, {60, 61}, {100, 101}, {119, 120}}
	for i, v := range row["outcomes"].([]any) {
		r := v.(map[string]any)
		r["started"], r["ended"], r["exact"] = times[i][0], times[i][1], true
	}
	after := row["after"].(map[string]any)
	after["read_errors"] = 0
	counters := after["loader"].(map[string]any)
	counters["read_objects"], counters["direct_dc_reads"] = 6, 6
	counters["read_bytes"], counters["direct_dc_bytes"] = 6*c.Length, 6*c.Length
	raw, _ := json.Marshal(row)
	if _, err := CheckReadTaskOutput(captured(raw), c, "load", "dc", ReadWindow{110}); err != nil {
		t.Fatalf("late-resume trace invalid before freeze gate: %v", err)
	}
	runner := f.runtime.Jobs.(*tr.SSHJobs).Runner
	dir := filepath.Dir(runner)
	if err := os.WriteFile(filepath.Join(dir, "load"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	script, err := os.ReadFile(runner)
	if err != nil {
		t.Fatal(err)
	}
	old := "shift=float(open(root+'/stop').read())-30"
	if strings.Count(string(script), old) != 1 {
		t.Fatal("fixture shift changed")
	}
	// Same-clock synthetic 120s trace ends STOP+11s, before the real 20s hold ends.
	if err = os.WriteFile(runner, []byte(strings.Replace(string(script), old, "shift=float(open(root+'/stop').read())-110", 1)), 0700); err != nil {
		t.Fatal(err)
	}
	result, bundle, err := tr.RunApprovedRequest(context.Background(), f.request, f.profile, registerFastRestart, &f.runtime, nil)
	if err != nil {
		t.Fatalf("entry=%v bundle=%s", err, bundle)
	}
	authority, err := os.ReadFile(filepath.Join(bundle, "artifacts", "authority.json"))
	if err != nil {
		t.Fatal(err)
	}
	var a struct{ Pause *tr.ProcessPauseReceipt }
	if err = json.Unmarshal(authority, &a); err != nil || a.Pause == nil {
		t.Fatalf("pause receipt: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "stop"))
	if err != nil {
		t.Fatal(err)
	}
	stop, err := strconv.ParseFloat(string(b), 64)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("status=%s P=%.6f S=%.6f C=%.6f synthetic_load_end=%.6f", result.Status, a.Pause.PreStopMono, a.Pause.StoppedMono, a.Pause.PreContMono, stop+11)
	if a.Pause.PreContMono <= stop+11 {
		t.Fatal("counterexample failed to place CONT after load completion")
	}
	if result.Status == tr.StatusPass {
		t.Error("freeze PASS without any continuing load after CONT/LIVE")
	}
}

func TestFreezeGETAfterPreContButBeforeValidatedLiveRefuses(t *testing.T) {
	f := freezeFixture(t, "positive")
	var c SmokeConfig
	if err := json.Unmarshal([]byte(f.policy.Jobs["read-dc"].Argv[3]), &c); err != nil {
		t.Fatal(err)
	}
	row := windowBody(t, c)
	row["exact"], row["faults"], row["last_exact"] = 6, []any{}, 120.85
	times := [][2]float64{{1, 2}, {20, 21}, {40, 41}, {60, 61}, {100, 101}, {120.75, 120.85}}
	for i, v := range row["outcomes"].([]any) {
		r := v.(map[string]any)
		r["started"], r["ended"], r["exact"] = times[i][0], times[i][1], true
	}
	after := row["after"].(map[string]any)
	after["read_errors"] = 0
	counters := after["loader"].(map[string]any)
	counters["read_objects"], counters["direct_dc_reads"] = 6, 6
	counters["read_bytes"], counters["direct_dc_bytes"] = 6*c.Length, 6*c.Length
	raw, _ := json.Marshal(row)
	if _, err := CheckReadTaskOutput(captured(raw), c, "load", "dc", ReadWindow{100}); err != nil {
		t.Fatalf("pre-LIVE fixture invalid: %v", err)
	}
	runner := f.runtime.Jobs.(*tr.SSHJobs).Runner
	dir := filepath.Dir(runner)
	if err := os.WriteFile(filepath.Join(dir, "load"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	script, err := os.ReadFile(runner)
	if err != nil {
		t.Fatal(err)
	}
	script = []byte(strings.Replace(string(script), "shift=float(open(root+'/stop').read())-30", "shift=float(open(root+'/cont').read())+.5-120.75", 1))
	script = []byte(strings.Replace(string(script), "if op=='pause-resume':stopped=False", "if op=='pause-resume':\n   stopped=False\n   with open(root+'/cont','w') as f:f.write(str(time.monotonic()))\n   time.sleep(2)", 1))
	script = []byte(strings.Replace(string(script), "raw=json.dumps(b).encode()", "b['outcomes'][4]['started']=float(open(root+'/stop').read())+10; b['outcomes'][4]['ended']=float(open(root+'/stop').read())+11; raw=json.dumps(b).encode()", 1))
	if err = os.WriteFile(runner, script, 0700); err != nil {
		t.Fatal(err)
	}
	result, bundle, err := tr.RunApprovedRequest(context.Background(), f.request, f.profile, registerFastRestart, &f.runtime, nil)
	if err != nil {
		t.Fatalf("entry=%v bundle=%s", err, bundle)
	}
	authority, err := os.ReadFile(filepath.Join(bundle, "artifacts", "authority.json"))
	if err != nil {
		t.Fatal(err)
	}
	var a struct{ Pause *tr.ProcessPauseReceipt }
	if err = json.Unmarshal(authority, &a); err != nil || a.Pause == nil {
		t.Fatalf("pause receipt: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "cont"))
	if err != nil {
		t.Fatal(err)
	}
	stop, err := strconv.ParseFloat(string(b), 64)
	if err != nil {
		t.Fatal(err)
	}
	if !(a.Pause.PreContMono < stop+.5 && a.Pause.ResumedMono > stop+.6) {
		t.Fatalf("fixture did not straddle validated LIVE: C=%f LIVE=%f last=%f..%f", a.Pause.PreContMono, a.Pause.ResumedMono, stop+.5, stop+.6)
	}
	if result.Status == tr.StatusPass {
		t.Error("pre-dispatch marker substituted for validated LIVE")
	}
}
