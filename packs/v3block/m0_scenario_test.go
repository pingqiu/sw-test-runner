package v3block

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tr "github.com/pingqiu/sw-test-runner"
)

// The committed M0 scenario through the real engine with recording handlers:
// every typed operation's parameters and shared configuration resolve, the
// sequence is the plan's (restore before the target's first read, reads of
// every written block before the deletes), and teardown runs when a step
// fails. It proves the wiring, not the product.
func TestM0ScenarioWiring(t *testing.T) {
	for _, fail := range []string{"", "v3_op_move", "master-spawn"} {
		t.Run("fail="+fail, func(t *testing.T) {
			s, err := tr.ParseFile("../../scenarios/v3block/m0-sequence.yaml")
			if err != nil {
				t.Fatal(err)
			}
			s.Env["product_src"], s.Env["runner_src"], s.Env["weed"] = "/src", "/runner", "/bin/weed"
			// set by the CLI in a bundled run
			s.Env["bundle_dir"], s.Env["artifacts_dir"], s.Env["run_id"] = "/bundle", "/bundle/artifacts", "run-1"
			var mu sync.Mutex
			var steps []string
			var bad []string
			handler := func(_ context.Context, actx *tr.ActionContext, a tr.Action) (map[string]string, error) {
				mu.Lock()
				defer mu.Unlock()
				steps = append(steps, a.Action+" "+a.Params["volume"]+" "+a.Params["lbas"])
				for k, v := range a.Params {
					if strings.Contains(v, "{{") {
						bad = append(bad, fmt.Sprintf("%s %s=%q", a.Action, k, v))
					}
				}
				if strings.HasPrefix(a.Action, "v3_op_") && a.Action != "v3_op_spec" {
					want := map[string]string{"bin_dir": "/root/m0-run/bin", "master": "127.0.0.1:19600", "filer": "127.0.0.1:29588", "root": "/root/m0-run/vol", "client": "c1",
						"own_script": "/runner/scenarios/v3block/m0-own.sh", "work": "/root/m0-run", "owner": "ok"}
					for k, w := range want {
						if got := opParam(a, actx, k); got != w {
							bad = append(bad, fmt.Sprintf("%s %s resolved to %q, want %q", a.Action, k, got, w))
						}
					}
				}
				if a.Action == "v3_op_spec" && a.Params["nqn_prefix"] != "nqn.2026-11.io.seaweedfs:op-run-1-" {
					bad = append(bad, "the NQN prefix does not carry the run id: "+a.Params["nqn_prefix"])
				}
				if a.Action == "exec" && strings.Contains(a.Params["cmd"], " teardown ") && a.Params["env.NQN"] != "nqn.2026-11.io.seaweedfs:op-run-1-" {
					bad = append(bad, "teardown's NQN prefix is not this run's: "+a.Params["env.NQN"])
				}
				if a.Action == "v3_start_blockmaster" {
					actx.Vars["bm_pid"] = "4242"
				}
				if fail == "master-spawn" && a.Action == "exec" && strings.Contains(a.Params["cmd"], " spawn ") && strings.Contains(a.Params["cmd"], "blockmaster") {
					return nil, fmt.Errorf("injected: port already listens")
				}
				if a.Action == "v3_start_blockmaster" {
					bad = append(bad, "the master starts outside the owned spawn")
				}
				if a.Action == fail {
					return nil, fmt.Errorf("injected")
				}
				return map[string]string{"value": "ok"}, nil
			}
			r := tr.NewRegistry()
			for _, name := range []string{"exec", "collect_path", "v3_history_end", "v3_start_blockmaster", "v3_op_spec", "v3_op_create", "v3_op_attach", "v3_op_write", "v3_op_read",
				"v3_op_snapshot", "v3_op_snapshot_delete", "v3_op_restore", "v3_op_release", "v3_op_stop", "v3_op_move", "v3_op_delete", "v3_op_observe"} {
				r.RegisterFunc(name, tr.TierCore, handler)
			}
			actx := &tr.ActionContext{Scenario: s, Vars: map[string]string{}, Log: func(string, ...interface{}) {}}
			res := tr.NewEngine(r, nil).Run(context.Background(), s, actx)
			if (res.Status == tr.StatusPass) != (fail == "") {
				t.Fatalf("status %s error %q", res.Status, res.Error)
			}
			for _, b := range bad {
				t.Error(b)
			}
			// Teardown, then the judge (logs collected, the checkers run), even
			// when the sequence failed.
			n := len(steps)
			if n < 5 || !strings.HasPrefix(steps[n-5], "exec ") || !strings.HasPrefix(steps[n-4], "v3_history_end ") || !strings.HasPrefix(steps[n-3], "collect_path ") || !strings.HasPrefix(steps[n-1], "exec ") {
				t.Errorf("the run does not end with teardown, log collection and the checkers: %q", steps[max(0, n-5):])
			}
			if first := steps[0]; !strings.HasPrefix(first, "exec ") {
				t.Errorf("the run does not start by claiming its work directory: %q", first)
			}
			order := strings.Join(steps, "\n")
			if fail == "master-spawn" {
				if strings.Contains(order, "v3_op_create") {
					t.Error("a typed create ran after the master's owned start failed")
				}
				return
			}
			if fail != "" {
				return
			}
			for _, before := range [][2]string{
				{"v3_op_restore v1 ", "v3_op_read v2 0 2048 4095"},
				{"v3_op_read v1 written", "v3_op_delete v2 "},
				{"v3_op_read v2 written", "v3_op_delete v2 "},
				{"v3_op_release v1 ", "v3_op_snapshot_delete v1 "},
				{"v3_op_stop v1 ", "v3_op_delete v1 "},
			} {
				if a, b := strings.Index(order, before[0]), strings.Index(order, before[1]); a < 0 || b < 0 || a > b {
					t.Errorf("%q must come before %q", before[0], before[1])
				}
			}
		})
	}
}

