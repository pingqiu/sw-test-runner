package declared

import (
	"context"
	"strings"
	"testing"

	tr "github.com/pingqiu/sw-test-runner"
)

const sequenceDesc = head + `
 "actions": [
  {"action": "p_handover", "verb": "handover", "op": "Svc.Handover", "kind": "sequence", "runs_on": "step_node",
   "params": [{"name": "svc", "kind": "subject", "vars": true}],
   "runner_deadline_seconds": 30,
   "record": {"volume": "{{svc.name}}", "args": {"epoch": {"template": "{{svc.epoch}}", "type": "int"}}},
   "sequence": {"changes_from": "seal", "result": {"epoch": {"template": "{{svc.epoch}}", "type": "int"}},
    "steps": [
     {"name": "check", "kind": "command", "command": ["probe", "{{svc.name}}"], "judge": {"kind": "error_body", "path": "probe"},
      "failure": "check: {{detail}}", "cite": "x.go:1"},
     {"name": "seal", "kind": "command", "command": ["seal", "{{svc.name}}"], "judge": {"kind": "error_body", "path": "seal"},
      "failure": "seal: {{detail}}", "result_failure": {"seal": "{{outcome}}"},
      "answer": {"kind": "json", "schema": {"name": "string", "gen": "uint"}, "unreadable": "seal answer {{stdout_first_line|q}}",
        "checks": [{"holds": [{"path": "name", "equals": "{{svc.name}}"}, {"path": "gen", "above": "0"}], "failure": "seal answered {{answer.name}} gen {{answer.gen}}"}],
        "capture": {"gen": "gen"}},
      "result": {"gen": {"template": "{{step.seal.gen}}", "type": "int"}}, "cite": "x.go:2"},
     {"name": "revoke", "kind": "command", "command": ["revoke", "--gen", "{{step.seal.gen}}"], "judge": {"kind": "exit_code", "path": "revoke"},
      "failure": "revoke: {{output_first_line}}", "result_failure": {"revoke_exit": {"template": "{{exit}}", "type": "int"}}, "cite": "x.go:3"},
     {"name": "gone", "kind": "wait_gone", "pid": "{{svc.pid}}", "start": "{{svc.pid_start}}", "within_seconds": 1,
      "failure": "still there: {{why}}", "on_ok_set": {"svc.pid": ""}, "cite": "x.go:4"},
     {"name": "ack", "kind": "poll", "command": ["show"], "pattern": "ack {{svc.name}}@(\\d+)", "every_ms": 10, "within_seconds": 1,
      "capture": {"at": "1"}, "result": {"ack": "{{step.ack.at}}"}, "failure": "no ack", "cite": "x.go:5"}
    ]},
   "on_outcome_set": {"ok": {"svc.state": "released"}, "unknown": {"svc.state": "unsure"}}},
  {"action": "p_move", "verb": "move", "op": "Svc.Move", "kind": "sequence", "runs_on": "step_node",
   "params": [{"name": "svc", "kind": "subject", "vars": true}],
   "runner_deadline_seconds": 30,
   "record": {"volume": "{{svc.name}}", "args": {}},
   "sequence": {"steps_of": {"action": "p_handover", "detail_prefix": "handover: "}, "changes_from": "seal",
    "steps": [{"name": "move", "kind": "command", "command": ["move", "--gen", "{{step.seal.gen}}"], "judge": {"kind": "exit0"},
      "failure": "move: {{output_first_line}}",
      "answer": {"kind": "first_line", "pattern": "^moved (\\S+) to (\\S+)$", "match": {"1": "{{svc.name}}"}, "failure": "not this move", "capture": {"to": "2"}},
      "on_ok_set": {"svc.host": "{{step.move.to}}"}, "cite": "x.go:6"}]}}
 ]}`

func seqRules(over map[string][]answer) *ruleNode {
	n := &ruleNode{}
	base := []struct {
		m  string
		as []answer
	}{
		{"probe ", []answer{{}}},
		{"seal ", []answer{{out: `{"name":"s1","gen":7}`}}},
		{"revoke ", []answer{{}}},
		{"/proc/5555", []answer{{out: "absent\n"}}},
		{"show", []answer{{out: "nothing\n"}, {out: "ack s1@9\n"}}},
		{"move ", []answer{{out: "moved s1 to h2\n"}}},
	}
	for _, b := range base {
		if o, ok := over[b.m]; ok {
			b.as = o
		}
		n.on(b.m, b.as...)
	}
	return n
}

func seqSetup(t *testing.T, n Runner, sub *subject) (*Executor, *tr.ActionContext, string) {
	t.Helper()
	return deviceSetupDesc(t, sequenceDesc, n, sub)
}

func seqSubject() *subject {
	return &subject{f: map[string]string{"epoch": "3", "pid": "5555", "pid_start": "77"}}
}

