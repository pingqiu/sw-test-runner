package declared

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	tr "github.com/pingqiu/sw-test-runner"
)

const processDesc = head + `
 "actions": [
  {"action": "p_start", "verb": "start", "op": "Svc.Start", "kind": "process", "runs_on": "step_node",
   "params": [{"name": "svc", "kind": "subject", "vars": true}, {"name": "ready_seconds", "kind": "int", "default": "2"}],
   "runner_deadline_seconds": 30, "plus_param": {"name": "ready_seconds", "default": 2},
   "record": {"volume": "{{svc.name}}", "args": {"min_epoch": {"template": "{{svc.epoch}}", "type": "int"}}},
   "process": {"op": "start", "subject": "svc", "name": "svcd", "cite": "x.go:1",
     "before": [{"name": "free", "kind": "ports_free", "addresses": ["{{svc.addr}}"], "cite": "x.go:2"}],
     "argv": ["{{config.bin}}/svcd", "--addr", "{{svc.addr}}"],
     "log": "/logs/{{svc.name}}.{{now_ns}}.log",
     "ready": {"within_param": "ready_seconds", "every_ms": 10,
       "probes": [{"name": "status", "kind": "json", "command": ["curl", "http://{{svc.addr}}/s"], "schema": {"Name": "string", "Epoch": "uint"},
          "holds": [{"path": "Name", "equals": "{{svc.name}}"}, {"path": "Epoch", "above": "{{svc.epoch}}"}], "capture": {"epoch": "Epoch"}, "cite": "x.go:3"},
         {"name": "own", "kind": "listeners_held", "addresses": ["{{svc.addr}}"], "cite": "x.go:4"}],
       "not_ready": "not ready within {{ready.within}}", "not_ready_by_deadline": "not ready by the deadline"},
     "on_ready_set": {"svc.epoch": "{{ready.epoch}}"},
     "result_ready": {"epoch": {"template": "{{ready.epoch}}", "type": "int"}}},
   "on_outcome_set": {"ok": {"svc.state": "up"}, "unknown": {"svc.state": "unsure"}}},
  {"action": "p_kill", "verb": "kill", "op": "Env.Kill", "kind": "process", "runs_on": "step_node",
   "params": [{"name": "proc", "kind": "process"}, {"name": "kill_seconds", "kind": "int", "default": "0"}],
   "runner_deadline_seconds": 30,
   "record": {"volume": "", "args": {"proc": "{{proc}}", "pid": {"template": "{{proc.pid}}", "type": "int", "optional": true}}},
   "process": {"op": "kill", "subject": "proc", "name": "process", "within_param": "kill_seconds", "cite": "x.go:5"}},
  {"action": "p_restart", "verb": "restart", "op": "Env.Restart", "kind": "process", "runs_on": "step_node",
   "params": [{"name": "proc", "kind": "process"}, {"name": "ready_seconds", "kind": "int", "default": "1"}],
   "runner_deadline_seconds": 30,
   "coded": {"param": "proc", "equals": "boss", "handler": "p_restart_boss", "why": "the boss restarts another way"},
   "record": {"volume": "", "args": {"proc": "{{proc}}"}},
   "process": {"op": "restart", "subject": "proc", "name": "svcd", "cite": "x.go:6",
     "log": "/logs/r.{{now_ns}}.log",
     "ready": {"within_param": "ready_seconds", "every_ms": 10,
       "probes": [{"name": "own", "kind": "listeners_held", "addresses": ["{{proc.addr}}"], "cite": "x.go:4"}],
       "not_ready": "not ready within {{ready.within}}", "not_ready_by_deadline": "not ready by the deadline"}}}
 ],
 "exceptions": [{"action": "p_restart_boss", "why": "another launcher", "declarable_with": "a launcher step", "runner_deadline_seconds": 30}]}`

