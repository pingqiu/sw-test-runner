package declared

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/randomrun"
)

// driven is action name as the driver runs it: re-issued by
// randomrun.Attempts under the executor's declared rerun.
func driven(x *Executor, name string) tr.ActionHandlerFunc {
	return func(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
		act.Action = name
		out, _, err := randomrun.Attempts(ctx, actx, x.Handler(name), act, x, act.Params["volume"], nil)
		return out, err
	}
}

// node answers every command with the next scripted answer (the last
// repeats) and records the commands.
type node struct {
	answers []answer
	calls   []string
}

type answer struct {
	out, errOut string
	code        int
	err         error
}

func (n *node) Run(_ context.Context, cmd string) (string, string, int, error) {
	n.calls = append(n.calls, cmd)
	a := n.answers[min(len(n.calls)-1, len(n.answers)-1)]
	return a.out, a.errOut, a.code, a.err
}

func (n *node) RunRoot(ctx context.Context, cmd string) (string, string, int, error) {
	return n.Run(ctx, "ROOT "+cmd)
}

type subject struct {
	f     map[string]string
	saved bool
}

func (s *subject) Field(_ context.Context, _ Runner, f string) (string, error) { return s.f[f], nil }
func (s *subject) Set(f, v string) error                                       { s.f[f] = v; return nil }
func (s *subject) Save()                                                       { s.saved = true }

const head = `{"product": "p", "read_at": "p 1", "config": {"keys": [{"name": "bin", "required": true}, {"name": "client", "default": "c1"}], "client": "client"}, "runner_io_dir": "/io",
 "device_resolvers": {"nvme_namespace": {"cite": "core/csi/linux_util.go:318 (the product's connect: nvme connect -t -a -s -n), :554 (a subsystem's NQN in sysfs); run by declared/resolver.go", "scan": {"command": [{"raw": "for d in /sys/class/nvme/nvme*; do [ -e \"$d\" ] || continue; q=$(cat \"$d/subsysnqn\") || exit 3; [ \"$q\" ="}, "{{subsystem}}", {"raw": "] || continue; echo \"ctrl $(basename \"$d\")\"; for n in \"$d\"/nvme*n*; do [ -e \"$n\" ] && echo \"ns $(basename \"$n\")\"; done; done; echo end"}], "end": "end", "lines": {"ctrl": "^ctrl (.*)$", "ns": "^ns (\\S+?)(?:c[0-9]+)?(n[0-9]+)$"}, "absent": {"ctrl": 0}, "one": {"ctrl": 1, "ns": 1}, "device_line": "ns", "device": "/dev/{{ns.1}}{{ns.2}}", "unreadable": "sysfs scan exit {{exit}}: {{stderr_first_line}}", "bad_line": "sysfs scan printed {{line}}", "incomplete": "subsystem {{subsystem}}: {{count.ctrl}} controllers, {{count.ns}} namespaces", "no_device": "no controller of {{subsystem}}", "no_device_by_deadline": "no single device of {{subsystem}} by the runner deadline", "every_ms": 250}, "connect": {"command": ["printf", "%s", "transport=tcp,traddr={{host}},trsvcid={{port}},nqn={{subsystem}}{{options}}", {"raw": ">"}, "/dev/nvme-fabrics"], "as_root": true, "require": [{"value": "{{host}}", "pattern": "^[0-9.]+$", "failure": "nvme connect: malformed address or nqn"}, {"value": "{{port}}", "pattern": "^[0-9]+$", "failure": "nvme connect: malformed address or nqn"}, {"value": "{{subsystem}}", "pattern": "^[A-Za-z0-9.:_-]+$", "failure": "nvme connect: malformed address or nqn"}, {"value": "{{options}}", "pattern": "^(,[a-z_]+=-?[0-9]+)*$", "failure": "nvme connect: malformed options {{options}}"}], "failure": "nvme connect: {{stderr_first_line}}"}, "disconnect": {"command": [{"raw": "n=0; for d in /sys/class/nvme/nvme*; do [ -e \"$d\" ] || continue; q=$(cat \"$d/subsysnqn\") || exit 3; [ \"$q\" ="}, "{{subsystem}}", {"raw": "] || continue; echo 1 > \"$d/delete_controller\" || exit 4; n=$((n+1)); done; echo \"deleted $n\""}], "as_root": true, "count_pattern": "^deleted ([0-9]+)$", "failure": "nvme disconnect: exit {{exit}}: {{stderr_first_line}}", "confirm_within_seconds": 5, "confirm_every_ms": 250, "not_confirmed": "nvme disconnect: cannot confirm: {{why}}", "remains": "nvme disconnect: a controller of {{subsystem}} remains", "remains_by_deadline": "nvme disconnect: a controller of {{subsystem}} remains at the runner deadline"}}},`

