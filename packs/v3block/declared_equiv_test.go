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
	"time"

	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/declared"
	"github.com/pingqiu/sw-test-runner/randomrun"
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

// runEq runs one case; detached says the handler runs its commands
// detached (a declared action), so the node is a simulated host over the
// scripted one, which sees exactly the commands a direct run issues.
func runEq(t *testing.T, h tr.ActionHandlerFunc, c eqCase, detached bool) eqRun {
	t.Helper()
	node := &eqNode{rules: c.rules, n: make([]int, len(c.rules))}
	old := opNode
	opNode = func(*tr.ActionContext, string) (runner, error) {
		if detached {
			return &declared.SimHost{Inner: node}, nil
		}
		return node, nil
	}
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
			rec.MonoNs, rec.FromNs = 0, 0 // the clock; the declared observation also stamps its start
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
	checkEquivalentAs(t, action, frozen, cases, nil)
}

// checkEquivalentAs is checkEquivalent with a deliberate change: expect
// turns the frozen handler's run into the run the declared action must
// give now.
func checkEquivalentAs(t *testing.T, action string, frozen tr.ActionHandlerFunc, cases []eqCase, expect func(eqCase, *eqRun)) {
	t.Helper()
	h := blockActions.Handler(action)
	for _, c := range cases {
		want, got := runEq(t, frozen, c, false), runEq(t, h, c, true)
		if expect != nil {
			expect(c, &want)
		}
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
	// The deliberate change since the frozen handler: the snapshot is a
	// retried action (docs/operator-guide.md:220-225), so its invocation
	// names its request and its attempt, and its log line the attempt.
	checkEquivalentAs(t, "v3_op_snapshot", v3OpSnapshotFrozen, cases, asSnapshotAttempt)
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
	frozen := runEq(t, v3OpSnapshotFrozen, c, false)
	a := blockActions.File.Actions
	for i := range a {
		if a[i].Action != "v3_op_snapshot" {
			continue
		}
		w := a[i].Variants[0].Command
		old := w[len(w)-1].Text
		w[len(w)-1].Text = old + "x"
		got := runEq(t, blockActions.Handler("v3_op_snapshot"), c, true)
		w[len(w)-1].Text = old
		if reflect.DeepEqual(frozen.calls, got.calls) {
			t.Error("a changed command word was not caught")
		}
		args := a[i].Record.Args
		n := args["name"]
		args["name"] = declared.Value{Template: "{{name}}-"}
		got = runEq(t, blockActions.Handler("v3_op_snapshot"), c, true)
		args["name"] = n
		if reflect.DeepEqual(frozen.records, got.records) {
			t.Error("a changed recorded arg was not caught")
		}
	}
	asSnapshotAttempt(c, &frozen) // the snapshot's one deliberate change
	if got := runEq(t, blockActions.Handler("v3_op_snapshot"), c, true); !reflect.DeepEqual(frozen, got) {
		t.Errorf("green twin differs: %+v vs %+v", frozen, got)
	}
}

func TestDeclaredSnapshotDeleteEquivalent(t *testing.T) {
	p := map[string]string{"volume": "v1", "name": "s1"}
	held := func(cur int, start string) []*opVolume {
		vs := eqVolumes()
		vs[0].PID, vs[0].PIDStart, vs[0].Cur = "4242", start, cur
		return vs
	}
	proc := func(out string, code int) eqRule { return eqRule{"/proc/4242", []eqAnswer{{out: out, code: code}}} }
	holder := func(a eqAnswer) []eqRule {
		return []eqRule{proc("start 999\n", 0), {"curl", []eqAnswer{a}}, {"snapshot-delete", []eqAnswer{{}}}}
	}
	store := func(p eqRule, a eqAnswer) []eqRule {
		return []eqRule{p, {"curl", []eqAnswer{{out: "\n200"}}}, {"snapshot-delete", []eqAnswer{a}}}
	}
	refusedBody := `{"code":"layout_refused","message":"has dependents","retryable":false}`
	cases := []eqCase{
		{name: "holder ok", params: p, vols: held(0, "999"), rules: holder(eqAnswer{out: `{"volumeID":"v1","name":"s1","state":"deleted"}` + "\n200"})},
		{name: "holder refused", params: p, vols: held(0, "999"), rules: holder(eqAnswer{out: refusedBody + "\n409"})},
		{name: "holder other code", params: p, vols: held(0, "999"), rules: holder(eqAnswer{out: `{"code":"internal","message":"x"}` + "\n500"})},
		{name: "holder status only", params: p, vols: held(0, "999"), rules: holder(eqAnswer{out: "200"})},
		{name: "holder status then newline", params: p, vols: held(0, "999"), rules: holder(eqAnswer{out: refusedBody + "\n409\n"})},
		{name: "holder no body", params: p, vols: held(0, "999"), rules: holder(eqAnswer{out: "\n502"})},
		{name: "holder not answering", params: p, vols: held(0, "999"), rules: holder(eqAnswer{code: 7})},
		{name: "holder not run", params: p, vols: held(0, "999"), rules: holder(eqAnswer{code: -1, err: broken})},
		{name: "holder on the second host", params: p, vols: held(1, "999"), rules: holder(eqAnswer{out: "{}\n200"})},
		{name: "a name to quote, holder", params: map[string]string{"volume": "v1", "name": `a'b "c"`}, vols: held(0, "999"), rules: holder(eqAnswer{out: "{}\n200"})},
		{name: "pid reused: store", params: p, vols: held(0, "999"), rules: store(proc("start 1000", 0), eqAnswer{})},
		{name: "holder gone: store", params: p, vols: held(0, "999"), rules: store(proc("absent", 0), eqAnswer{})},
		{name: "holder unreadable: store", params: p, vols: held(0, "999"), rules: store(proc("", 3), chunkErr("snapshot_has_dependents", "x"))},
		{name: "no identity: store", params: p, vols: held(0, ""), rules: store(proc("start 999", 0), eqAnswer{})},
		{name: "store refused", params: p, rules: store(proc("absent", 0), chunkErr("conflict", "lost"))},
		{name: "store volume deleting", params: p, rules: store(proc("absent", 0), chunkErr("volume_deleting", "x"))},
		{name: "store other code", params: p, rules: store(proc("absent", 0), chunkErr("internal", "x"))},
		{name: "store no body", params: p, rules: store(proc("absent", 0), eqAnswer{code: 1})},
		{name: "store not run", params: p, rules: store(proc("absent", 0), eqAnswer{err: broken})},
		{name: "name from vars", params: map[string]string{"volume": "v1"}, vars: map[string]string{"name": "s2"}, rules: store(proc("absent", 0), eqAnswer{})},
		{name: "no name", params: map[string]string{"volume": "v1"}, vols: held(0, "999"), rules: holder(eqAnswer{})},
	}
	cases = append(cases, commonErrorCases(p, store(proc("absent", 0), eqAnswer{}))...)
	checkEquivalent(t, "v3_op_snapshot_delete", v3OpSnapshotDeleteFrozen, cases)
	// The cases reach both variants.
	for i, via := range map[int]string{0: "holder", 10: "store"} {
		if r := runEq(t, blockActions.Handler("v3_op_snapshot_delete"), cases[i], true); len(r.records) < 1 || r.records[0].Args["via"] != via {
			t.Errorf("%s: records %v, want via %s", cases[i].name, r.records, via)
		}
	}
}

// asOneAttempt is the deliberate change from the frozen delete's single
// call to the declared delete's one attempt: its invoke carries attempt 1,
// its complete no attempts list, its log line names the attempt.
// asSnapshotAttempt is asOneAttempt for the snapshot, whose record also
// names its request (<volume>-<name>, as its --request-id).
func asSnapshotAttempt(c eqCase, r *eqRun) {
	for i := range r.records {
		if r.records[i].Kind == tr.HistoryInvoke {
			r.records[i].Args["request"] = r.records[i].Volume + "-" + fmt.Sprint(r.records[i].Args["name"])
		}
	}
	asOneAttempt(c, r)
	for i, l := range r.logs {
		if strings.HasPrefix(l, "  v3_op_snapshot(") {
			r.logs[i] = strings.Replace(l, "): ", ") attempt 1: ", 1)
		}
	}
}

func asOneAttempt(_ eqCase, r *eqRun) {
	for i := range r.records {
		switch r.records[i].Kind {
		case tr.HistoryInvoke:
			r.records[i].Args["attempt"] = float64(1)
		case tr.HistoryComplete:
			delete(r.records[i].Result, "attempts")
			if len(r.records[i].Result) == 0 {
				r.records[i].Result = nil
			}
		}
	}
	for i, l := range r.logs {
		if strings.HasPrefix(l, "  v3_op_delete(") {
			r.logs[i] = strings.Replace(l, "): ", ") attempt 1: ", 1)
		}
	}
}

func TestDeclaredDeleteEquivalent(t *testing.T) {
	defer func(d time.Duration, p func(time.Duration) time.Duration) {
		deleteRetryEveryFrozen, blockActions.Pause = d, p
	}(deleteRetryEveryFrozen, blockActions.Pause)
	deleteRetryEveryFrozen = time.Millisecond
	blockActions.Pause = func(time.Duration) time.Duration { return time.Millisecond }

	live := chunkErr("not_admitted", "layout: delete reservation: workspace: deletion refused before persistence: live until T")
	horizon := chunkErr("not_admitted", "layout: a holder is acting on the volume: live until T")
	deleted := eqAnswer{out: `{"volume":"v1","state":"deleted"}`}
	on := func(as ...eqAnswer) []eqRule { return []eqRule{{"volume-delete", as}} }
	p := map[string]string{"volume": "v1"}
	neg := map[string]string{"volume": "v1", "expect": "refused"}
	// The cases where both make one call: the same record, as one attempt.
	cases := []eqCase{
		{name: "ok", params: p, rules: on(deleted)},
		{name: "not run at once", params: p, rules: on(eqAnswer{err: broken})},
		{name: "a kept-unknown code", params: p, rules: on(chunkErr("conflict", "lost"))},
		{name: "no body", params: p, rules: on(eqAnswer{out: "x", code: 1})},
		{name: "negative refused is one call", params: neg, rules: on(live, deleted)},
		{name: "negative unknown is one call", params: neg, rules: on(horizon, deleted)},
		{name: "negative ok", params: neg, rules: on(deleted)},
		{name: "a volume without a layout id", params: map[string]string{"volume": "tgt"}, rules: on(deleted)},
	}
	cases = append(cases, commonErrorCases(p, on(deleted))...)
	checkEquivalentAs(t, "v3_op_delete", v3OpDeleteFrozen, cases, asOneAttempt)
}

// The declared delete reruns ONLY for the answer the page tells the
// operator to run again for (not_admitted with the sentinel, before
// persistence), each attempt its own invoke and complete with the request
// and the attempt number; at most max_attempts; the last attempt stands.
func TestDeclaredDeleteAttempts(t *testing.T) {
	defer func(p func(time.Duration) time.Duration) { blockActions.Pause = p }(blockActions.Pause)
	blockActions.Pause = func(time.Duration) time.Duration { return time.Millisecond }
	live := chunkErr("not_admitted", "layout: delete reservation: workspace: deletion refused before persistence: live until T")
	after := chunkErr("not_admitted", "layout: a holder is acting on the volume: live until T")
	deleted := eqAnswer{out: `{"volume":"v1","state":"deleted"}`}
	on := func(as ...eqAnswer) []eqRule { return []eqRule{{"volume-delete", as}} }
	maxAttempts := blockActions.File.Actions[5].Retry.MaxAttempts
	if blockActions.File.Actions[5].Action != "v3_op_delete" || maxAttempts < 2 {
		t.Fatalf("the delete's retry: %+v", blockActions.File.Actions[5].Retry)
	}
	for name, c := range map[string]struct {
		params   map[string]string
		answers  []eqAnswer
		outcomes []string
	}{
		"refused twice, then deleted":              {map[string]string{"volume": "v1"}, []eqAnswer{live, live, deleted}, []string{"refused", "refused", "ok"}},
		"a not_admitted after the reservation":     {map[string]string{"volume": "v1"}, []eqAnswer{after, deleted}, []string{"unknown"}},
		"refused, then a kept-unknown code":        {map[string]string{"volume": "v1"}, []eqAnswer{live, chunkErr("conflict", "lost")}, []string{"refused", "unknown"}},
		"refused, then not run":                    {map[string]string{"volume": "v1"}, []eqAnswer{live, {err: broken}}, []string{"refused", "unknown"}},
		"refused until the bound":                  {map[string]string{"volume": "v1", "admit_seconds": "600"}, []eqAnswer{live}, nil},
		"a negative with retry_horizon reruns too": {map[string]string{"volume": "v1", "expect": "refused", "retry_horizon": "true"}, []eqAnswer{live, live, chunkErr("snapshot_has_dependents", "layout: a snapshot has dependents")}, []string{"refused", "refused", "refused_effect"}},
	} {
		if c.outcomes == nil {
			for n := 0; n < maxAttempts; n++ {
				c.outcomes = append(c.outcomes, "refused")
			}
		}
		r := runEq(t, reissued("v3_op_delete"), eqCase{params: c.params, rules: on(c.answers...)}, true)
		if r.err != nil || len(r.calls) != len(c.outcomes) || r.ret["value"] != c.outcomes[len(c.outcomes)-1] {
			t.Fatalf("%s: %v, %d calls, %v", name, r.err, len(r.calls), r.ret)
		}
		var got []string
		n := 0
		for _, rec := range r.records {
			switch rec.Kind {
			case tr.HistoryInvoke:
				n++
				if rec.Args["request"] != "delete-v1" || rec.Args["attempt"] != float64(n) {
					t.Fatalf("%s: attempt %d args %v", name, n, rec.Args)
				}
			case tr.HistoryComplete:
				got = append(got, rec.Outcome)
				if _, folded := rec.Result["attempts"]; folded {
					t.Fatalf("%s: attempts folded into a record", name)
				}
			}
		}
		if strings.Join(got, " ") != strings.Join(c.outcomes, " ") {
			t.Fatalf("%s: outcomes %v, want %v", name, got, c.outcomes)
		}
	}
}

func TestDeclaredObserveEquivalent(t *testing.T) {
	head := `{"head":{"Gen":5,"Epoch":2,"Holder":"r1","State":"serving","Snapshots":{"b":{},"a":1}}}`
	ws := eqAnswer{out: strings.Repeat("key: value\n", 15)}
	obs := func(show, wsA, dirs, files eqAnswer) []eqRule {
		return []eqRule{{"ops chunk show", []eqAnswer{show}}, {"ops workspace show", []eqAnswer{wsA}},
			{"namePattern", []eqAnswer{dirs}}, {"python3", []eqAnswer{files}}}
	}
	okDirs, okFiles := eqAnswer{out: `["v1-01234567"]` + "\n"}, eqAnswer{out: `["root/x","pool/1"]`}
	with := func(show eqAnswer) []eqRule { return obs(show, ws, okDirs, okFiles) }
	p := map[string]string{"volume": "v1"}
	cases := []eqCase{
		{name: "all answered", params: p, rules: with(eqAnswer{out: head})},
		{name: "null snapshots", params: p, rules: with(eqAnswer{out: `{"head":{"Gen":1,"Snapshots":null}}`})},
		{name: "no snapshots key, lower-case keys", params: p, rules: with(eqAnswer{out: `{"head":{"gen":1,"epoch":3,"holder":"h","state":"s"}}`})},
		{name: "the largest generation", params: p, rules: with(eqAnswer{out: `{"head":{"Gen":18446744073709551615}}`})},
		{name: "no head", params: p, rules: with(eqAnswer{out: `{"other":1}`})},
		{name: "null head", params: p, rules: with(eqAnswer{out: `{"head":null}`})},
		{name: "garbage", params: p, rules: with(eqAnswer{out: `nope`})},
		{name: "a generation of the wrong type", params: p, rules: with(eqAnswer{out: `{"head":{"Gen":"5"}}`})},
		{name: "snapshots not an object", params: p, rules: with(eqAnswer{out: `{"head":{"Gen":5,"Snapshots":[1]}}`})},
		{name: "an error body", params: p, rules: with(chunkErr("not_found", "no volume"))},
		{name: "a code to quote", params: p, rules: with(eqAnswer{out: `{"code":"a\"b\c"}`, code: 2})},
		{name: "no error body", params: p, rules: with(eqAnswer{out: "boom", code: 1})},
		{name: "show not run", params: p, rules: with(eqAnswer{err: broken})},
		{name: "workspace not run", params: p, rules: obs(eqAnswer{out: head}, eqAnswer{err: broken}, okDirs, okFiles)},
		{name: "workspace refused, empty", params: p, rules: obs(eqAnswer{out: head}, eqAnswer{code: 1}, okDirs, okFiles)},
		{name: "workspace refused, stderr says why", params: p, rules: obs(eqAnswer{out: head},
			eqAnswer{errOut: "ops workspace show: rpc error: code = FailedPrecondition desc = volume v1 is being deleted\nline2\nline3\nline4\nline5\n", code: 1}, okDirs, okFiles)},
		{name: "listings fail", params: p, rules: obs(eqAnswer{out: head}, ws, eqAnswer{errOut: "Traceback\nURLError", code: 1}, eqAnswer{out: "x", errOut: "e"})},
		{name: "listings null and empty", params: p, rules: obs(eqAnswer{out: head}, ws, eqAnswer{out: "null"}, eqAnswer{out: "[]"})},
		{name: "listings not run", params: p, rules: obs(eqAnswer{out: head}, ws, eqAnswer{err: broken}, eqAnswer{err: broken})},
		{name: "no layout id: no file listing", params: map[string]string{"volume": "tgt"}, rules: with(eqAnswer{out: head})},
		{name: "chunk path with trailing slashes", params: p, vars: map[string]string{"chunk_path": "/chunks//"}, rules: with(eqAnswer{out: head})},
		{name: "filer without a port", params: p, vars: map[string]string{"filer": "filer"}, rules: with(eqAnswer{out: head})},
		{name: "filer on a low port", params: p, vars: map[string]string{"filer": "127.0.0.1:9000"}, rules: with(eqAnswer{out: head})},
		{name: "filer port not a number", params: p, vars: map[string]string{"filer": "127.0.0.1:x"}, rules: with(eqAnswer{out: head})},
	}
	cases = append(cases, commonErrorCases(p, with(eqAnswer{out: head}))...)
	// The one deliberate change since the frozen handler: the workspace
	// field also keeps the first 4 lines of the show's stderr.
	checkEquivalentAs(t, "v3_op_observe", v3OpObserveFrozen, cases, func(c eqCase, r *eqRun) {
		for _, rec := range r.records {
			ws, ok := rec.Result["workspace"].(map[string]any)
			if !ok {
				continue
			}
			for _, rule := range c.rules {
				if rule.match == "ops workspace show" {
					ws["stderr"] = toAny(firstLines(rule.answers[0].errOut, 4))
				}
			}
		}
	})
	// The cases reach every field: a full observation has them all.
	r := runEq(t, blockActions.Handler("v3_op_observe"), cases[0], true)
	if len(r.records) != 2 {
		t.Fatalf("records %v", r.records)
	}
	for _, k := range []string{"head", "snapshots", "workspace", "dirs", "files"} {
		if _, ok := r.records[0].Result[k]; !ok {
			t.Errorf("no %s in %v", k, r.records[0].Result)
		}
	}
}

// toAny is a []string as a history record read back holds it.
func toAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, x := range ss {
		out[i] = x
	}
	return out
}

