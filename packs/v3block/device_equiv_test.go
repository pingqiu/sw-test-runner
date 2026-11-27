package v3block

// Equivalence of the declared device actions (v3_op_write, v3_op_read,
// v3_op_flush) with the Go handlers they replaced (frozen_device_test.go).
// Both run on a simulated host over the same scripted node answers (the
// frozen handlers ran their I/O detached too), and the test requires the
// identical commands in the same order, the identical history records (the
// clock aside), return value, log lines and runner state (vars). A poll's
// repeat count is the clock's, so consecutive repeats of one command count
// once. A runner error must be a runner error in both.

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/declared"
)

type devCase struct {
	eqCase
	// stuck never answers a command holding it (a dead device).
	stuck      string
	unkillable bool
}

// squeeze drops consecutive repeats of one command, and of one poll round
// (a run of up to 8 commands repeated at once): how many rounds a poll takes
// within its bound is the clock's.
func squeeze(calls []string) []string {
	out := append([]string(nil), calls...)
	for changed := true; changed; {
		changed = false
		for k := 1; k <= 8 && !changed; k++ {
			for i := 0; i+2*k <= len(out); i++ {
				if slices.Equal(out[i:i+k], out[i+k:i+2*k]) {
					out = append(out[:i+k], out[i+2*k:]...)
					changed = true
					break
				}
			}
		}
	}
	return out
}

func runDev(t *testing.T, h tr.ActionHandlerFunc, c devCase) eqRun {
	t.Helper()
	node := &eqNode{rules: c.rules, n: make([]int, len(c.rules))}
	var inner runner = node
	if c.stuck != "" {
		inner = stuckOn{match: c.stuck, inner: node}
	}
	old := opNode
	opNode = func(*tr.ActionContext, string) (runner, error) {
		return &declared.SimHost{Inner: inner, Unkillable: c.unkillable}, nil
	}
	defer func() { opNode = old }()
	vars := eqBaseVars()
	vols := c.vols
	if vols == nil {
		vols = eqVolumes()
	}
	var r eqRun
	actx := &tr.ActionContext{Vars: vars, Log: func(f string, a ...interface{}) { r.logs = append(r.logs, fmt.Sprintf(f, a...)) }}
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
		// The run's end: abandoned children are recorded as both would.
		if err := declared.Check(context.Background(), nil, actx.History, true); err != nil {
			t.Fatal(err)
		}
		if err := actx.History.Close(); err != nil {
			t.Fatal(err)
		}
		for _, rec := range records(t, p) {
			rec.MonoNs = 0
			r.records = append(r.records, rec)
		}
	}
	r.calls, r.vars = squeeze(node.calls), vars
	return r
}

// reporter is the part of *testing.T the check reports through (a red
// control counts its reports).
type reporter interface {
	Helper()
	Errorf(format string, args ...any)
}

func checkDevEquivalent(t *testing.T, action string, frozen tr.ActionHandlerFunc, cases []devCase) {
	t.Helper()
	checkDevEquivalentTo(t, t, action, frozen, cases)
}

func checkDevEquivalentTo(t *testing.T, rep reporter, action string, frozen tr.ActionHandlerFunc, cases []devCase) {
	t.Helper()
	h := blockActions.Handler(action)
	for _, c := range cases {
		want, got := runDev(t, frozen, c), runDev(t, h, c)
		if (want.err == nil) != (got.err == nil) {
			rep.Errorf("%s %s: error %v, frozen %v", action, c.name, got.err, want.err)
			continue
		}
		for what, pair := range map[string][2]any{
			"return": {want.ret, got.ret}, "records": {want.records, got.records}, "commands": {want.calls, got.calls},
			"vars": {want.vars, got.vars}, "logs": {want.logs, got.logs},
		} {
			if !reflect.DeepEqual(pair[0], pair[1]) {
				w, _ := json.MarshalIndent(pair[0], "", " ")
				g, _ := json.MarshalIndent(pair[1], "", " ")
				rep.Errorf("%s %s: %s differ\nfrozen:   %s\ndeclared: %s", action, c.name, what, w, g)
			}
		}
	}
}