const desc = head + `
 "actions": [
  {"action": "p_put", "verb": "put", "op": "Store.Put", "runs_on": "step_node",
   "params": [{"name": "volume", "kind": "subject", "vars": true}, {"name": "key", "required": true}, {"name": "n", "kind": "int", "default": "7"}],
   "record": {"volume": "{{volume.name}}", "args": {"key": "{{key}}", "n": {"template": "{{n}}", "type": "int"}}},
   "variants": [
     {"name": "fast", "when": {"field": "volume.fast", "equals": "yes"}, "command": ["{{config.bin}}/put", "--fast", {"json": {"k": "{{key}}"}}], "runner_deadline_seconds": 5, "cite": "x.go:1",
      "decode": {"kind": "error_body", "path": "put fast", "answer": "http_status_last_line", "transport_failure": "no answer (exit {{exit}})"}},
     {"name": "slow", "command": ["{{config.bin}}/put", "--key", "{{key}}", "--n", "{{n}}"], "runner_deadline_seconds": 5, "cite": "x.go:2",
      "decode": {"kind": "error_body", "path": "put"}}
   ],
   "retry": {"while_code": "busy", "while_message_contains": "later", "max_attempts": 5, "every_seconds": 1, "within_param": "n", "request_arg": "key", "page_cite": "docs/x.md:1"}},
  {"action": "p_make", "verb": "make", "op": "Store.Make", "runs_on": "step_node", "as_root": true,
   "params": [{"name": "volume", "kind": "subject", "vars": true}],
   "record": {"volume": "{{volume.name}}", "args": {}},
   "variants": [{"command": ["mk", "{{volume.name}}"], "runner_deadline_seconds": 5, "cite": "x.go:3",
     "decode": {"kind": "self_reported", "fields": {"outcome": "string", "volume": "string", "id": "string", "ns": "int", "error": "string"},
       "outcome_field": "outcome", "exits": {"ok": 0, "refused": 1, "unknown": 3}, "match": {"volume": "{{volume.name}}"},
       "ok_length": {"id": 4}, "result": {"ns": "ns"}, "result_ok": {"id": "id"}, "detail": "{{answer.error}} {{stderr}}"}}],
   "on_ok_set": {"volume.id": "{{answer.id}}"}}
 ],
 "exceptions": [{"action": "p_attach", "why": "a process start", "declarable_with": "a process step", "runner_deadline_seconds": 30}]}`

func lookup(action, path, code, message string) (string, map[string]any) {
	if code == "nope" || (code == "gone" && strings.Contains(message, "before")) || (code == "busy" && message == "later") {
		return tr.OutcomeRefused, nil
	}
	if code == "reserved" {
		return tr.OutcomeRefusedEffect, map[string]any{"part": "workspace", "to": map[string]any{"exit_nonzero": true}, "cite": "docs/x.md:1"}
	}
	return tr.OutcomeUnknown, nil
}