// The delete of a source whose snapshot has a restore target is refused
// with a documented effect. The deliberate changes since the frozen
// handler: one attempt, and it completes refused_effect with the effect
// exactly as outcomes.json states it.
func TestDeclaredDeleteRefusedEffect(t *testing.T) {
	_, eff := outcomes.decide(chunkVolumeDelete, "snapshot_has_dependents", "")
	if eff == nil {
		t.Fatal("the row decides no effect")
	}
	b, _ := json.Marshal(eff.record())
	var asRead map[string]any
	_ = json.Unmarshal(b, &asRead)
	refusal := []eqRule{{"volume-delete", []eqAnswer{chunkErr("snapshot_has_dependents", "layout: a snapshot has dependents")}}}
	cases := []eqCase{
		{name: "dependents after the reservation", params: map[string]string{"volume": "v1"}, rules: refusal},
		{name: "a negative", params: map[string]string{"volume": "v1", "expect": "refused"}, rules: refusal},
	}
	checkEquivalentAs(t, "v3_op_delete", v3OpDeleteFrozen, cases, func(c eqCase, r *eqRun) {
		asOneAttempt(c, r)
		r.ret["value"] = tr.OutcomeRefusedEffect
		for i := range r.records {
			if r.records[i].Kind == tr.HistoryComplete {
				r.records[i].Outcome = tr.OutcomeRefusedEffect
				r.records[i].Result["effect"] = asRead
			}
		}
		for i, l := range r.logs {
			r.logs[i] = strings.Replace(l, ": "+tr.OutcomeUnknown, ": "+tr.OutcomeRefusedEffect, 1)
		}
	})
	r := runEq(t, blockActions.Handler("v3_op_delete"), cases[0], true)
	to, _ := asRead["to"].(map[string]any)
	if r.ret["value"] != tr.OutcomeRefusedEffect || to["exit"] != float64(1) || to["stderr_contains"] != "volume is being deleted" {
		t.Fatalf("%v effect %v", r.ret, asRead)
	}
}