func devCases(cs []eqCase) []devCase {
	out := make([]devCase, len(cs))
	for i, c := range cs {
		out[i] = devCase{eqCase: c}
	}
	return out
}

const scanOne = "ctrl nvme3\nns nvme3c3n1\nend\n"

func genOK(vol, round string, lbas ...int) []eqRule {
	var rs []eqRule
	for _, l := range lbas {
		tag := fmt.Sprintf("%s|%s|%d|", vol, round, l)
		rs = append(rs, eqRule{"'" + tag + "'", []eqAnswer{{out: blockSHAFrozen(tag) + "\n"}}})
	}
	return rs
}

func withWritten(lbas ...string) []*opVolume {
	vs := eqVolumes()
	vs[0].Written = map[string]bool{}
	for _, l := range lbas {
		vs[0].Written[l] = true
	}
	return vs
}

func TestDeclaredWriteEquivalent(t *testing.T) {
	defer func(d func(time.Duration) time.Duration, f time.Duration) {
		blockActions.Deadline, deviceDeadlineFrozen = d, f
	}(blockActions.Deadline, deviceDeadlineFrozen)
	scan := eqRule{"subsysnqn", []eqAnswer{{out: scanOne}}}
	dd := eqRule{"dd if=", []eqAnswer{{}}}
	ok := append(genOK("v1", "r1", 0, 7), scan, dd)
	p := map[string]string{"volume": "v1", "round": "r1", "lbas": "0 7"}
	one := map[string]string{"volume": "v1", "round": "r1", "lbas": "7"}
	with := func(k, v string) map[string]string {
		out := map[string]string{"volume": "v1", "round": "r1", "lbas": "7"}
		out[k] = v
		return out
	}
	cases := devCases([]eqCase{
		{name: "two lbas ok", params: p, rules: ok},
		{name: "unflushed", params: with("flush", "false"), rules: ok},
		{name: "flushed named", params: with("flush", "true"), rules: ok},
		{name: "flush not a bool", params: with("flush", "yes"), rules: ok},
		{name: "round from vars", params: map[string]string{"volume": "v1", "lbas": "7"}, vars: map[string]string{"round": "r1"}, rules: ok},
		{name: "bad round", params: with("round", "a b"), rules: ok},
		{name: "no round", params: map[string]string{"volume": "v1", "lbas": "7"}, rules: ok},
		{name: "lbas written", params: with("lbas", "written"), vols: withWritten("7", "0"), rules: ok},
		{name: "lbas written, none", params: with("lbas", "written"), rules: ok},
		{name: "lbas not numbers", params: with("lbas", "0 x"), rules: ok},
		{name: "no lbas", params: with("lbas", " "), rules: ok},
		{name: "generator mismatch", params: one, rules: []eqRule{{"python3 -c", []eqAnswer{{out: strings.Repeat("0", 64)}}}, scan, dd}},
		{name: "generator mismatch on the second", params: p, rules: append(genOK("v1", "r1", 0), eqRule{"python3 -c", []eqAnswer{{out: "x", code: 1}}}, scan, dd)},
		{name: "dd fails", params: one, rules: append(genOK("v1", "r1", 7), scan, eqRule{"dd if=", []eqAnswer{{errOut: "dd: error writing\nmore", code: 1}}})},
		{name: "dd not run", params: one, rules: append(genOK("v1", "r1", 7), scan, eqRule{"dd if=", []eqAnswer{{err: broken}}})},
		{name: "scan unreadable", params: one, rules: append(genOK("v1", "r1", 7), eqRule{"subsysnqn", []eqAnswer{{errOut: "boom", code: 3}}}, dd)},
		{name: "scan garbage", params: one, rules: append(genOK("v1", "r1", 7), eqRule{"subsysnqn", []eqAnswer{{out: "x\nend\n"}}}, dd)},
		{name: "bad guest connect", params: one, rules: ok, vars: map[string]string{"guest_connect_variant": "nope"}},
		{name: "expected refusal recorded", params: with("expect", "refused"), rules: ok},
	})
	cases = append(cases, devCases(commonErrorCases(one, ok))...)
	checkDevEquivalent(t, "v3_op_write", v3OpWriteFrozen, cases)

	// A dd that never answers: its deadline, with and without a survivor.
	blockActions.Deadline = func(time.Duration) time.Duration { return 150 * time.Millisecond }
	deviceDeadlineFrozen = 150 * time.Millisecond
	stuck := append(genOK("v1", "r1", 0, 7), scan)
	checkDevEquivalent(t, "v3_op_write", v3OpWriteFrozen, []devCase{
		{eqCase: eqCase{name: "dd stuck", params: p, rules: stuck}, stuck: "dd if="},
		{eqCase: eqCase{name: "dd stuck, survives", params: p, rules: stuck}, stuck: "dd if=", unkillable: true},
	})
}

