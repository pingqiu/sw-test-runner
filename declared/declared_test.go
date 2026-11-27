package declared

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tr "github.com/pingqiu/sw-test-runner"
)

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

const head = `{"product": "p", "read_at": "p 1", "config": {"keys": [{"name": "bin", "required": true}, {"name": "client", "default": "c1"}], "client": "client"},`

const desc = head + `
 "actions": [
  {"action": "p_put", "verb": "put", "op": "Store.Put", "runs_on": "step_node",
   "params": [{"name": "volume", "kind": "subject", "vars": true}, {"name": "key", "required": true}, {"name": "n", "kind": "int", "default": "7"}],
   "record": {"volume": "{{volume.name}}", "args": {"key": "{{key}}", "n": {"template": "{{n}}", "type": "int"}}},
   "variants": [
     {"name": "fast", "when": {"field": "volume.fast", "equals": "yes"}, "command": ["{{config.bin}}/put", "--fast", {"json": {"k": "{{key}}"}}], "timeout_seconds": 5, "cite": "x.go:1",
      "decode": {"kind": "error_body", "path": "put fast", "answer": "http_status_last_line", "transport_failure": "no answer (exit {{exit}})"}},
     {"name": "slow", "command": ["{{config.bin}}/put", "--key", "{{key}}", "--n", "{{n}}"], "timeout_seconds": 5, "cite": "x.go:2",
      "decode": {"kind": "error_body", "path": "put"}}
   ],
   "retry": {"while_code": "busy", "within_param": "n", "every_seconds": 1, "attempts": "attempts"}},
  {"action": "p_make", "verb": "make", "op": "Store.Make", "runs_on": "step_node", "as_root": true,
   "params": [{"name": "volume", "kind": "subject", "vars": true}],
   "record": {"volume": "{{volume.name}}", "args": {}},
   "variants": [{"command": ["mk", "{{volume.name}}"], "timeout_seconds": 5, "cite": "x.go:3",
     "decode": {"kind": "self_reported", "fields": {"outcome": "string", "volume": "string", "id": "string", "ns": "int", "error": "string"},
       "outcome_field": "outcome", "exits": {"ok": 0, "refused": 1, "unknown": 3}, "match": {"volume": "{{volume.name}}"},
       "ok_length": {"id": 4}, "result": {"ns": "ns"}, "result_ok": {"id": "id"}, "detail": "{{answer.error}} {{stderr}}"}}],
   "on_ok_set": {"volume.id": "{{answer.id}}"}}
 ],
 "exceptions": [{"action": "p_attach", "why": "a process start"}]}`

func lookup(action, path, code, message string) string {
	if code == "nope" || (code == "gone" && strings.Contains(message, "before")) {
		return tr.OutcomeRefused
	}
	return tr.OutcomeUnknown
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
		Node:    func(*tr.ActionContext, string) (Runner, error) { return n, nil },
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
	out, _ := x.Handler("p_put")(context.Background(), actx, tr.Action{Params: map[string]string{"volume": "v1", "key": "k", "n": "60"}})
	if out["value"] != tr.OutcomeOK || len(n.calls) != 3 {
		t.Fatalf("%v after %d calls", out, len(n.calls))
	}
	// Red control: an expected refusal is one call.
	n = &node{answers: []answer{busy, {code: 0}}}
	x, actx = setup(t, n, &subject{f: map[string]string{}})
	out, _ = x.Handler("p_put")(context.Background(), actx, tr.Action{Params: map[string]string{"volume": "v1", "key": "k", "n": "60", "expect": "refused"}})
	if out["value"] != tr.OutcomeUnknown || len(n.calls) != 1 {
		t.Fatalf("expected refusal: %v after %d calls", out, len(n.calls))
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
	  "variants": [{"command": ["x", "{{p}}"], "timeout_seconds": 1, "cite": "x.go:1", "decode": {"kind": "error_body", "path": "x"}}]}], "exceptions": []}`
	if _, err := Parse([]byte(head + good)); err != nil {
		t.Fatalf("green twin: %v", err)
	}
	for name, bad := range map[string][2]string{
		"no cite":               {`"cite": "x.go:1"`, `"cite": " "`},
		"no timeout":            {`"timeout_seconds": 1`, `"timeout_seconds": 0`},
		"undefined name":        {`"{{p}}"]`, `"{{q}}"]`},
		"unknown decode":        {`"kind": "error_body"`, `"kind": "grep"`},
		"no path":               {`"path": "x"`, `"path": ""`},
		"unknown field":         {`"op": "M.A"`, `"op": "M.A", "shell": "x"`},
		"declared and excepted": {`"exceptions": []`, `"exceptions": [{"action": "a", "why": "w"}]`},
		"exception without why": {`"exceptions": []`, `"exceptions": [{"action": "b", "why": ""}]`},
		"other node":            {`"runs_on": "step_node"`, `"runs_on": "anywhere"`},
	} {
		if !strings.Contains(good, bad[0]) {
			t.Fatalf("%s: seed %q not in the green twin", name, bad[0])
		}
		if _, err := Parse([]byte(head + strings.Replace(good, bad[0], bad[1], 1))); err == nil {
			t.Errorf("%s: not refused", name)
		}
	}
}