// retry_horizon: a negative delete reruns through the lease-horizon refusal
// before persistence (the sentinel) and records the first other answer;
// every attempt is listed; it stops at admit_seconds; without the param a
// negative is one call.
func TestDeclaredDeleteRetryHorizon(t *testing.T) {
	defer func(p func(time.Duration) time.Duration) { blockActions.Pause = p }(blockActions.Pause)
	blockActions.Pause = func(time.Duration) time.Duration { return 5 * time.Millisecond }
	horizon := chunkErr("not_admitted", "layout: delete reservation: chunkstore: volume delete not admitted: rpc error: code = FailedPrecondition desc = reserve volume deletion: workspace: deletion refused before persistence: workspace delete: live until T")
	deps := chunkErr("snapshot_has_dependents", "layout: a snapshot has dependents")
	neg := func(extra map[string]string) map[string]string {
		p := map[string]string{"volume": "v1", "expect": "refused"}
		for k, v := range extra {
			p[k] = v
		}
		return p
	}
	// The outcome of each attempt, in order (each its own record pair).
	attempts := func(r eqRun) []string {
		var out []string
		for _, rec := range r.records {
			if rec.Kind == tr.HistoryComplete {
				out = append(out, rec.Outcome+" "+fmt.Sprint(rec.Result["detail"]))
			}
		}
		return out
	}
	last := func(r eqRun) tr.HistoryRecord {
		var l tr.HistoryRecord
		for _, rec := range r.records {
			if rec.Kind == tr.HistoryComplete {
				l = rec
			}
		}
		return l
	}
	h := reissued("v3_op_delete")

	r := runEq(t, h, eqCase{params: neg(map[string]string{"retry_horizon": "true"}), rules: []eqRule{{"volume-delete", []eqAnswer{horizon, horizon, deps}}}}, true)
	if len(r.calls) != 3 || len(attempts(r)) != 3 || last(r).Outcome != tr.OutcomeRefusedEffect ||
		!strings.HasPrefix(attempts(r)[0], "refused not_admitted:") || !strings.HasPrefix(attempts(r)[2], "refused_effect snapshot_has_dependents:") {
		t.Fatalf("horizon twice then dependents: %d calls, records %+v", len(r.calls), r.records)
	}

	start := time.Now()
	r = runEq(t, h, eqCase{params: neg(map[string]string{"retry_horizon": "true", "admit_seconds": "1"}), rules: []eqRule{{"volume-delete", []eqAnswer{horizon}}}}, true)
	if len(r.calls) < 2 || last(r).Outcome != tr.OutcomeRefused || len(attempts(r)) != len(r.calls) || time.Since(start) > 10*time.Second {
		t.Fatalf("horizon until admit_seconds: %d calls, records %+v", len(r.calls), r.records)
	}
	if d, _ := last(r).Result["detail"].(string); !strings.Contains(d, "workspace: deletion refused before persistence") {
		t.Fatalf("the horizon refusal is not the one recorded: %q", d)
	}

	for _, p := range []map[string]string{neg(nil), neg(map[string]string{"retry_horizon": "false"})} {
		r = runEq(t, h, eqCase{params: p, rules: []eqRule{{"volume-delete", []eqAnswer{horizon, deps}}}}, true)
		if len(r.calls) != 1 || r.records[1].Outcome != tr.OutcomeRefused {
			t.Fatalf("without retry_horizon (%v): %d calls, %+v", p, len(r.calls), r.records)
		}
	}
	// A negative with retry_horizon does not rerun through a not_admitted
	// that is not the horizon (after the reservation committed).
	after := chunkErr("not_admitted", "layout: a holder is acting on the volume: live until T")
	r = runEq(t, h, eqCase{params: neg(map[string]string{"retry_horizon": "true"}), rules: []eqRule{{"volume-delete", []eqAnswer{after, deps}}}}, true)
	if len(r.calls) != 1 || r.records[1].Outcome != tr.OutcomeUnknown {
		t.Fatalf("other not_admitted: %d calls, %+v", len(r.calls), r.records)
	}
	// A bad value is a runner error, recording nothing.
	r = runEq(t, h, eqCase{params: neg(map[string]string{"retry_horizon": "maybe"}), rules: []eqRule{{"volume-delete", []eqAnswer{deps}}}}, true)
	if r.err == nil || len(r.calls) != 0 {
		t.Fatalf("retry_horizon=maybe: %v, %d calls", r.err, len(r.calls))
	}
}