func TestDeclaredReadEquivalent(t *testing.T) {
	defer func(d func(time.Duration) time.Duration, f time.Duration) {
		blockActions.Deadline, deviceDeadlineFrozen = d, f
	}(blockActions.Deadline, deviceDeadlineFrozen)
	sum := strings.Repeat("ab", 32)
	scan := eqRule{"subsysnqn", []eqAnswer{{out: scanOne}}}
	ok := []eqRule{scan, {"skip=", []eqAnswer{{out: sum + "\n"}}}}
	p := map[string]string{"volume": "v1", "lbas": "0 7"}
	one := map[string]string{"volume": "v1", "lbas": "7"}
	cases := devCases([]eqCase{
		{name: "two lbas ok", params: p, rules: ok},
		{name: "lbas from vars", vars: map[string]string{"lbas": "3"}, params: map[string]string{"volume": "v1"}, rules: ok},
		{name: "written", params: map[string]string{"volume": "v1", "lbas": "written"}, vols: withWritten("9", "1"), rules: ok},
		{name: "written, none", params: map[string]string{"volume": "v1", "lbas": "written"}, rules: ok},
		{name: "not a sum", params: one, rules: []eqRule{scan, {"skip=", []eqAnswer{{out: "abc\n"}}}}},
		{name: "exit 1", params: one, rules: []eqRule{scan, {"skip=", []eqAnswer{{out: sum, errOut: "dd: io error", code: 1}}}}},
		{name: "not run", params: one, rules: []eqRule{scan, {"skip=", []eqAnswer{{err: broken}}}}},
		{name: "scan unreadable", params: one, rules: []eqRule{{"subsysnqn", []eqAnswer{{errOut: "boom", code: 3}}}}},
		{name: "two controllers, then one", params: one, rules: []eqRule{{"subsysnqn", []eqAnswer{{out: "ctrl nvme3\nns nvme3c3n1\nctrl nvme4\nns nvme4c4n1\nend\n"}, {out: scanOne}}}, {"skip=", []eqAnswer{{out: sum}}}}},
		{name: "no lbas", params: map[string]string{"volume": "v1", "lbas": ""}, rules: ok},
		{name: "expected refusal recorded", params: map[string]string{"volume": "v1", "lbas": "7", "expect": "refused"}, rules: ok},
	})
	cases = append(cases, devCases(commonErrorCases(one, ok))...)
	checkDevEquivalent(t, "v3_op_read", v3OpReadFrozen, cases)

	blockActions.Deadline = func(time.Duration) time.Duration { return 150 * time.Millisecond }
	deviceDeadlineFrozen = 150 * time.Millisecond
	checkDevEquivalent(t, "v3_op_read", v3OpReadFrozen, []devCase{
		{eqCase: eqCase{name: "first stuck, survives", params: p, rules: ok}, stuck: "skip=0 ", unkillable: true},
	})
}

