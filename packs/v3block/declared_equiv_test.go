package v3block

// Equivalence of each declared action (product/actions.json) with the Go
// handler it replaced (frozen_ops_test.go): both run on the same scripted
// node answers, and the test requires the identical commands in the same
// order, the identical history records (the clock aside), the identical
// return value, the identical log lines and the identical runner state
// (vars). A runner error must be a runner error in both, recording nothing.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/declared"
)

type eqAnswer struct {
	out, errOut string
	code        int
	err         error
}

type eqRule struct {
	match   string
	answers []eqAnswer
}

// eqNode answers a command by the first rule whose substring it holds, each
// rule's answers in turn (the last repeats); no rule is exit 127.
type eqNode struct {
	rules []eqRule
	n     []int
	calls []string
}

func (f *eqNode) Run(_ context.Context, cmd string) (string, string, int, error) {
	f.calls = append(f.calls, cmd)
	for i, r := range f.rules {
		if strings.Contains(cmd, r.match) {
			a := r.answers[min(f.n[i], len(r.answers)-1)]
			f.n[i]++
			return a.out, a.errOut, a.code, a.err
		}
	}
	return "", "no rule", 127, nil
}

func (f *eqNode) RunRoot(ctx context.Context, cmd string) (string, string, int, error) {
	return f.Run(ctx, "ROOT "+cmd)
}

type eqCase struct {
	name      string
	params    map[string]string
	vars      map[string]string // set (or, with "", removed) over the base vars
	rules     []eqRule
	vols      []*opVolume
	noHistory bool
}

type eqRun struct {
	ret     map[string]string
	err     error
	records []tr.HistoryRecord
	calls   []string
	vars    map[string]string
	logs    []string
}

const eqLayout = "0123456789abcdef0123456789abcdef"

func eqVolumes() []*opVolume {
	vs, _ := specVolumes([]string{"v1", "tgt"}, 20000, "nqn.2026-11.io.seaweedfs:op-")
	vs[0].LayoutID = eqLayout
	return vs
}

func eqBaseVars() map[string]string {
	return map[string]string{"bin_dir": "{{work}}/bin", "work": "/w", "master": "127.0.0.1:9333", "filer": "127.0.0.1:18888",
		"chunk_path": "/chunks", "root": "/w/root"}
}

func runEq(t *testing.T, h tr.ActionHandlerFunc, c eqCase) eqRun {
	t.Helper()
	node := &eqNode{rules: c.rules, n: make([]int, len(c.rules))}
	old := opNode
	opNode = func(*tr.ActionContext, string) (runner, error) { return node, nil }
	defer func() { opNode = old }()
	vars := eqBaseVars()
	vols := c.vols
	if vols == nil {
		vols = eqVolumes()
	}
	var r eqRun
	actx := &tr.ActionContext{Vars: vars, Log: func(f string, a ...interface{}) {
		r.logs = append(r.logs, fmt.Sprintf(f, a...))
	}}
	for _, v := range vols {
		saveVolume(actx, v)
	}
	for k, v := range c.vars {
		if v == "" {
			delete(vars, k)
		} else {
			vars[k] = v
		}
	}
	p := filepath.Join(t.TempDir(), "history.jsonl")
	if !c.noHistory {
		hist, err := tr.OpenHistory(p)
		if err != nil {
			t.Fatal(err)
		}
		actx.History = hist
	}
	params := map[string]string{}
	for k, v := range c.params {
		params[k] = v
	}
	r.ret, r.err = h(context.Background(), actx, tr.Action{Params: params})
	if actx.History != nil {
		if err := actx.History.Close(); err != nil {
			t.Fatal(err)
		}
		for _, rec := range records(t, p) {
			rec.MonoNs = 0
			r.records = append(r.records, rec)
		}
	}
	r.calls, r.vars = node.calls, vars
	return r
}