func processSetup(t *testing.T, n Runner, subs map[string]*subject) (*Executor, *tr.ActionContext, string) {
	t.Helper()
	x, actx, p := deviceSetupDesc(t, processDesc, n, nil)
	x.Subject = func(_ *tr.ActionContext, name string) (Subject, error) {
		s := subs[name]
		s.f["name"] = name
		return s, nil
	}
	x.Process = func(_ context.Context, _ *tr.ActionContext, _ tr.Action, _ Runner, name string) (Subject, string) {
		if s, ok := subs[name]; ok {
			return s, ""
		}
		return nil, "unknown process " + name
	}
	return x, actx, p
}

const held = "LISTEN 0 4096 127.0.0.1:9001 0.0.0.0:* users:((\"svcd\",pid=5555,fd=9))"

// A start: endpoints free, the launch through the wrapper, the identity
// recorded, ready only from its own status above the recorded epoch AND
// its own listener; the outcome sets the subject's state.
func TestProcessStartCreditsOnlyItsOwnProcess(t *testing.T) {
	n := (&ruleNode{}).on("ss -Hltnp", answer{}, answer{out: held}).on("setsid sh -c", answer{out: "5555\n"}).
		on("/proc/5555", answer{out: "start 77\n"}).on("curl ", answer{out: `{"Name":"s1","Epoch":3}`})
	sub := &subject{f: map[string]string{"addr": "127.0.0.1:9001", "epoch": "2"}}
	x, actx, p := processSetup(t, n, map[string]*subject{"s1": sub})
	out, err := x.Handler("p_start")(context.Background(), actx, tr.Action{Params: map[string]string{"svc": "s1"}})
	if err != nil || out["value"] != tr.OutcomeOK {
		t.Fatalf("%v %v", out, err)
	}
	if sub.f["pid"] != "5555" || sub.f["pid_start"] != "77" || sub.f["epoch"] != "3" || sub.f["state"] != "up" || !strings.HasSuffix(sub.f["exit_file"], ".log.exit") || sub.f["launch_argv"] != `["/b/svcd","--addr","127.0.0.1:9001"]` {
		t.Fatalf("subject %+v", sub.f)
	}
	rs := readRecords(t, p)
	if rs[0].Args["min_epoch"] != float64(2) || rs[1].Result["epoch"] != float64(3) || rs[1].Result["pid"] != "5555" {
		t.Fatalf("records %+v", rs)
	}
	// Red controls: another process holds the listener; the status is at the
	// recorded epoch; the process dies; an endpoint is taken before the launch.
	for name, n := range map[string]*ruleNode{
		"listener another's": (&ruleNode{}).on("ss -Hltnp", answer{}, answer{out: strings.ReplaceAll(held, "5555", "8")}).on("setsid sh -c", answer{out: "5555\n"}).
			on("/proc/5555", answer{out: "start 77\n"}).on("curl ", answer{out: `{"Name":"s1","Epoch":3}`}),
		"at the recorded epoch": (&ruleNode{}).on("ss -Hltnp", answer{}, answer{out: held}).on("setsid sh -c", answer{out: "5555\n"}).
			on("/proc/5555", answer{out: "start 77\n"}).on("curl ", answer{out: `{"Name":"s1","Epoch":2}`}),
		"died": (&ruleNode{}).on("ss -Hltnp", answer{}, answer{out: held}).on("setsid sh -c", answer{out: "5555\n"}).
			on("/proc/5555", answer{out: "start 77\n"}, answer{out: "absent\n"}).on("curl ", answer{out: `{"Name":"s1","Epoch":1}`}),
		"taken": (&ruleNode{}).on("ss -Hltnp", answer{out: held}),
	} {
		sub := &subject{f: map[string]string{"addr": "127.0.0.1:9001", "epoch": "2"}}
		x, actx, _ := processSetup(t, n, map[string]*subject{"s1": sub})
		out, _ := x.Handler("p_start")(context.Background(), actx, tr.Action{Params: map[string]string{"svc": "s1", "ready_seconds": "0"}})
		if out["value"] != tr.OutcomeUnknown || sub.f["state"] != "unsure" {
			t.Errorf("%s: %v, subject %+v", name, out, sub.f)
		}
		if name == "taken" && n.issued("setsid") != 0 {
			t.Errorf("taken: launched although the endpoint was held")
		}
	}
}