func TestDeclaredFlushEquivalent(t *testing.T) {
	defer func(d func(time.Duration) time.Duration, f time.Duration) {
		blockActions.Deadline, deviceDeadlineFrozen = d, f
	}(blockActions.Deadline, deviceDeadlineFrozen)
	scan := eqRule{"subsysnqn", []eqAnswer{{out: scanOne}}}
	ok := []eqRule{scan, {"os.fsync", []eqAnswer{{}}}}
	p := map[string]string{"volume": "v1"}
	cases := devCases([]eqCase{
		{name: "ok", params: p, rules: ok},
		{name: "fsync fails", params: p, rules: []eqRule{scan, {"os.fsync", []eqAnswer{{errOut: "OSError: [Errno 5]\nTraceback", code: 1}}}}},
		{name: "not run", params: p, rules: []eqRule{scan, {"os.fsync", []eqAnswer{{err: broken}}}}},
		{name: "scan unreadable", params: p, rules: []eqRule{{"subsysnqn", []eqAnswer{{errOut: "boom", code: 3}}}}},
		{name: "expected refusal recorded", params: map[string]string{"volume": "v1", "expect": "refused"}, rules: ok},
	})
	cases = append(cases, devCases(commonErrorCases(p, ok))...)
	checkDevEquivalent(t, "v3_op_flush", v3OpFlushFrozen, cases)

	blockActions.Deadline = func(time.Duration) time.Duration { return 150 * time.Millisecond }
	deviceDeadlineFrozen = 150 * time.Millisecond
	checkDevEquivalent(t, "v3_op_flush", v3OpFlushFrozen, []devCase{
		{eqCase: eqCase{name: "fsync stuck, survives", params: p, rules: ok}, stuck: "os.fsync", unkillable: true},
	})
}

// A device that never shows up: both wait the declared lookup bound, then
// unknown "no controller of <nqn>" (slow: the 5 s lookup, twice).
func TestDeclaredDeviceAbsentEquivalent(t *testing.T) {
	if testing.Short() {
		t.Skip("waits the 5 s device lookup twice")
	}
	checkDevEquivalent(t, "v3_op_flush", v3OpFlushFrozen, devCases([]eqCase{
		{name: "no controller", params: map[string]string{"volume": "v1"}, rules: []eqRule{{"subsysnqn", []eqAnswer{{out: "end\n"}}}}},
	}))
}

type countReports struct{ n int }

func (c *countReports) Helper()               {}
func (c *countReports) Errorf(string, ...any) { c.n++ }

// Red controls: the check catches a handler that does other I/O (one lba of
// two) and one that leaves other runner state; the green twin passes.
func TestDeviceEquivalenceRedControls(t *testing.T) {
	sum := strings.Repeat("ab", 32)
	ok := []eqRule{{"subsysnqn", []eqAnswer{{out: scanOne}}}, {"skip=", []eqAnswer{{out: sum + "\n"}}}}
	c := []devCase{{eqCase: eqCase{name: "two lbas", params: map[string]string{"volume": "v1", "lbas": "0 7"}, rules: ok}}}
	for name, frozen := range map[string]tr.ActionHandlerFunc{
		"one lba of two": func(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
			act.Params["lbas"] = "7"
			return v3OpReadFrozen(ctx, actx, act)
		},
		"other state": func(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
			actx.Vars["left_behind"] = "x"
			return v3OpReadFrozen(ctx, actx, act)
		},
	} {
		rep := &countReports{}
		checkDevEquivalentTo(t, rep, "v3_op_read", frozen, c)
		if rep.n == 0 {
			t.Errorf("%s: not caught", name)
		}
	}
	rep := &countReports{}
	checkDevEquivalentTo(t, rep, "v3_op_read", v3OpReadFrozen, c)
	if rep.n != 0 {
		t.Errorf("green twin: %d reports", rep.n)
	}
}