// The snapshot's page-named answers (docs/operator-guide.md:220-225):
// head_conflict, then an internal "is not in HEAD after its commit". Both
// decode UNKNOWN (outcomes.json: the page does not say what they leave), and
// an unknown is never re-issued: one attempt, recorded with the request and
// attempt 1, and the request ends unknown. Red control of the condition
// that only a refused attempt is re-issued.
func TestDeclaredSnapshotUnknownsAreNotReissued(t *testing.T) {
	defer func(p func(time.Duration) time.Duration) { blockActions.Pause = p }(blockActions.Pause)
	blockActions.Pause = func(time.Duration) time.Duration { return time.Millisecond }
	conflict := chunkErr("head_conflict", "storage: capture CAS not applied: storage: chunk HEAD precondition failed")
	notInHead := chunkErr("internal", `durable: snapshot "s1" of v1 is not in HEAD after its commit`)
	for name, c := range map[string]struct {
		answers  []eqAnswer
		outcomes []string
	}{
		"a conflict, then ok":           {[]eqAnswer{conflict, {}}, []string{"unknown"}},
		"the known defect, then ok":     {[]eqAnswer{notInHead, {}}, []string{"unknown"}},
		"conflicts to the bound":        {[]eqAnswer{conflict}, []string{"unknown"}},
		"another internal is not rerun": {[]eqAnswer{chunkErr("internal", "boom"), {}}, []string{"unknown"}},
		"a layout refusal is not rerun": {[]eqAnswer{chunkErr("layout_refused", "x"), {}}, []string{"unknown"}},
	} {
		r := runEq(t, reissued("v3_op_snapshot"), eqCase{params: map[string]string{"volume": "v1", "name": "s1"}, rules: []eqRule{{"ops chunk snapshot", c.answers}}}, true)
		var got []string
		n := 0
		for _, rec := range r.records {
			switch rec.Kind {
			case tr.HistoryInvoke:
				n++
				if rec.Args["request"] != "v1-s1" || rec.Args["attempt"] != float64(n) {
					t.Fatalf("%s: attempt %d args %v", name, n, rec.Args)
				}
			case tr.HistoryComplete:
				got = append(got, rec.Outcome)
			}
		}
		if r.err != nil || strings.Join(got, " ") != strings.Join(c.outcomes, " ") || r.ret["value"] != c.outcomes[len(c.outcomes)-1] {
			t.Fatalf("%s: %v %v, outcomes %v", name, r.err, r.ret, got)
		}
	}
}

// reissued is action name as the driver re-issues it (randomrun.Attempts
// under the declared rerun), with no observation between attempts: the
// attempts alone, as these tests judge them. The observation after a
// refused attempt is TestDriverObservesAfterEachRefusedAttempt's.
func reissued(name string) tr.ActionHandlerFunc {
	return func(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
		act.Action = name
		out, _, err := randomrun.Attempts(ctx, actx, blockActions.Handler(name), act, &blockModel{}, nil)
		return out, err
	}
}