// checkEquivalent runs every case through the frozen handler and the
// declared action and requires the same run.
func checkEquivalent(t *testing.T, action string, frozen tr.ActionHandlerFunc, cases []eqCase) {
	t.Helper()
	h := blockActions.Handler(action)
	for _, c := range cases {
		want, got := runEq(t, frozen, c), runEq(t, h, c)
		if (want.err == nil) != (got.err == nil) {
			t.Errorf("%s %s: error %v, frozen %v", action, c.name, got.err, want.err)
			continue
		}
		if want.err != nil && len(got.records) > 1 {
			t.Errorf("%s %s: a runner error recorded %v", action, c.name, got.records)
		}
		for what, pair := range map[string][2]any{
			"return": {want.ret, got.ret}, "records": {want.records, got.records}, "commands": {want.calls, got.calls},
			"vars": {want.vars, got.vars}, "logs": {want.logs, got.logs},
		} {
			if !reflect.DeepEqual(pair[0], pair[1]) {
				w, _ := json.MarshalIndent(pair[0], "", " ")
				g, _ := json.MarshalIndent(pair[1], "", " ")
				t.Errorf("%s %s: %s differ\nfrozen:   %s\ndeclared: %s", action, c.name, what, w, g)
			}
		}
		if len(want.calls) == 0 && want.err == nil {
			t.Errorf("%s %s: the case issued no command", action, c.name)
		}
	}
}

func chunkErr(code, msg string) eqAnswer {
	b, _ := json.Marshal(map[string]any{"code": code, "message": msg, "retryable": false})
	return eqAnswer{out: string(b) + "\n", code: 1}
}

var broken = errors.New("ssh: connection reset")

// Cases every volume action shares: the runner errors before any record.
func commonErrorCases(params map[string]string, rules []eqRule) []eqCase {
	with := func(k, v string) map[string]string {
		out := map[string]string{}
		for a, b := range params {
			out[a] = b
		}
		if v == "" {
			delete(out, k)
		} else {
			out[k] = v
		}
		return out
	}
	return []eqCase{
		{name: "no history", params: params, rules: rules, noHistory: true},
		{name: "no volume", params: with("volume", ""), rules: rules},
		{name: "volume not in the spec", params: with("volume", "nope"), rules: rules},
		{name: "bad expect", params: with("expect", "ok"), rules: rules},
		{name: "missing configuration", params: params, rules: rules, vars: map[string]string{"filer": ""}},
		{name: "unresolved configuration", params: params, rules: rules, vars: map[string]string{"bin_dir": "{{nowhere}}/bin"}},
		{name: "expected refusal recorded", params: with("expect", "refused"), rules: rules},
		{name: "configuration from params, client named", params: with("client", "c7"), rules: rules, vars: map[string]string{"master": "", "client": ""}},
	}
}