// Steps run in order, a later step's command names the generation an
// earlier step's answer decoded, the result gathers each step's fields,
// and the outcome sets the subject.
func TestSequencePassesDecodedFieldsOn(t *testing.T) {
	n := seqRules(nil)
	sub := seqSubject()
	x, actx, p := seqSetup(t, n, sub)
	if out, err := x.Handler("p_handover")(context.Background(), actx, tr.Action{Params: map[string]string{"svc": "s1"}}); err != nil || out["value"] != tr.OutcomeOK {
		t.Fatalf("%v %v", out, err)
	}
	if n.issued("revoke --gen 7") != 1 || sub.f["pid"] != "" || sub.f["state"] != "released" {
		t.Fatalf("calls %q subject %+v", n.calls, sub.f)
	}
	rs := readRecords(t, p)
	if r := rs[1].Result; r["gen"] != float64(7) || r["ack"] != "9" || r["epoch"] != float64(3) {
		t.Fatalf("result %+v", r)
	}
	// The included steps come first, then the action's own.
	n = seqRules(nil)
	sub = seqSubject()
	x, actx, _ = seqSetup(t, n, sub)
	if out, _ := x.Handler("p_move")(context.Background(), actx, tr.Action{Params: map[string]string{"svc": "s1"}}); out["value"] != tr.OutcomeOK || sub.f["host"] != "h2" || n.issued("move --gen 7") != 1 {
		t.Fatalf("move: %v %+v %q", out, sub.f, n.calls)
	}
}

// The outcome rule: a refusal before the first changing step is the
// step's own; at or after it, any failure is unknown (red control: the
// seal's own answer is a refusal by the table, yet the operation is
// unknown); and nothing after a failed step runs.
func TestSequenceOutcomeIsUnknownAfterTheFirstChange(t *testing.T) {
	refused := answer{out: `{"code":"nope","message":"not now"}`, code: 1}
	for name, c := range map[string]struct {
		over   map[string][]answer
		action string
		want   string
		detail string
		never  string
	}{
		"refused before any change":    {map[string][]answer{"probe ": {refused}}, "p_handover", tr.OutcomeRefused, "check: nope: not now", "seal "},
		"refused at the first change":  {map[string][]answer{"seal ": {refused}}, "p_handover", tr.OutcomeUnknown, "seal: nope: not now", "revoke "},
		"an answer that does not hold": {map[string][]answer{"seal ": {{out: `{"name":"s2","gen":7}`}}}, "p_handover", tr.OutcomeUnknown, "seal answered s2 gen 7", "revoke "},
		"unreadable":                   {map[string][]answer{"seal ": {{out: "x\ny"}}}, "p_handover", tr.OutcomeUnknown, `seal answer "x"`, "revoke "},
		"revoke exit":                  {map[string][]answer{"revoke ": {{errOut: "denied", code: 2}}}, "p_handover", tr.OutcomeUnknown, "revoke: denied", "show"},
		"never gone":                   {map[string][]answer{"/proc/5555": {{out: "start 77\n"}}}, "p_handover", tr.OutcomeUnknown, "still there: pid 5555 still running after 1s", "show"},
		"no ack":                       {map[string][]answer{"show": {{out: "nothing\n"}}}, "p_handover", tr.OutcomeUnknown, "no ack", "move "},
		"an included step's prefix":    {map[string][]answer{"show": {{out: "nothing\n"}}}, "p_move", tr.OutcomeUnknown, "handover: no ack", "move "},
		"not this move":                {map[string][]answer{"move ": {{out: "moved s9 to h2\n"}}}, "p_move", tr.OutcomeUnknown, "not this move", ""},
	} {
		n := seqRules(c.over)
		sub := seqSubject()
		x, actx, p := seqSetup(t, n, sub)
		out, err := x.Handler(c.action)(context.Background(), actx, tr.Action{Params: map[string]string{"svc": "s1"}})
		if err != nil || out["value"] != c.want {
			t.Errorf("%s: %v %v", name, out, err)
			continue
		}
		rs := readRecords(t, p)
		if rs[1].Result["detail"] != c.detail {
			t.Errorf("%s: detail %q, want %q", name, rs[1].Result["detail"], c.detail)
		}
		if c.never != "" && n.issued(c.never) != 0 {
			t.Errorf("%s: %q ran after the failure", name, c.never)
		}
		if c.want == tr.OutcomeRefused && sub.f["state"] != "" {
			t.Errorf("%s: a refusal changed the subject: %+v", name, sub.f)
		}
		if c.action == "p_handover" && c.want == tr.OutcomeUnknown && sub.f["state"] != "unsure" {
			t.Errorf("%s: unknown did not unsettle: %+v", name, sub.f)
		}
	}
}

func TestSequenceParseRefusals(t *testing.T) {
	if _, err := Parse([]byte(sequenceDesc)); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string][2]string{
		"a field of a later step":          {`"command": ["probe", "{{svc.name}}"]`, `"command": ["probe", "{{step.seal.gen}}"]`},
		"a field no step captures":         {`"command": ["revoke", "--gen", "{{step.seal.gen}}"]`, `"command": ["revoke", "--gen", "{{step.seal.lsn}}"]`},
		"changes_from names no step":       {`"changes_from": "seal", "result"`, `"changes_from": "sael", "result"`},
		"steps_of a non-sequence":          {`"steps_of": {"action": "p_handover"`, `"steps_of": {"action": "p_nothing"`},
		"a step without its failure":       {`"failure": "no ack", "cite": "x.go:5"`, `"cite": "x.go:5"`},
		"two steps of one name":            {`{"name": "revoke", "kind": "command"`, `{"name": "seal", "kind": "command"`},
		"a match group not in the pattern": {`"match": {"1": "{{svc.name}}"}`, `"match": {"3": "{{svc.name}}"}`},
	} {
		bad := strings.Replace(sequenceDesc, c[0], c[1], 1)
		if bad == sequenceDesc {
			t.Fatalf("%s: the seed did not apply", name)
		}
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