// record writes one invoke and one complete with the op id of the verb.
func TestRecordWritesInvokeAndComplete(t *testing.T) {
	h, err := tr.OpenHistory(filepath.Join(t.TempDir(), "history.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	actx := &tr.ActionContext{History: h, Log: func(string, ...interface{}) {}}
	out, err := record(actx, opEnv{client: "c1"}, "write", "v1", map[string]any{"lba": 0}, func() (string, map[string]any) {
		return tr.OutcomeUnknown, map[string]any{"detail": "x"}
	})
	if err != nil || out["value"] != tr.OutcomeUnknown {
		t.Fatalf("%v %v", out, err)
	}
	if h.Open() != 0 {
		t.Fatal("the invocation stayed open")
	}
	for verb, op := range opIDs {
		if !strings.Contains(op, ".") {
			t.Errorf("verb %s has op %q, not Module.Action", verb, op)
		}
	}
}

// The callers' rule: the invocation is on disk before the command runs, and
// the completion is stamped after its answer. A handler that sleeps between
// the two shows both.
func TestRecordInvokesBeforeAndCompletesAfter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	h, err := tr.OpenHistory(path)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	actx := &tr.ActionContext{History: h, Log: func(string, ...interface{}) {}}
	const nap = 60 * time.Millisecond
	var seenDuring string
	if _, err := record(actx, opEnv{client: "c1"}, "write", "v1", map[string]any{"lba": 0}, func() (string, map[string]any) {
		b, _ := os.ReadFile(path)
		seenDuring = string(b)
		time.Sleep(nap)
		return tr.OutcomeOK, nil
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(seenDuring, `"kind":"invoke"`) || strings.Contains(seenDuring, `"kind":"complete"`) {
		t.Fatalf("while the command ran the file held %q; want the invocation only", seenDuring)
	}
	b, _ := os.ReadFile(path)
	var rs []tr.HistoryRecord
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var r tr.HistoryRecord
		if err := json.Unmarshal([]byte(l), &r); err != nil {
			t.Fatal(err)
		}
		rs = append(rs, r)
	}
	if len(rs) != 2 || rs[1].MonoNs-rs[0].MonoNs < nap.Nanoseconds() {
		t.Fatalf("records %+v; want the completion stamped at least %s after the invocation", rs, nap)
	}
}

// "written" is every lba a write was attempted on, in numeric order (the
// volume subject's field the declared write and read name as an alias); a
// list is checked by the declared list param (declared/device_test.go).
func TestWrittenLBAs(t *testing.T) {
	v := &opVolume{Written: map[string]bool{"4095": true, "0": true, "2048": true, "300": true}}
	s := &volumeSubject{v: v}
	if got, err := s.Field(context.Background(), nil, "written"); err != nil || got != "0 300 2048 4095" {
		t.Errorf("written: %q %v", got, err)
	}
	if err := s.Set("written", "7"); err != nil || !v.Written["7"] {
		t.Errorf("set: %v %v", v.Written, err)
	}
	if got, _ := (&volumeSubject{v: &opVolume{}}).Field(context.Background(), nil, "written"); got != "" {
		t.Errorf("none written: %q", got)
	}
}