func TestDeclaredCreateEquivalent(t *testing.T) {
	ans := func(o string, code int, extra string) []eqRule {
		return []eqRule{{"v3ctl", []eqAnswer{{out: `{"outcome":"` + o + `","volume":"v1"` + extra + "}\n", errOut: "warn: one\ntwo", code: code}}}}
	}
	p := map[string]string{"volume": "tgt"}
	tgt := func(extra string, o string, code int) []eqRule {
		return []eqRule{{"v3ctl", []eqAnswer{{out: `{"outcome":"` + o + `","volume":"tgt"` + extra + "}", code: code}}}}
	}
	okRules := tgt(`,"layout_id":"`+eqLayout+`","store_ready":true,"call_ns":1234`, "ok", 0)
	cases := []eqCase{
		{name: "ok sets the layout id", params: p, rules: tgt(`,"layout_id":"`+eqLayout+`","store_ready":true,"call_ns":1234`, "ok", 0)},
		{name: "refused", params: p, rules: tgt(`,"code":"AlreadyExists","error":"exists","call_ns":7`, "refused", 1)},
		{name: "unknown", params: p, rules: tgt(`,"code":"NotFound","error":"after the write","call_ns":7`, "unknown", 3)},
		{name: "exit disagrees", params: p, rules: tgt(`,"error":"x"`, "refused", 3)},
		{name: "ok without a layout id", params: p, rules: tgt(`,"layout_id":"abc"`, "ok", 0)},
		{name: "another volume", params: p, rules: ans("ok", 0, `,"layout_id":"`+eqLayout+`"`)},
		{name: "a field of the wrong type", params: p, rules: tgt(`,"layout_id":"`+eqLayout+`","call_ns":"9","error":"e"`, "ok", 0)},
		{name: "keys in other case", params: p, rules: []eqRule{{"v3ctl", []eqAnswer{{out: `{"Outcome":"ok","VOLUME":"tgt","Layout_ID":"` + eqLayout + `","call_ns":3}`}}}}},
		{name: "garbage", params: p, rules: []eqRule{{"v3ctl", []eqAnswer{{out: "panic: boom", errOut: "trace", code: 2}}}}},
		{name: "no answer", params: p, rules: []eqRule{{"v3ctl", []eqAnswer{{code: 2}}}}},
		{name: "null answer", params: p, rules: []eqRule{{"v3ctl", []eqAnswer{{out: "null", code: 0}}}}},
		{name: "not run", params: p, rules: []eqRule{{"v3ctl", []eqAnswer{{err: broken}}}}},
		{name: "size given", params: map[string]string{"volume": "tgt", "size_bytes": "1048576"}, rules: okRules},
		{name: "size not a number", params: map[string]string{"volume": "tgt", "size_bytes": "big"}, rules: okRules},
		{name: "volume from vars", vars: map[string]string{"volume": "tgt"}, rules: tgt(`,"layout_id":"`+eqLayout+`"`, "ok", 0)},
	}
	cases = append(cases, commonErrorCases(p, tgt(`,"layout_id":"`+eqLayout+`"`, "ok", 0))...)
	checkEquivalent(t, "v3_op_create", v3OpCreateFrozen, cases)
}

func TestDeclaredSnapshotEquivalent(t *testing.T) {
	p := map[string]string{"volume": "v1", "name": "s1"}
	on := func(a eqAnswer) []eqRule { return []eqRule{{"ops chunk snapshot", []eqAnswer{a}}} }
	moved := eqVolumes()
	moved[0].Cur = 1
	cases := []eqCase{
		{name: "ok", params: p, rules: on(eqAnswer{out: `{"name":"s1"}`})},
		{name: "refused", params: p, rules: on(chunkErr("bad_request", "--name is required"))},
		{name: "kept unknown", params: p, rules: on(chunkErr("layout_refused", "too many"))},
		{name: "other code", params: p, rules: on(chunkErr("outcome_unknown", "timeout"))},
		{name: "no error body", params: p, rules: on(eqAnswer{out: "oops", code: 1})},
		{name: "not run", params: p, rules: on(eqAnswer{err: broken})},
		{name: "a name to quote", params: map[string]string{"volume": "v1", "name": "a'b c;$(x)"}, rules: on(eqAnswer{})},
		{name: "name from vars", params: map[string]string{"volume": "v1"}, vars: map[string]string{"name": "fromvar"}, rules: on(eqAnswer{})},
		{name: "no name", params: map[string]string{"volume": "v1"}, rules: on(eqAnswer{})},
		{name: "on the second host", params: p, vols: moved, rules: on(eqAnswer{})},
	}
	cases = append(cases, commonErrorCases(p, on(eqAnswer{}))...)
	checkEquivalent(t, "v3_op_snapshot", v3OpSnapshotFrozen, cases)
}