func argv64(argv ...string) string {
	return base64.StdEncoding.EncodeToString([]byte(strings.Join(argv, "\x00") + "\x00"))
}

// A kill: by pid and start time in one shell, recorded as killed, ok only
// when a check finds it gone. Red controls: another process now holds the
// pid (refused, nothing recorded as killed); no identity; an unknown name.
func TestProcessKillByRecordedIdentity(t *testing.T) {
	sent := answer{out: "sent " + argv64("/b/svcd", "--addr", "x") + "\n"}
	n := (&ruleNode{}).on("kill -KILL", sent).on("/proc/5555", answer{out: "absent\n"})
	sub := &subject{f: map[string]string{"pid": "5555", "pid_start": "77"}}
	x, actx, p := processSetup(t, n, map[string]*subject{"s1": sub})
	if out, err := x.Handler("p_kill")(context.Background(), actx, tr.Action{Params: map[string]string{"proc": "s1"}}); err != nil || out["value"] != tr.OutcomeOK {
		t.Fatalf("%v %v", out, err)
	}
	if !strings.Contains(n.calls[0], `[ "${20}" = 77 ]`) || !strings.Contains(n.calls[0], "kill -KILL 5555") {
		t.Fatalf("not by identity: %q", n.calls[0])
	}
	if sub.f["killed_pid"] != "5555" || sub.f["killed_argv"] != `["/b/svcd","--addr","x"]` || !sub.saved {
		t.Fatalf("subject %+v", sub.f)
	}
	if rs := readRecords(t, p); rs[0].Args["pid"] != float64(5555) {
		t.Fatalf("records %+v", rs)
	}
	for name, c := range map[string]struct {
		n    *ruleNode
		sub  *subject
		proc string
		want string
	}{
		"another process holds the pid": {(&ruleNode{}).on("kill -KILL", answer{out: "gone\n"}), &subject{f: map[string]string{"pid": "5555", "pid_start": "77"}}, "s1", tr.OutcomeRefused},
		"no start time":                 {&ruleNode{}, &subject{f: map[string]string{"pid": "5555"}}, "s1", tr.OutcomeRefused},
		"unknown name":                  {&ruleNode{}, &subject{f: map[string]string{}}, "s9", tr.OutcomeRefused},
		"still alive":                   {(&ruleNode{}).on("kill -KILL", sent).on("/proc/5555", answer{out: "start 77\n"}), &subject{f: map[string]string{"pid": "5555", "pid_start": "77"}}, "s1", tr.OutcomeUnknown},
	} {
		x, actx, p := processSetup(t, c.n, map[string]*subject{"s1": c.sub})
		out, err := x.Handler("p_kill")(context.Background(), actx, tr.Action{Params: map[string]string{"proc": c.proc}})
		if err != nil || out["value"] != c.want {
			t.Errorf("%s: %v %v", name, out, err)
		}
		if c.want == tr.OutcomeRefused && (c.sub.f["killed_pid"] != "" || c.n.issued("kill -KILL") > 1) {
			t.Errorf("%s: a refusal changed something: %+v", name, c.sub.f)
		}
		if rs := readRecords(t, p); len(rs) < 2 || rs[0].Args["proc"] != c.proc {
			t.Errorf("%s: records %+v", name, rs)
		}
	}
}

