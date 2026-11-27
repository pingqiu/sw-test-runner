package declared

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tr "github.com/pingqiu/sw-test-runner"
)

// ruleNode answers a command by the first rule whose substring it holds (the
// rule's answers in turn, the last repeating); no rule is exit 127. A rule
// with block never answers.
type ruleNode struct {
	mu    sync.Mutex
	rules []rule
	calls []string
}

type rule struct {
	match   string
	answers []answer
	n       int
	block   bool
}

func (r *ruleNode) on(match string, as ...answer) *ruleNode {
	r.rules = append(r.rules, rule{match: match, answers: as})
	return r
}

func (r *ruleNode) stuck(match string) *ruleNode {
	r.rules = append(r.rules, rule{match: match, block: true})
	return r
}

func (r *ruleNode) Run(ctx context.Context, cmd string) (string, string, int, error) {
	r.mu.Lock()
	r.calls = append(r.calls, cmd)
	for i := range r.rules {
		ru := &r.rules[i]
		if strings.Contains(cmd, ru.match) {
			if ru.block {
				r.mu.Unlock()
				<-ctx.Done()
				select {}
			}
			a := ru.answers[min(ru.n, len(ru.answers)-1)]
			ru.n++
			r.mu.Unlock()
			return a.out, a.errOut, a.code, a.err
		}
	}
	r.mu.Unlock()
	return "", "no rule", 127, nil
}

func (r *ruleNode) RunRoot(ctx context.Context, cmd string) (string, string, int, error) {
	return r.Run(ctx, "ROOT "+cmd)
}

func (r *ruleNode) issued(sub string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.calls {
		if strings.Contains(c, sub) {
			n++
		}
	}
	return n
}

const deviceDesc = head + `
 "actions": [
  {"action": "p_write", "verb": "write", "op": "Disk.Write", "kind": "device", "runs_on": "step_node",
   "params": [{"name": "volume", "kind": "subject", "vars": true},
     {"name": "blocks", "kind": "list", "item_pattern": "^[0-9]+$", "aliases": {"all": "volume.all"}},
     {"name": "sync", "kind": "enum", "values": ["true", "false"], "default": "true"}],
   "derived": {"conv": {"kind": "map", "from": "{{sync}}", "map": {"true": "fsync", "false": "none"}, "error": "sync"},
     "sync_arg": {"kind": "map", "from": "{{sync}}", "map": {"true": "", "false": "false"}, "error": "sync"}},
   "runner_deadline_seconds": 30, "each": "blocks",
   "input": {"kind": "repeat", "text": "{{volume.name}}|{{item}}|", "bytes": 64, "path": "/t/{{item}}", "cite": "x.go:9",
     "command": ["gen", "{{input.text}}", "{{input.path}}", {"raw": "&&"}, "sum", {"raw": "<"}, "{{input.path}}"]},
   "on_invoke_set": {"volume.touched": "{{item}}"},
   "record": {"volume": "{{volume.name}}", "args": {"block": {"template": "{{item}}", "type": "int"}, "sha256": "{{input.sha256}}",
     "sync": {"template": "{{derived.sync_arg}}", "type": "bool", "optional": true}}},
   "device": {"resolver": "nvme_namespace", "subsystem": "{{volume.nqn}}", "within_seconds": 0, "cite": "x.go:10"},
   "io": {"command": ["dd", "if={{input.path}}", "of={{device}}", "seek={{item}}", "conv={{derived.conv}}"], "as_root": true,
     "runner_deadline_seconds": 5, "cite": "x.go:11", "failure": "dd exit {{exit}}: {{stderr_first_line}}"}},
  {"action": "p_read", "verb": "read", "op": "Disk.Read", "kind": "device", "runs_on": "step_node",
   "params": [{"name": "volume", "kind": "subject", "vars": true}, {"name": "blocks", "kind": "list", "item_pattern": "^[0-9]+$", "aliases": {"all": "volume.all"}, "non_empty": true}],
   "runner_deadline_seconds": 30, "each": "blocks",
   "record": {"volume": "{{volume.name}}", "args": {"block": {"template": "{{item}}", "type": "int"}}},
   "device": {"resolver": "nvme_namespace", "subsystem": "{{volume.nqn}}", "within_seconds": 0, "cite": "x.go:10"},
   "io": {"command": ["bash", "-c", "rd {{device}} {{item}} | sha256sum"], "as_root": true, "runner_deadline_seconds": 5, "cite": "x.go:12",
     "ok_stdout": "^[0-9a-f]{64}$", "failure": "read exit {{exit}}: {{stderr_first_line}}", "result": {"sha256": "{{io.stdout}}"},
     "result_unanswered": {"device": "{{device}}"}}}
 ]}`