func setup(t *testing.T, n *node, sub *subject) (*Executor, *tr.ActionContext) {
	t.Helper()
	f, err := Parse([]byte(desc))
	if err != nil {
		t.Fatal(err)
	}
	h, err := tr.OpenHistory(filepath.Join(t.TempDir(), "h.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	x := &Executor{File: f, Lookup: lookup, Pause: func(time.Duration) time.Duration { return time.Millisecond },
		Node:    func(*tr.ActionContext, string) (Runner, error) { return &SimHost{Inner: n}, nil },
		Subject: func(_ *tr.ActionContext, name string) (Subject, error) { sub.f["name"] = name; return sub, nil }}
	return x, &tr.ActionContext{History: h, Vars: map[string]string{"bin": "{{work}}/b in", "work": "/w"}, Log: func(string, ...interface{}) {}}
}

func TestCommandIsTemplatedAndEveryWordQuoted(t *testing.T) {
	n := &node{answers: []answer{{code: 0}}}
	x, actx := setup(t, n, &subject{f: map[string]string{}})
	out, err := x.Handler("p_put")(context.Background(), actx, tr.Action{Params: map[string]string{"volume": "v1", "key": "a'b c"}})
	if err != nil || out["value"] != tr.OutcomeOK {
		t.Fatalf("%v %v", out, err)
	}
	if want := `'/w/b in/put' --key 'a'\''b c' --n 7`; n.calls[0] != want {
		t.Fatalf("command %q, want %q", n.calls[0], want)
	}
}

func TestVariantAndHTTPAnswer(t *testing.T) {
	n := &node{answers: []answer{{out: `{"code":"nope","message":"m"}` + "\n409"}}}
	x, actx := setup(t, n, &subject{f: map[string]string{"fast": "yes"}})
	out, _ := x.Handler("p_put")(context.Background(), actx, tr.Action{Params: map[string]string{"volume": "v1", "key": "k"}})
	if out["value"] != tr.OutcomeRefused || n.calls[0] != `'/w/b in/put' --fast '{"k":"k"}'` {
		t.Fatalf("%v %q", out, n.calls)
	}
	n = &node{answers: []answer{{code: 7}}}
	x, actx = setup(t, n, &subject{f: map[string]string{"fast": "yes"}})
	if out, _ := x.Handler("p_put")(context.Background(), actx, tr.Action{Params: map[string]string{"volume": "v1", "key": "k"}}); out["value"] != tr.OutcomeUnknown {
		t.Fatalf("transport failure: %v", out)
	}
}

func TestRetryWhileCode(t *testing.T) {
	busy := answer{out: `{"code":"busy","message":"later"}`, code: 1}
	n := &node{answers: []answer{busy, busy, {code: 0}}}
	x, actx := setup(t, n, &subject{f: map[string]string{}})
	out, _ := driven(x, "p_put")(context.Background(), actx, tr.Action{Params: map[string]string{"volume": "v1", "key": "k", "n": "60"}})
	if out["value"] != tr.OutcomeOK || len(n.calls) != 3 {
		t.Fatalf("%v after %d calls", out, len(n.calls))
	}
	// Red control: an expected refusal is one call.
	n = &node{answers: []answer{busy, {code: 0}}}
	x, actx = setup(t, n, &subject{f: map[string]string{}})
	out, _ = driven(x, "p_put")(context.Background(), actx, tr.Action{Params: map[string]string{"volume": "v1", "key": "k", "n": "60", "expect": "refused"}})
	if out["value"] != tr.OutcomeRefused || len(n.calls) != 1 {
		t.Fatalf("expected refusal: %v after %d calls", out, len(n.calls))
	}
}

// Red control: an UNKNOWN is never re-issued, even when its detail is the
// page's answer (an unknown may have changed the subject, and the next
// attempt would have no "before"): exactly one call, and the request ends
// unknown.
func TestAnUnknownIsNeverReissued(t *testing.T) {
	n := &node{answers: []answer{{out: `{"code":"busy","message":"later"}`, code: 1}, {code: 0}}}
	x, actx := setup(t, n, &subject{f: map[string]string{}})
	x.Lookup = func(string, string, string, string) (string, map[string]any) { return tr.OutcomeUnknown, nil }
	out, err := driven(x, "p_put")(context.Background(), actx, tr.Action{Params: map[string]string{"volume": "v1", "key": "k", "n": "60"}})
	if err != nil || out["value"] != tr.OutcomeUnknown || out["detail"] != "busy: later" || len(n.calls) != 1 {
		t.Fatalf("%v %v after %d calls", out, err, len(n.calls))
	}
}

func TestSelfReported(t *testing.T) {
	for _, c := range []struct {
		name, out string
		code      int
		want      string
	}{
		{"ok", `{"outcome":"ok","volume":"v1","id":"abcd","ns":5}`, 0, tr.OutcomeOK},
		{"refused", `{"outcome":"refused","volume":"v1","error":"no"}`, 1, tr.OutcomeRefused},
		{"exit disagrees", `{"outcome":"refused","volume":"v1"}`, 3, tr.OutcomeUnknown},
		{"other volume", `{"outcome":"ok","volume":"v2","id":"abcd"}`, 0, tr.OutcomeUnknown},
		{"short id", `{"outcome":"ok","volume":"v1","id":"abc"}`, 0, tr.OutcomeUnknown},
		{"wrong type", `{"outcome":"ok","volume":"v1","id":"abcd","ns":"5"}`, 0, tr.OutcomeUnknown},
		{"case-insensitive key", `{"Outcome":"ok","VOLUME":"v1","id":"abcd"}`, 0, tr.OutcomeOK},
		{"garbage", `nope`, 0, tr.OutcomeUnknown},
	} {
		sub := &subject{f: map[string]string{}}
		n := &node{answers: []answer{{out: c.out, code: c.code}}}
		x, actx := setup(t, n, sub)
		out, err := x.Handler("p_make")(context.Background(), actx, tr.Action{Params: map[string]string{"volume": "v1"}})
		if err != nil || out["value"] != c.want {
			t.Errorf("%s: %v %v, want %s", c.name, out, err, c.want)
		}
		if (c.want == tr.OutcomeOK) != (sub.f["id"] == "abcd" && sub.saved) {
			t.Errorf("%s: subject %v saved %v", c.name, sub.f, sub.saved)
		}
		if !strings.HasPrefix(n.calls[0], "ROOT mk v1") {
			t.Errorf("%s: as_root not honoured: %q", c.name, n.calls[0])
		}
	}
}

func TestRunnerErrorsRecordNothing(t *testing.T) {
	for name, act := range map[string]tr.Action{
		"no volume":  {Params: map[string]string{"key": "k"}},
		"no key":     {Params: map[string]string{"volume": "v1"}},
		"bad expect": {Params: map[string]string{"volume": "v1", "key": "k", "expect": "ok"}},
		"unresolved": {Params: map[string]string{"volume": "v1", "key": "k", "bin": "{{nowhere}}"}},
	} {
		n := &node{answers: []answer{{}}}
		x, actx := setup(t, n, &subject{f: map[string]string{}})
		if _, err := x.Handler("p_put")(context.Background(), actx, act); err == nil || len(n.calls) != 0 || actx.History.Open() != 0 {
			t.Errorf("%s: err %v, %d calls", name, err, len(n.calls))
		}
	}
}

// Red controls of the description's checks: each seeded defect is refused.
func TestParseRedControls(t *testing.T) {
	good := `"actions": [{"action": "a", "verb": "v", "op": "M.A", "runs_on": "step_node", "params": [{"name": "p"}], "record": {"volume": "{{p}}", "args": {}},
	  "variants": [{"command": ["x", "{{p}}"], "runner_deadline_seconds": 1, "cite": "x.go:1", "decode": {"kind": "error_body", "path": "x"}}]}], "exceptions": []}`
	if _, err := Parse([]byte(head + good)); err != nil {
		t.Fatalf("green twin: %v", err)
	}
	for name, bad := range map[string][2]string{
		"no cite":               {`"cite": "x.go:1"`, `"cite": " "`},
		"no timeout":            {`"runner_deadline_seconds": 1`, `"runner_deadline_seconds": 0`},
		"undefined name":        {`"{{p}}"]`, `"{{q}}"]`},
		"unknown decode":        {`"kind": "error_body"`, `"kind": "grep"`},
		"no path":               {`"path": "x"`, `"path": ""`},
		"unknown field":         {`"op": "M.A"`, `"op": "M.A", "shell": "x"`},
		"declared and excepted": {`"exceptions": []`, `"exceptions": [{"action": "a", "why": "w", "declarable_with": "f", "runner_deadline_seconds": 5}]`},
		"exception without why": {`"exceptions": []`, `"exceptions": [{"action": "b", "why": "", "declarable_with": "f", "runner_deadline_seconds": 5}]`},
		"exception without the feature that would declare it": {`"exceptions": []`, `"exceptions": [{"action": "b", "why": "w", "runner_deadline_seconds": 5}]`},
		"runner setup without why":                            {`"exceptions": []`, `"exceptions": [], "runner_setup": [{"action": "s", "why": ""}]`},
		"a rerun without a page line":                         {`"cite": "x.go:1", "decode": {"kind": "error_body", "path": "x"}}]}`, `"cite": "x.go:1", "decode": {"kind": "error_body", "path": "x"}}], "retry": {"while_code": "c", "while_message_contains": "m", "max_attempts": 3, "every_seconds": 1, "request_arg": "r", "page_cite": "x.go:1"}}`},
		"exception without a runner deadline":                 {`"exceptions": []`, `"exceptions": [{"action": "b", "why": "w", "declarable_with": "f"}]`},
		"other node":                                          {`"runs_on": "step_node"`, `"runs_on": "anywhere"`},
	} {
		if !strings.Contains(good, bad[0]) {
			t.Fatalf("%s: seed %q not in the green twin", name, bad[0])
		}
		if _, err := Parse([]byte(head + strings.Replace(good, bad[0], bad[1], 1))); err == nil {
			t.Errorf("%s: not refused", name)
		}
	}
}

// hung never answers a command.
type hung struct{ calls int }

func (h *hung) Run(ctx context.Context, cmd string) (string, string, int, error) {
	h.calls++
	select {}
}

func (h *hung) RunRoot(ctx context.Context, cmd string) (string, string, int, error) {
	return h.Run(ctx, cmd)
}

// A command that never answers ends at its runner deadline as unknown, the
// operation is completed in the history, and the next operation runs.
func TestRunnerDeadlineEndsAHungCommand(t *testing.T) {
	for _, unkillable := range []bool{false, true} {
		f, err := Parse([]byte(desc))
		if err != nil {
			t.Fatal(err)
		}
		h, err := tr.OpenHistory(filepath.Join(t.TempDir(), "h.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		host := &SimHost{Inner: &hung{}, Unkillable: unkillable}
		x := &Executor{File: f, Lookup: lookup, Poll: 5 * time.Millisecond,
			Deadline: func(time.Duration) time.Duration { return 100 * time.Millisecond },
			Node:     func(*tr.ActionContext, string) (Runner, error) { return host, nil },
			Subject: func(*tr.ActionContext, string) (Subject, error) {
				return &subject{f: map[string]string{"name": "v1"}}, nil
			}}
		actx := &tr.ActionContext{History: h, Vars: map[string]string{"bin": "/b"}, Log: func(string, ...interface{}) {}}
		start := time.Now()
		out, err := x.Handler("p_put")(context.Background(), actx, tr.Action{Params: map[string]string{"volume": "v1", "key": "k"}})
		if err != nil || out["value"] != tr.OutcomeUnknown || time.Since(start) > 5*time.Second {
			t.Fatalf("unkillable=%v: %v %v after %s", unkillable, out, err, time.Since(start))
		}
		if h.Open() != 0 {
			t.Fatalf("the hung operation was not completed")
		}
		h.Close()
		b, _ := os.ReadFile(h.Path())
		if !strings.Contains(string(b), `"detail":"timeout after 0s"`) || strings.Contains(string(b), `"abandoned"`) != unkillable {
			t.Fatalf("unkillable=%v: history %s", unkillable, b)
		}
		if unkillable && !strings.Contains(string(b), `"abandoned":[{"pid":"40002","start":"777"}]`) {
			t.Fatalf("the survivor is not named: %s", b)
		}
	}
}

// A node that answers nothing at all, not even the protocol: each call is
// bounded, so the operation still ends, unknown.
func TestRunnerDeadlineEndsAHungNode(t *testing.T) {
	defer func(d time.Duration) { ProtocolCallBound = d }(ProtocolCallBound)
	ProtocolCallBound = 50 * time.Millisecond
	f, _ := Parse([]byte(desc))
	h, _ := tr.OpenHistory(filepath.Join(t.TempDir(), "h.jsonl"))
	defer h.Close()
	x := &Executor{File: f, Lookup: lookup, Deadline: func(time.Duration) time.Duration { return 100 * time.Millisecond },
		Node: func(*tr.ActionContext, string) (Runner, error) { return &hung{}, nil },
		Subject: func(*tr.ActionContext, string) (Subject, error) {
			return &subject{f: map[string]string{"name": "v1"}}, nil
		}}
	actx := &tr.ActionContext{History: h, Vars: map[string]string{"bin": "/b"}, Log: func(string, ...interface{}) {}}
	start := time.Now()
	out, err := x.Handler("p_put")(context.Background(), actx, tr.Action{Params: map[string]string{"volume": "v1", "key": "k"}})
	if err != nil || out["value"] != tr.OutcomeUnknown || time.Since(start) > 2*time.Second || h.Open() != 0 {
		t.Fatalf("%v %v after %s", out, err, time.Since(start))
	}
}

// Red controls of an observation's checks.
func TestParseObserveRedControls(t *testing.T) {
	good := `"actions": [{"action": "o", "verb": "observe", "kind": "observe", "runs_on": "step_node", "params": [{"name": "volume", "kind": "subject"}],
	  "record": {"volume": "{{volume.name}}"}, "derived": {"p": {"kind": "map", "from": "{{volume.id}}", "map": {"a": "b"}, "error": "no row"}},
	  "steps": [{"name": "s", "command": ["show", "{{derived.p}}"], "runner_deadline_seconds": 1, "cite": "x.go:1",
	    "read": {"kind": "json_strings", "into": "l", "error_into": "e", "error": "listing: {{stderr_first_line}}"}}]}], "exceptions": []}`
	if _, err := Parse([]byte(head + good)); err != nil {
		t.Fatalf("green twin: %v", err)
	}
	for name, bad := range map[string][2]string{
		"step without a cite":      {`"cite": "x.go:1"`, `"cite": ""`},
		"step without a deadline":  {`"runner_deadline_seconds": 1`, `"runner_deadline_seconds": 0`},
		"an op on an observation":  {`"kind": "observe",`, `"kind": "observe", "op": "M.A",`},
		"unknown read":             {`"kind": "json_strings"`, `"kind": "grep"`},
		"undefined derived":        {`{{derived.p}}`, `{{derived.q}}`},
		"unknown derived kind":     {`"kind": "map", "from"`, `"kind": "eval", "from"`},
		"a function derived kind":  {`"kind": "map", "from"`, `"kind": "prefix", "from"`},
		"a map without its rows":   {`"map": {"a": "b"}, `, ``},
		"undefined name in a read": {`{{stderr_first_line}}`, `{{stdout_all}}`},
	} {
		if !strings.Contains(good, bad[0]) {
			t.Fatalf("%s: seed not in the green twin", name)
		}
		if _, err := Parse([]byte(head + strings.Replace(good, bad[0], bad[1], 1))); err == nil {
			t.Errorf("%s: not refused", name)
		}
	}
}

// A refused_effect answer completes refused_effect with the table's effect
// copied into the result; a refused one carries no effect.
func TestDecoderCopiesTheEffect(t *testing.T) {
	for code, want := range map[string]string{"reserved": tr.OutcomeRefusedEffect, "nope": tr.OutcomeRefused} {
		n := &node{answers: []answer{{out: `{"code":"` + code + `","message":"m"}`, code: 1}}}
		x, actx := setup(t, n, &subject{f: map[string]string{}})
		out, err := x.Handler("p_put")(context.Background(), actx, tr.Action{Params: map[string]string{"volume": "v1", "key": "k"}})
		if err != nil || out["value"] != want {
			t.Fatalf("%s: %v %v", code, out, err)
		}
		actx.History.Close()
		b, _ := os.ReadFile(actx.History.Path())
		hasEffect := strings.Contains(string(b), `"effect":{"cite":"docs/x.md:1","part":"workspace","to":{"exit_nonzero":true}}`)
		if hasEffect != (want == tr.OutcomeRefusedEffect) {
			t.Fatalf("%s: history %s", code, b)
		}
	}
}

// Each attempt of a rerun is its own invoke and complete, with the request
// and the attempt number; the bound ends it; the last attempt's outcome
// stands; a rerun answer the page does not name is not rerun.
func TestRerunAttemptsAreOperations(t *testing.T) {
	busy := answer{out: `{"code":"busy","message":"later"}`, code: 1}
	other := answer{out: `{"code":"busy","message":"never"}`, code: 1}
	for name, c := range map[string]struct {
		answers []answer
		calls   int
		last    string
	}{
		"twice busy then ok":     {[]answer{busy, busy, {code: 0}}, 3, tr.OutcomeOK},
		"busy to the bound":      {[]answer{busy}, 5, tr.OutcomeRefused},
		"the page's answer only": {[]answer{other, {code: 0}}, 1, tr.OutcomeUnknown},
	} {
		n := &node{answers: c.answers}
		x, actx := setup(t, n, &subject{f: map[string]string{}})
		out, err := driven(x, "p_put")(context.Background(), actx, tr.Action{Params: map[string]string{"volume": "v1", "key": "k", "n": "60"}})
		if err != nil || out["value"] != c.last || len(n.calls) != c.calls {
			t.Fatalf("%s: %v %v after %d calls", name, out, err, len(n.calls))
		}
		actx.History.Close()
		var inv, comp int
		for _, r := range historyOf(t, actx.History.Path()) {
			switch r.Kind {
			case tr.HistoryInvoke:
				inv++
				if r.Args["key"] != "k" || r.Args["attempt"] != float64(inv) {
					t.Fatalf("%s: attempt %d args %v", name, inv, r.Args)
				}
			case tr.HistoryComplete:
				comp++
				if _, folded := r.Result["attempts"]; folded {
					t.Fatalf("%s: attempts folded into one record: %v", name, r.Result)
				}
			}
		}
		if inv != c.calls || comp != c.calls {
			t.Fatalf("%s: %d invokes, %d completes for %d calls", name, inv, comp, c.calls)
		}
	}
}

// The run's answer counts: attempts per verb and answers per code.
func TestAnswerCounts(t *testing.T) {
	n := &node{answers: []answer{{out: `{"code":"busy","message":"later"}`, code: 1}, {code: 0}}}
	x, actx := setup(t, n, &subject{f: map[string]string{}})
	_, _ = driven(x, "p_put")(context.Background(), actx, tr.Action{Params: map[string]string{"volume": "v1", "key": "k", "n": "60"}})
	got := Counts(actx.History)
	put, _ := got["put"].(map[string]any)
	codes, _ := put["codes"].(map[string]any)
	if put["attempted"] != 2 || codes["busy"] != 1 {
		t.Fatalf("%v", got)
	}
	if len(Counts(actx.History)) != 0 {
		t.Fatal("counts kept after they were taken")
	}
}

// A computed configuration key is the product's: the executor reads it from
// Compute, never from the params or vars; a key the product cannot compute
// puts its why where a step reads it, and the command does not run. Red
// controls: no Compute, a key Compute does not supply, a computed key with
// a default.
func TestComputedConfiguration(t *testing.T) {
	ch := strings.Replace(head, `{"name": "client", "default": "c1"}`, `{"name": "client", "default": "c1"}, {"name": "listing", "computed": "the bin's listing"}`, 1)
	good := `"actions": [{"action": "o", "verb": "observe", "kind": "observe", "runs_on": "step_node", "params": [{"name": "volume", "kind": "subject"}],
	  "record": {"volume": "{{volume.name}}"},
	  "steps": [{"name": "s", "command": ["show", "{{config.listing}}"], "runner_deadline_seconds": 1, "cite": "x.go:1",
	    "read": {"kind": "json_strings", "into": "l", "error_into": "e", "error": "listing: {{stderr_first_line}}"}}]}], "exceptions": []}`
	f, err := Parse([]byte(ch + good))
	if err != nil {
		t.Fatal(err)
	}
	run := func(compute func(map[string]string) (map[string]string, map[string]string)) (*node, map[string]any, error) {
		n := &node{answers: []answer{{out: `["x"]`}}}
		p := filepath.Join(t.TempDir(), "h.jsonl")
		h, err := tr.OpenHistory(p)
		if err != nil {
			t.Fatal(err)
		}
		defer h.Close()
		sub := &subject{f: map[string]string{}}
		x := &Executor{File: f, Lookup: lookup, Compute: compute,
			Node:    func(*tr.ActionContext, string) (Runner, error) { return &SimHost{Inner: n}, nil },
			Subject: func(_ *tr.ActionContext, name string) (Subject, error) { sub.f["name"] = name; return sub, nil }}
		actx := &tr.ActionContext{History: h, Vars: map[string]string{"bin": "/b", "listing": "from a var"}, Log: func(string, ...interface{}) {}}
		_, err = x.Handler("o")(context.Background(), actx, tr.Action{Params: map[string]string{"volume": "v1"}})
		var res map[string]any
		if b, _ := os.ReadFile(p); len(b) > 0 {
			rs := readRecords(t, p)
			res = rs[len(rs)-1].Result
		}
		return n, res, err
	}
	n, _, err := run(func(cfg map[string]string) (map[string]string, map[string]string) {
		return map[string]string{"listing": "L:" + cfg["bin"]}, nil
	})
	if err != nil || len(n.calls) != 1 || !strings.HasSuffix(n.calls[0], "show L:/b") {
		t.Fatalf("green: %v %q", err, n.calls)
	}
	n, res, err := run(func(map[string]string) (map[string]string, map[string]string) {
		return nil, map[string]string{"listing": "no listing for /b"}
	})
	if err != nil || len(n.calls) != 0 || res["e"] != "no listing for /b" {
		t.Fatalf("uncomputed: %v %q %v", err, n.calls, res)
	}
	if _, _, err := run(nil); err == nil {
		t.Error("red control: no Compute ran")
	}
	if _, _, err := run(func(map[string]string) (map[string]string, map[string]string) { return nil, nil }); err == nil {
		t.Error("red control: a key Compute does not supply ran")
	}
	if _, err := Parse([]byte(strings.Replace(ch, `"computed": "the bin's listing"`, `"computed": "the bin's listing", "default": "d"`, 1) + good)); err == nil {
		t.Error("red control: a computed key with a default loaded")
	}
}

// An observation records when its reading began twice: on the history's
// clock (from_mono_ns) and on the runner's wall clock (from_wall_ns), both
// taken before the first step runs. Red controls: a wall start before the
// call, after it, or past the record's own end.
func TestObservationRecordsWallStart(t *testing.T) {
	good := `"actions": [{"action": "o", "verb": "observe", "kind": "observe", "runs_on": "step_node", "params": [{"name": "volume", "kind": "subject"}],
	  "record": {"volume": "{{volume.name}}"},
	  "steps": [{"name": "s", "command": ["show"], "runner_deadline_seconds": 1, "cite": "x.go:1",
	    "read": {"kind": "json_strings", "into": "l", "error_into": "e", "error": "listing: {{stderr_first_line}}"}}]}], "exceptions": []}`
	f, err := Parse([]byte(head + good))
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "h.jsonl")
	h, err := tr.OpenHistory(p)
	if err != nil {
		t.Fatal(err)
	}
	n := &node{answers: []answer{{out: `["x"]`}}}
	sub := &subject{f: map[string]string{}}
	x := &Executor{File: f, Lookup: lookup,
		Node:    func(*tr.ActionContext, string) (Runner, error) { return &SimHost{Inner: n}, nil },
		Subject: func(_ *tr.ActionContext, name string) (Subject, error) { sub.f["name"] = name; return sub, nil }}
	actx := &tr.ActionContext{History: h, Vars: map[string]string{"bin": "/b"}, Log: func(string, ...interface{}) {}}
	lo := time.Now().UnixNano()
	if _, err := x.Handler("o")(context.Background(), actx, tr.Action{Params: map[string]string{"volume": "v1"}}); err != nil {
		t.Fatal(err)
	}
	hi := time.Now().UnixNano()
	h.Close()
	rs := readRecords(t, p)
	r := rs[0] // then the end record Close writes
	if r.Kind != tr.HistoryObserve || len(n.calls) != 1 {
		t.Fatalf("%+v after %d calls", r, len(n.calls))
	}
	if r.FromWallNs < lo || r.FromWallNs > hi {
		t.Errorf("from_wall_ns %d outside the call [%d, %d]", r.FromWallNs, lo, hi)
	}
	if end := r.FromWallNs + (r.MonoNs - r.FromNs); r.MonoNs < r.FromNs || end > hi {
		t.Errorf("the reading's end in wall time %d (from_mono_ns %d, mono_ns %d) after the call ended %d", end, r.FromNs, r.MonoNs, hi)
	}
	b, _ := os.ReadFile(p)
	if !strings.Contains(string(b), `"from_wall_ns":`) {
		t.Errorf("no from_wall_ns in %s", b)
	}
}

// Red control: the executor never loops. One call of an action with a
// declared rerun is ONE attempt (attempt 1, one invoke and one complete),
// whatever it answers; the attempt number is the driver's param.
func TestExecutorRunsOneAttempt(t *testing.T) {
	busy := answer{out: `{"code":"busy","message":"later"}`, code: 1}
	n := &node{answers: []answer{busy, busy, {code: 0}}}
	x, actx := setup(t, n, &subject{f: map[string]string{}})
	out, err := x.Handler("p_put")(context.Background(), actx, tr.Action{Params: map[string]string{"volume": "v1", "key": "k", "n": "60"}})
	if err != nil || len(out) != 1 || out["value"] != tr.OutcomeRefused || len(n.calls) != 1 {
		t.Fatalf("%v %v after %d calls", out, err, len(n.calls))
	}
	out, err = x.Handler("p_put")(context.Background(), actx, tr.Action{Params: map[string]string{"volume": "v1", "key": "k", "n": "60", "attempt": "2"}})
	if err != nil || out["value"] != tr.OutcomeRefused || out["detail"] != "busy: later" || len(n.calls) != 2 {
		t.Fatalf("attempt 2: %v %v after %d calls", out, err, len(n.calls))
	}
	actx.History.Close()
	var attempts []any
	for _, r := range historyOf(t, actx.History.Path()) {
		if r.Kind == tr.HistoryInvoke {
			attempts = append(attempts, r.Args["attempt"])
		}
	}
	if len(attempts) != 2 || attempts[0] != float64(1) || attempts[1] != float64(2) {
		t.Fatalf("attempts %v", attempts)
	}
	// A bad attempt number, or one on an action with no declared rerun, is a
	// runner error that records nothing.
	for name, act := range map[string]tr.Action{
		"attempt 0":         {Params: map[string]string{"volume": "v1", "key": "k", "attempt": "0"}},
		"attempt x":         {Params: map[string]string{"volume": "v1", "key": "k", "attempt": "x"}},
		"attempt, no rerun": {Action: "p_make", Params: map[string]string{"volume": "v1", "attempt": "1"}},
	} {
		n := &node{answers: []answer{{code: 0}}}
		x, actx := setup(t, n, &subject{f: map[string]string{}})
		h := x.Handler("p_put")
		if act.Action != "" {
			h = x.Handler(act.Action)
		}
		if _, err := h(context.Background(), actx, act); err == nil || len(n.calls) != 0 {
			t.Fatalf("%s: %v, %d calls", name, err, len(n.calls))
		}
	}
}

// Rerun is the declaration read by the driver: only the page's answer, never
// after ok or a transport failure, at most max_attempts (the count is the
// bound), within_param only a guard, an expected refusal only with its
// param, and no rerun for an action that declares none.
func TestRerunIsTheDeclaration(t *testing.T) {
	x, _ := setup(t, &node{}, &subject{f: map[string]string{}})
	p := map[string]string{"volume": "v1", "key": "k", "n": "60"}
	busy := map[string]string{"value": tr.OutcomeRefused, "detail": "busy: later"}
	for name, c := range map[string]struct {
		action string
		params map[string]string
		n      int
		since  time.Duration
		out    map[string]string
		again  bool
		why    string
	}{
		"the page's answer":                       {"p_put", p, 1, 0, busy, true, ""},
		"the last attempt":                        {"p_put", p, 4, 0, busy, true, ""},
		"past max_attempts":                       {"p_put", p, 5, 0, busy, false, "max_attempts"},
		"an answer the page does not name":        {"p_put", p, 1, 0, map[string]string{"value": tr.OutcomeUnknown, "detail": "busy: never"}, false, ""},
		"another code":                            {"p_put", p, 1, 0, map[string]string{"value": tr.OutcomeUnknown, "detail": "nope: later"}, false, ""},
		"ok":                                      {"p_put", p, 1, 0, map[string]string{"value": tr.OutcomeOK, "detail": "busy: later"}, false, ""},
		"an unknown with the page's answer":       {"p_put", p, 1, 0, map[string]string{"value": tr.OutcomeUnknown, "detail": "busy: later"}, false, ""},
		"a refused_effect with the page's answer": {"p_put", p, 1, 0, map[string]string{"value": tr.OutcomeRefusedEffect, "detail": "busy: later"}, false, ""},
		"a transport failure":                     {"p_put", p, 1, 0, map[string]string{"value": tr.OutcomeUnknown, "detail": "busy: later", "transport": "true"}, false, ""},
		"past the guard":                          {"p_put", p, 1, 61 * time.Second, busy, false, "guard"},
		"the guard's default":                     {"p_put", map[string]string{"volume": "v1", "key": "k"}, 1, 8 * time.Second, busy, false, "guard"},
		"an expected refusal":                     {"p_put", map[string]string{"volume": "v1", "key": "k", "n": "60", "expect": "refused"}, 1, 0, busy, false, ""},
		"an action with no rerun":                 {"p_make", map[string]string{"volume": "v1"}, 1, 0, busy, false, ""},
		"an action not declared":                  {"p_none", p, 1, 0, busy, false, ""},
	} {
		again, pause, why := x.Rerun(c.action, c.params, c.n, c.since, c.out)
		if again != c.again || why != c.why || (again && pause != time.Millisecond) {
			t.Errorf("%s: again %v pause %v why %q", name, again, pause, why)
		}
	}
	if x.RequestArg("p_put") != "key" || x.RequestArg("p_make") != "" {
		t.Fatalf("request args %q %q", x.RequestArg("p_put"), x.RequestArg("p_make"))
	}
}