// A restart: only a process the runner killed and that no longer runs,
// relaunched with its recorded command line; the new identity clears the
// killed one. The coded case goes to the product's handler.
func TestProcessRestartOnlyWhatTheRunnerKilled(t *testing.T) {
	n := (&ruleNode{}).on("/proc/5555", answer{out: "absent\n"}).on("setsid sh -c", answer{out: "6666\n"}).
		on("/proc/6666", answer{out: "start 88\n"}).on("ss -Hltnp", answer{out: strings.ReplaceAll(held, "5555", "6666")})
	sub := &subject{f: map[string]string{"pid": "5555", "pid_start": "77", "killed_pid": "5555", "killed_argv": `["/b/svcd","--x","a b"]`, "addr": "127.0.0.1:9001"}}
	x, actx, _ := processSetup(t, n, map[string]*subject{"s1": sub})
	if out, err := x.Handler("p_restart")(context.Background(), actx, tr.Action{Params: map[string]string{"proc": "s1"}}); err != nil || out["value"] != tr.OutcomeOK {
		t.Fatalf("%v %v", out, err)
	}
	if n.issued("' sh /b/svcd --x 'a b' >/dev/null") != 1 || sub.f["pid"] != "6666" || sub.f["killed_pid"] != "" || sub.f["killed_argv"] != "" {
		t.Fatalf("calls %q subject %+v", n.calls, sub.f)
	}
	for name, sub := range map[string]*subject{
		"never killed":  {f: map[string]string{"pid": "5555", "pid_start": "77", "killed_argv": `["/b/svcd"]`}},
		"no argv":       {f: map[string]string{"pid": "5555", "pid_start": "77", "killed_pid": "5555"}},
		"still running": {f: map[string]string{"pid": "5555", "pid_start": "1", "killed_pid": "5555", "killed_argv": `["/b/svcd"]`}},
	} {
		n := (&ruleNode{}).on("/proc/5555", answer{out: "start 1\n"})
		if name != "still running" {
			n = (&ruleNode{}).on("/proc/5555", answer{out: "absent\n"})
		}
		x, actx, _ := processSetup(t, n, map[string]*subject{"s1": sub})
		if out, _ := x.Handler("p_restart")(context.Background(), actx, tr.Action{Params: map[string]string{"proc": "s1"}}); out["value"] != tr.OutcomeRefused || n.issued("setsid") != 0 {
			t.Errorf("%s: %v %q", name, out, n.calls)
		}
	}
	// The coded case.
	x, actx, _ = processSetup(t, &ruleNode{}, map[string]*subject{})
	called := false
	x.Coded = func(name string) (tr.ActionHandlerFunc, bool) {
		return func(context.Context, *tr.ActionContext, tr.Action) (map[string]string, error) {
			called = name == "p_restart_boss"
			return map[string]string{"value": "ok"}, nil
		}, true
	}
	if _, err := x.Handler("p_restart")(context.Background(), actx, tr.Action{Params: map[string]string{"proc": "boss"}}); err != nil || !called {
		t.Fatalf("coded: %v %v", called, err)
	}
}

func TestProcessParseRefusals(t *testing.T) {
	if _, err := Parse([]byte(processDesc)); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string][2]string{
		"pid named before the launch":  {`"addresses": ["{{svc.addr}}"], "cite": "x.go:2"`, `"addresses": ["{{launched.pid}}"], "cite": "x.go:2"`},
		"capture named before ready":   {`"log": "/logs/{{svc.name}}.{{now_ns}}.log"`, `"log": "/logs/{{ready.epoch}}.log"`},
		"a refusal that changes state": {`"unknown": {"svc.state": "unsure"}`, `"refused": {"svc.state": "unsure"}`},
		"a restart with its own argv":  {`"op": "restart", "subject": "proc", "name": "svcd", "cite": "x.go:6",`, `"op": "restart", "subject": "proc", "name": "svcd", "cite": "x.go:6", "argv": ["x"],`},
		"coded to no exception":        {`"handler": "p_restart_boss"`, `"handler": "p_other"`},
		"ports_free as a ready probe":  {`{"name": "own", "kind": "listeners_held", "addresses": ["{{svc.addr}}"]`, `{"name": "own", "kind": "ports_free", "addresses": ["{{svc.addr}}"]`},
		"a kill without its bound":     {`"within_param": "kill_seconds"`, `"within_param": "nope"`},
	} {
		bad := strings.Replace(processDesc, c[0], c[1], 1)
		if bad == processDesc {
			t.Fatalf("%s: the seed did not apply", name)
		}
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