const oneNS = "ctrl nvme3\nns nvme3c3n1\nend\n"

func deviceSetup(t *testing.T, n Runner, sub *subject) (*Executor, *tr.ActionContext, string) {
	t.Helper()
	return deviceSetupDesc(t, deviceDesc, n, sub)
}

func deviceSetupDesc(t *testing.T, desc string, n Runner, sub *subject) (*Executor, *tr.ActionContext, string) {
	t.Helper()
	f, err := Parse([]byte(desc))
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "h.jsonl")
	h, err := tr.OpenHistory(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	x := &Executor{File: f, Lookup: lookup, Poll: 5 * time.Millisecond,
		Node:    func(*tr.ActionContext, string) (Runner, error) { return n, nil },
		Subject: func(_ *tr.ActionContext, name string) (Subject, error) { sub.f["name"] = name; return sub, nil }}
	return x, &tr.ActionContext{History: h, Vars: map[string]string{"bin": "/b"}, Log: func(string, ...interface{}) {}}, p
}

func readRecords(t *testing.T, p string) []tr.HistoryRecord {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var out []tr.HistoryRecord
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if l == "" {
			continue
		}
		var r tr.HistoryRecord
		if err := json.Unmarshal([]byte(l), &r); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// A device write: the input is made and its hash checked before the
// invoke, the device found by its subsystem, one operation per item, the
// subject updated before each invoke.
func TestDeviceWritePerItem(t *testing.T) {
	sha0, sha2 := RepeatSHA("v1|0|", 64), RepeatSHA("v1|2|", 64)
	n := (&ruleNode{}).on("gen 'v1|0|'", answer{out: sha0 + "\n"}).on("gen 'v1|2|'", answer{out: sha2 + "\n"}).
		on("subsysnqn", answer{out: oneNS}).on("dd ", answer{})
	sub := &subject{f: map[string]string{"nqn": "nqn.x:v1"}}
	x, actx, p := deviceSetup(t, &SimHost{Inner: n}, sub)
	out, err := x.Handler("p_write")(context.Background(), actx, tr.Action{Params: map[string]string{"volume": "v1", "blocks": "0 2", "sync": "false"}})
	if err != nil || out["value"] != tr.OutcomeOK {
		t.Fatalf("%v %v", out, err)
	}
	if n.calls[0] != "gen 'v1|0|' /t/0 && sum < /t/0" {
		t.Fatalf("input command %q", n.calls[0])
	}
	if n.issued("ROOT dd if=/t/2 of=/dev/nvme3n1 seek=2 conv=none") != 1 {
		t.Fatalf("calls %q", n.calls)
	}
	rs := readRecords(t, p)
	if len(rs) != 4 || rs[0].Args["sha256"] != sha0 || rs[0].Args["block"] != float64(0) || rs[0].Args["sync"] != false || rs[3].Outcome != tr.OutcomeOK {
		t.Fatalf("records %+v", rs)
	}
	if sub.f["touched"] != "2" || !sub.saved {
		t.Fatalf("subject %+v", sub)
	}
	// The flag's other value records no sync key.
	x, actx, p = deviceSetup(t, &SimHost{Inner: n}, sub)
	if _, err := x.Handler("p_write")(context.Background(), actx, tr.Action{Params: map[string]string{"volume": "v1", "blocks": "0"}}); err != nil {
		t.Fatal(err)
	}
	if rs := readRecords(t, p); len(rs) != 2 {
		t.Fatalf("records %+v", rs)
	} else if _, has := rs[0].Args["sync"]; has {
		t.Fatalf("an optional arg was recorded: %+v", rs[0].Args)
	}
}

// Red controls: an input whose hash is not the runner's records nothing; a
// value outside the enum, an item outside the pattern and an empty literal
// list are runner errors; an unreadable device scan is unknown with no I/O.
func TestDeviceRefusesWhatItCannotEstablish(t *testing.T) {
	n := (&ruleNode{}).on("gen ", answer{out: strings.Repeat("0", 64)}).on("subsysnqn", answer{out: oneNS}).on("dd ", answer{})
	x, actx, p := deviceSetup(t, &SimHost{Inner: n}, &subject{f: map[string]string{"nqn": "nqn.x:v1"}})
	if _, err := x.Handler("p_write")(context.Background(), actx, tr.Action{Params: map[string]string{"volume": "v1", "blocks": "0"}}); err == nil || n.issued("dd ") != 0 {
		t.Fatalf("a mismatched input was written: %v %q", err, n.calls)
	}
	_ = actx.History.Close()
	for _, r := range readRecords(t, p) {
		if r.Kind == tr.HistoryInvoke {
			t.Fatalf("a runner failure recorded %+v", r)
		}
	}
	for name, params := range map[string]map[string]string{
		"enum":         {"volume": "v1", "blocks": "0", "sync": "yes"},
		"item":         {"volume": "v1", "blocks": "0 x"},
		"empty":        {"volume": "v1", "blocks": " "},
		"alias, empty": {"volume": "v1", "blocks": "all"},
	} {
		x, actx, _ := deviceSetup(t, &SimHost{Inner: n}, &subject{f: map[string]string{"nqn": "nqn.x:v1"}})
		act := "p_write"
		if name == "alias, empty" {
			act = "p_read" // non_empty: an empty alias list is an error for a read
		}
		if _, err := x.Handler(act)(context.Background(), actx, tr.Action{Params: params}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// The alias, non-empty: the subject's own list.
	n = (&ruleNode{}).on("subsysnqn", answer{out: oneNS}).on("rd ", answer{out: strings.Repeat("a", 64) + "\n"})
	x, actx, p = deviceSetup(t, &SimHost{Inner: n}, &subject{f: map[string]string{"nqn": "nqn.x:v1", "all": "4 9"}})
	if out, err := x.Handler("p_read")(context.Background(), actx, tr.Action{Params: map[string]string{"volume": "v1", "blocks": "all"}}); err != nil || out["value"] != tr.OutcomeOK {
		t.Fatalf("%v %v", out, err)
	}
	if rs := readRecords(t, p); len(rs) != 4 || rs[2].Args["block"] != float64(9) || rs[3].Result["sha256"] != strings.Repeat("a", 64) {
		t.Fatalf("records %+v", rs)
	}
	// Unreadable scan: unknown, no I/O.
	n = (&ruleNode{}).on("subsysnqn", answer{errOut: "boom", code: 3}).on("rd ", answer{})
	x, actx, p = deviceSetup(t, &SimHost{Inner: n}, &subject{f: map[string]string{"nqn": "nqn.x:v1"}})
	if out, _ := x.Handler("p_read")(context.Background(), actx, tr.Action{Params: map[string]string{"volume": "v1", "blocks": "1"}}); out["value"] != tr.OutcomeUnknown || n.issued("rd ") != 0 {
		t.Fatalf("%v %q", out, n.calls)
	}
	// A read whose answer is not the declared pattern: unknown, never ok.
	n = (&ruleNode{}).on("subsysnqn", answer{out: oneNS}).on("rd ", answer{out: "short\n"})
	x, actx, p = deviceSetup(t, &SimHost{Inner: n}, &subject{f: map[string]string{"nqn": "nqn.x:v1"}})
	if out, _ := x.Handler("p_read")(context.Background(), actx, tr.Action{Params: map[string]string{"volume": "v1", "blocks": "1"}}); out["value"] != tr.OutcomeUnknown {
		t.Fatalf("%v", out)
	}
	if rs := readRecords(t, p); rs[1].Result["detail"] != "read exit 0: " {
		t.Fatalf("detail %+v", rs[1].Result)
	}
}

// An I/O that never answers ends at the I/O's runner deadline: unknown,
// the survivor abandoned and watched, the abandoned result fields added; a
// whole operation that does not answer ends at the action's deadline.
func TestDeviceIODeadline(t *testing.T) {
	n := (&ruleNode{}).on("subsysnqn", answer{out: oneNS}).stuck("rd ")
	x, actx, p := deviceSetup(t, &SimHost{Inner: n, Unkillable: true}, &subject{f: map[string]string{"nqn": "nqn.x:v1"}})
	x.Deadline = func(time.Duration) time.Duration { return 100 * time.Millisecond }
	if out, err := x.Handler("p_read")(context.Background(), actx, tr.Action{Params: map[string]string{"volume": "v1", "blocks": "1"}}); err != nil || out["value"] != tr.OutcomeUnknown {
		t.Fatalf("%v %v", out, err)
	}
	rs := readRecords(t, p)
	if rs[1].Result["detail"] != "timeout after 0s" || rs[1].Result["device"] != "/dev/nvme3n1" || rs[1].Result[AbandonedKey] == nil || Watched(actx.History) != 1 {
		t.Fatalf("result %+v", rs[1].Result)
	}
	// The scan itself hangs: the action's deadline ends it.
	n = (&ruleNode{}).stuck("subsysnqn")
	x, actx, p = deviceSetup(t, &SimHost{Inner: n}, &subject{f: map[string]string{"nqn": "nqn.x:v1"}})
	x.OpDeadline = func(time.Duration) time.Duration { return 200 * time.Millisecond }
	start := time.Now()
	if out, _ := x.Handler("p_read")(context.Background(), actx, tr.Action{Params: map[string]string{"volume": "v1", "blocks": "1"}}); out["value"] != tr.OutcomeUnknown || time.Since(start) > 5*time.Second {
		t.Fatalf("%v after %s", out, time.Since(start))
	}
	if rs := readRecords(t, p); rs[1].Result["detail"] != "timeout after 0s" {
		t.Fatalf("result %+v", rs[1].Result)
	}
}

// Parse refuses a device action that misses a part or names what is not
// defined where it is used.
func TestDeviceParseRefusals(t *testing.T) {
	if _, err := Parse([]byte(deviceDesc)); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string][2]string{
		"io deadline outside the action's": {`"runner_deadline_seconds": 5, "cite": "x.go:11"`, `"runner_deadline_seconds": 30, "cite": "x.go:11"`},
		"device named before resolved":     {`"path": "/t/{{item}}"`, `"path": "/t/{{device}}"`},
		"stdout named in a failure":        {`"failure": "read exit {{exit}}: {{stderr_first_line}}"`, `"failure": "read {{io.stdout}}"`},
		"raw with a template":              {`{"raw": "&&"}`, `{"raw": "&& {{item}}"}`},
		"each is not a list": {`"each": "blocks",
   "input"`, `"each": "sync",
   "input"`},
		"unknown resolver": {`"resolver": "nvme_namespace", "subsystem": "{{volume.nqn}}", "within_seconds": 0, "cite": "x.go:10"},
   "io": {"command": ["dd"`, `"resolver": "by_name", "subsystem": "{{volume.nqn}}", "within_seconds": 0, "cite": "x.go:10"},
   "io": {"command": ["dd"`},
	} {
		bad := strings.Replace(deviceDesc, c[0], c[1], 1)
		if bad == deviceDesc {
			t.Fatalf("%s: the seed did not apply", name)
		}
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