func TestDeclaredRestoreEquivalent(t *testing.T) {
	p := map[string]string{"volume": "v1", "snapshot": "s1", "target_volume": "tgt"}
	on := func(a eqAnswer) []eqRule { return []eqRule{{"ops chunk restore", []eqAnswer{a}}} }
	cases := []eqCase{
		{name: "ok", params: p, rules: on(eqAnswer{out: `{"volume":"tgt"}`})},
		{name: "refused", params: p, rules: on(chunkErr("target_exists", "exists"))},
		{name: "kept unknown", params: p, rules: on(chunkErr("not_admitted", "bound"))},
		{name: "a wildcard refusal", params: p, rules: on(chunkErr("detach_aborted", "x"))},
		{name: "no error body", params: p, rules: on(eqAnswer{code: 3})},
		{name: "not run", params: p, rules: on(eqAnswer{err: broken})},
		{name: "target not in the spec", params: map[string]string{"volume": "v1", "snapshot": "s1", "target_volume": "nope"}, rules: on(eqAnswer{})},
		{name: "no target", params: map[string]string{"volume": "v1", "snapshot": "s1"}, rules: on(eqAnswer{})},
		{name: "no snapshot", params: map[string]string{"volume": "v1", "target_volume": "tgt"}, rules: on(eqAnswer{})},
		{name: "snapshot and target from vars", params: map[string]string{"volume": "v1"}, vars: map[string]string{"snapshot": "s9", "target_volume": "tgt"}, rules: on(eqAnswer{})},
	}
	cases = append(cases, commonErrorCases(p, on(eqAnswer{}))...)
	checkEquivalent(t, "v3_op_restore", v3OpRestoreFrozen, cases)
}

func TestDeclaredReleaseEquivalent(t *testing.T) {
	p := map[string]string{"volume": "v1", "target_volume": "tgt"}
	on := func(a eqAnswer) []eqRule { return []eqRule{{"ops chunk release", []eqAnswer{a}}} }
	cases := []eqCase{
		{name: "ok", params: p, rules: on(eqAnswer{out: `{}`})},
		{name: "refused", params: p, rules: on(chunkErr("target_live", "live"))},
		{name: "kept unknown", params: p, rules: on(chunkErr("conflict", "lost"))},
		{name: "no error body", params: p, rules: on(eqAnswer{out: "{}", code: 1})},
		{name: "not run", params: p, rules: on(eqAnswer{err: broken})},
		{name: "no target", params: map[string]string{"volume": "v1"}, rules: on(eqAnswer{})},
		{name: "a target that is no spec volume", params: map[string]string{"volume": "v1", "target_volume": "gone"}, rules: on(eqAnswer{})},
	}
	cases = append(cases, commonErrorCases(p, on(eqAnswer{}))...)
	checkEquivalent(t, "v3_op_release", v3OpReleaseFrozen, cases)
}

// Red control of the comparison itself: a declared action that differs from
// its frozen handler in one word of the command, or in one recorded arg, is
// caught.
func TestEquivalenceRedControl(t *testing.T) {
	c := eqCase{name: "ok", params: map[string]string{"volume": "v1", "name": "s1"}, rules: []eqRule{{"ops chunk snapshot", []eqAnswer{{}}}}}
	frozen := runEq(t, v3OpSnapshotFrozen, c)
	a := blockActions.File.Actions
	for i := range a {
		if a[i].Action != "v3_op_snapshot" {
			continue
		}
		w := a[i].Variants[0].Command
		old := w[len(w)-1].Text
		w[len(w)-1].Text = old + "x"
		got := runEq(t, blockActions.Handler("v3_op_snapshot"), c)
		w[len(w)-1].Text = old
		if reflect.DeepEqual(frozen.calls, got.calls) {
			t.Error("a changed command word was not caught")
		}
		args := a[i].Record.Args
		n := args["name"]
		args["name"] = declared.Value{Template: "{{name}}-"}
		got = runEq(t, blockActions.Handler("v3_op_snapshot"), c)
		args["name"] = n
		if reflect.DeepEqual(frozen.records, got.records) {
			t.Error("a changed recorded arg was not caught")
		}
	}
	if got := runEq(t, blockActions.Handler("v3_op_snapshot"), c); !reflect.DeepEqual(frozen, got) {
		t.Errorf("green twin differs: %+v vs %+v", frozen, got)
	}
}
