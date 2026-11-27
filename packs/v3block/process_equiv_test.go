package v3block

// Equivalence of the declared process actions (v3_op_attach, v3_op_kill,
// v3_op_restart) with the Go handlers they replaced (frozen_process_test.go):
// both run on the same scripted node answers, and the test requires the
// identical commands in the same order, history records (the clock aside),
// return value, log lines and runner state (vars). Two clocks are set
// aside: a log path's launch time (<server>.<unix ns>.log, the same
// function of the clock in both) and a poll's repeat count (consecutive
// repeats of one command count once). The recovery clock (nowFn) is fixed.

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	tr "github.com/pingqiu/sw-test-runner"
)

var launchClock = regexp.MustCompile(`\.[0-9]{16,20}\.log`)

// procView is one run as compared: its parts as JSON, the launch clock
// replaced.
func procView(r eqRun) map[string]string {
	r.calls = squeeze(r.calls)
	out := map[string]string{}
	for what, v := range map[string]any{"return": r.ret, "records": r.records, "commands": r.calls, "vars": r.vars, "logs": r.logs} {
		b, _ := json.MarshalIndent(v, "", " ")
		out[what] = launchClock.ReplaceAllString(string(b), ".<ns>.log")
	}
	return out
}

func checkProcEquivalentTo(t *testing.T, rep reporter, action string, frozen tr.ActionHandlerFunc, cases []eqCase) {
	t.Helper()
	defer func(f func() time.Time) { nowFn = f }(nowFn)
	at := time.Unix(1800000000, 0)
	nowFn = func() time.Time { return at }
	h := blockActions.Handler(action)
	for _, c := range cases {
		want, got := runEq(t, frozen, c, false), runEq(t, h, c, false)
		if (want.err == nil) != (got.err == nil) {
			rep.Errorf("%s %s: error %v, frozen %v", action, c.name, got.err, want.err)
			continue
		}
		if want.err != nil && len(got.records) > 1 {
			rep.Errorf("%s %s: a runner error recorded %v", action, c.name, got.records)
		}
		if want.err == nil && len(want.records) < 2 {
			rep.Errorf("%s %s: the case recorded no operation", action, c.name)
		}
		w, g := procView(want), procView(got)
		for _, what := range []string{"return", "records", "commands", "vars", "logs"} {
			if w[what] != g[what] {
				rep.Errorf("%s %s: %s differ\nfrozen:   %s\ndeclared: %s", action, c.name, what, w[what], g[what])
			}
		}
	}
}

func checkProcEquivalent(t *testing.T, action string, frozen tr.ActionHandlerFunc, cases []eqCase) {
	t.Helper()
	checkProcEquivalentTo(t, t, action, frozen, cases)
}

// The holder of v1 at host 0 (ports 20010..20013) as the scripted node sees
// it: pid 5555 started 200, listening on its status and NVMe ports.
const ownListeners = "LISTEN 0 4096 127.0.0.1:20012 0.0.0.0:* users:((\"blockvolume\",pid=5555,fd=9))\nLISTEN 0 4096 127.0.0.1:20013 0.0.0.0:* users:((\"blockvolume\",pid=5555,fd=10))"

func statusOf(vol string, epoch int, healthy bool) eqAnswer {
	b, _ := json.Marshal(map[string]any{"VolumeID": vol, "Epoch": epoch, "Healthy": healthy, "AuthorityRole": "primary", "FrontendPrimaryReady": true})
	return eqAnswer{out: string(b)}
}

// attachRules is a good attach of v1, with the named rule's answers
// replaced (a rule not there is put first).
func attachRules(over map[string][]eqAnswer) []eqRule {
	base := []eqRule{
		{"v3ctl", []eqAnswer{{out: `{"outcome":"ok","volume":"tgt","layout_id":"` + eqLayout + `","store_ready":true,"call_ns":1}`}}},
		{"ss -Hltnp", []eqAnswer{{}, {out: ownListeners}}},
		{"mkdir -p", []eqAnswer{{}}},
		{"setsid", []eqAnswer{{out: "5555\n"}}},
		{"/proc/5555", []eqAnswer{{out: "start 200\n"}}},
		{"/own.sh record", []eqAnswer{{}}},
		{"/status?volume=v1", []eqAnswer{statusOf("v1", 1, true)}},
		{"/status?volume=tgt", []eqAnswer{statusOf("tgt", 1, true)}},
		{"nvme-fabrics", []eqAnswer{{}}},
		{"subsysnqn", []eqAnswer{{out: scanOne}}},
	}
	var extra []eqRule
	for k, as := range over {
		found := false
		for i := range base {
			if base[i].match == k {
				base[i].answers, found = as, true
			}
		}
		if !found {
			extra = append(extra, eqRule{k, as})
		}
	}
	return append(extra, base...)
}

func TestDeclaredAttachEquivalent(t *testing.T) {
	p := map[string]string{"volume": "v1"}
	with := func(kv ...string) map[string]string {
		out := map[string]string{"volume": "v1"}
		for i := 0; i+1 < len(kv); i += 2 {
			out[kv[i]] = kv[i+1]
		}
		return out
	}
	tgtListeners := strings.NewReplacer("20012", "20022", "20013", "20023").Replace(ownListeners)
	own := map[string]string{"own_script": "/own.sh", "owner": "tok"}
	cases := []eqCase{
		{name: "ok", params: p, rules: attachRules(nil)},
		{name: "ok, layout resolved first", params: map[string]string{"volume": "tgt"}, rules: attachRules(map[string][]eqAnswer{"ss -Hltnp": {{}, {out: tgtListeners}}})},
		{name: "resolve refused", params: map[string]string{"volume": "tgt"}, rules: attachRules(map[string][]eqAnswer{"v3ctl": {{out: `{"outcome":"refused","volume":"tgt","error":"not created\nyet"}`, code: 1}}})},
		{name: "resolve garbage", params: map[string]string{"volume": "tgt"}, rules: attachRules(map[string][]eqAnswer{"v3ctl": {{out: "panic", code: 2}}})},
		{name: "resolve not run", params: map[string]string{"volume": "tgt"}, rules: attachRules(map[string][]eqAnswer{"v3ctl": {{err: broken}}})},
		{name: "an endpoint is held", params: p, rules: attachRules(map[string][]eqAnswer{"ss -Hltnp": {{out: "LISTEN 0 4096 127.0.0.1:20011 0.0.0.0:* users:((\"x\",pid=7,fd=3),(\"y\",pid=8,fd=4))"}}})},
		{name: "an endpoint is held by no one ss names", params: p, rules: attachRules(map[string][]eqAnswer{"ss -Hltnp": {{out: "LISTEN 0 4096 [::]:20010 [::]:*"}}})},
		{name: "ss fails", params: p, rules: attachRules(map[string][]eqAnswer{"ss -Hltnp": {{errOut: "ss: no\nmore", code: 1}}})},
		{name: "mkdir fails", params: p, rules: attachRules(map[string][]eqAnswer{"mkdir -p": {{errOut: "denied", code: 1}}})},
		{name: "launch fails", params: p, rules: attachRules(map[string][]eqAnswer{"setsid": {{errOut: "no pid file x\nmore", code: 1}}})},
		{name: "launch answers no pid", params: p, rules: attachRules(map[string][]eqAnswer{"setsid": {{out: "pid?"}}})},
		{name: "exits at once", params: p, rules: attachRules(map[string][]eqAnswer{"/proc/5555": {{out: "absent\n"}}})},
		{name: "identity unreadable", params: p, rules: attachRules(map[string][]eqAnswer{"/proc/5555": {{errOut: "x", code: 3}}})},
		{name: "recorded as owned", params: p, vars: own, rules: attachRules(nil)},
		{name: "ownership not recorded", params: p, vars: own, rules: attachRules(map[string][]eqAnswer{"/own.sh record": {{errOut: "ledger busy\nx", code: 1}}})},
		{name: "exits before serving", params: p, rules: attachRules(map[string][]eqAnswer{"/proc/5555": {{out: "start 200\n"}, {out: "absent\n"}}})},
		{name: "not healthy, bound 0", params: with("ready_seconds", "0"), rules: attachRules(map[string][]eqAnswer{"/status?volume=v1": {statusOf("v1", 1, false)}})},
		{name: "another volume answers, bound 0", params: with("ready_seconds", "0"), rules: attachRules(map[string][]eqAnswer{"/status?volume=v1": {statusOf("v9", 1, true)}})},
		{name: "status not json, bound 0", params: with("ready_seconds", "0"), rules: attachRules(map[string][]eqAnswer{"/status?volume=v1": {{out: "<html>"}}})},
		{name: "status a string epoch, bound 0", params: with("ready_seconds", "0"), rules: attachRules(map[string][]eqAnswer{"/status?volume=v1": {{out: `{"VolumeID":"v1","Epoch":"1","Healthy":true,"AuthorityRole":"primary","FrontendPrimaryReady":true}`}}})},
		{name: "status keys in another case", params: p, rules: attachRules(map[string][]eqAnswer{"/status?volume=v1": {{out: `{"volumeid":"v1","epoch":2,"healthy":true,"authorityRole":"primary","FRONTENDPRIMARYREADY":true}`}}})},
		{name: "listeners another's, bound 0", params: with("ready_seconds", "0"), rules: attachRules(map[string][]eqAnswer{"ss -Hltnp": {{}, {out: strings.ReplaceAll(ownListeners, "5555", "777")}}})},
		{name: "ready on the second look", params: with("ready_seconds", "3"), rules: attachRules(map[string][]eqAnswer{"/status?volume=v1": {statusOf("v1", 1, false), statusOf("v1", 2, true)}})},
		{name: "connect fails", params: p, rules: attachRules(map[string][]eqAnswer{"nvme-fabrics": {{errOut: "invalid\nx", code: 1}}})},
		{name: "device scan unreadable", params: p, rules: attachRules(map[string][]eqAnswer{"subsysnqn": {{errOut: "boom", code: 3}}})},
		{name: "host 1", params: with("host", "1"), rules: attachRules(map[string][]eqAnswer{"ss -Hltnp": {{}, {out: strings.NewReplacer("20012", "20016", "20013", "20017").Replace(ownListeners)}}})},
		{name: "host out of range", params: with("host", "2"), rules: attachRules(nil)},
		{name: "host not a number", params: with("host", "x"), rules: attachRules(nil)},
		{name: "lock directories", params: p, vars: map[string]string{"lock_dirs_test_only": "true"}, rules: attachRules(nil)},
		{name: "a declared guest connect variant", params: p, vars: map[string]string{"guest_connect_variant": "host_connect_settings"}, rules: attachRules(nil)},
		{name: "an undeclared guest connect", params: p, vars: map[string]string{"guest_connect_variant": "nope"}, rules: attachRules(nil)},
		{name: "blocks given", params: with("blocks", "64"), rules: attachRules(nil)},
		{name: "blocks not a number", params: with("blocks", "many"), rules: attachRules(nil)},
		{name: "expected refusal recorded", params: with("expect", "refused"), rules: attachRules(nil)},
	}
	// A recorded epoch: ready only above it.
	at3 := eqVolumes()
	at3[0].Epoch, at3[0].PID, at3[0].PIDStart = 3, "4242", "100"
	cases = append(cases,
		eqCase{name: "at the recorded epoch, bound 0", params: with("ready_seconds", "0"), vols: at3, rules: attachRules(map[string][]eqAnswer{"/status?volume=v1": {statusOf("v1", 3, true)}})},
		eqCase{name: "above the recorded epoch", params: p, vols: at3, rules: attachRules(map[string][]eqAnswer{"/status?volume=v1": {statusOf("v1", 4, true)}})},
	)
	cases = append(cases, commonErrorCases(p, attachRules(nil))...)
	checkProcEquivalent(t, "v3_op_attach", v3OpAttachFrozen, cases)
}

// faultVols: v1's holder runs as pid 4242 (start 100) at epoch 3; with
// killed, the runner killed it (its command line recorded).
func faultVols(killed bool) []*opVolume {
	vs := eqVolumes()
	vs[0].PID, vs[0].PIDStart, vs[0].Epoch = "4242", "100", 3
	vs[0].Extra = map[string]string{"intent": intentHeld}
	if killed {
		b, _ := json.Marshal(holderArgv)
		vs[0].Extra["killed_pid"], vs[0].Extra["argv"] = "4242", string(b)
	}
	vs[1].PID, vs[1].PIDStart = "4343", "300"
	return vs
}

func faultVars(t *testing.T, extra map[string]string) map[string]string {
	pol, err := parseUnit(testUnit)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(pol)
	out := map[string]string{specOrderVar: "v1 tgt", unitPolicyVar: string(b)}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func TestDeclaredKillEquivalent(t *testing.T) {
	vars := faultVars(t, nil)
	kill := func(name string, rules []eqRule, extra ...string) eqCase {
		p := map[string]string{"process": name, "kill_seconds": "1"}
		for i := 0; i+1 < len(extra); i += 2 {
			p[extra[i]] = extra[i+1]
		}
		return eqCase{name: name, params: p, vars: vars, vols: faultVols(false), rules: rules}
	}
	sent := eqAnswer{out: "sent " + cmdline(holderArgv) + "\n"}
	named := func(c eqCase, n string) eqCase { c.name = n; return c }
	cases := []eqCase{
		named(kill("blockvolume:0", []eqRule{{"kill -KILL", []eqAnswer{sent}}, {"/proc/4242", []eqAnswer{{out: "absent\n"}}}}), "holder ok"),
		named(kill("blockvolume:1", []eqRule{{"kill -KILL", []eqAnswer{sent}}, {"/proc/4343", []eqAnswer{{out: "absent\n"}}}}), "second holder ok"),
		named(kill("blockvolume:0", []eqRule{{"kill -KILL", []eqAnswer{sent}}, {"/proc/4242", []eqAnswer{{out: "start 100\n"}}}}, "kill_seconds", "0"), "still running"),
		named(kill("blockvolume:0", []eqRule{{"kill -KILL", []eqAnswer{sent}}, {"/proc/4242", []eqAnswer{{errOut: "x", code: 3}}}}, "kill_seconds", "0"), "unreadable after"),
		named(kill("blockvolume:0", []eqRule{{"kill -KILL", []eqAnswer{sent}}, {"/proc/4242", []eqAnswer{{out: "start 999\n"}}}}), "recycled pid"),
		named(kill("blockvolume:0", []eqRule{{"kill -KILL", []eqAnswer{{out: "gone\n"}}}}), "gone"),
		named(kill("blockvolume:0", []eqRule{{"kill -KILL", []eqAnswer{{errOut: "denied", code: 3}}}}), "identity unreadable"),
		named(kill("blockvolume:0", []eqRule{{"kill -KILL", []eqAnswer{{errOut: "no perm\nx", code: 4}}}}), "kill fails"),
		named(kill("blockvolume:0", []eqRule{{"kill -KILL", []eqAnswer{{out: "what"}}}}), "odd answer"),
		named(kill("blockvolume:0", []eqRule{{"kill -KILL", []eqAnswer{{err: broken}}}}), "not run"),
		named(kill("blockvolume:0", []eqRule{{"kill -KILL", []eqAnswer{{out: "sent !!"}}}, {"/proc/4242", []eqAnswer{{out: "absent\n"}}}}), "argv unreadable"),
		named(kill("blockmaster", []eqRule{{"cat /w/owned", []eqAnswer{{out: masterLedger}}}, {"kill -KILL", []eqAnswer{{out: "sent " + cmdline(masterArgv)}}}, {"/proc/77", []eqAnswer{{out: "absent\n"}}}}), "master by the ledger"),
		named(kill("blockmaster", []eqRule{{"cat /w/owned", []eqAnswer{{errOut: "no such file", code: 1}}}}), "ledger unreadable"),
		named(kill("blockmaster", []eqRule{{"cat /w/owned", []eqAnswer{{out: "77 blockmaster\n"}}}}), "ledger malformed"),
		named(kill("blockmaster", []eqRule{{"cat /w/owned", []eqAnswer{{out: "11 50 weed-master\n"}}}}), "no master in the ledger"),
		named(kill("blockvolume:2", nil), "no such index"),
		named(kill("nfs", nil), "unknown name"),
		named(kill("", nil), "no name"),
		named(kill("blockvolume:-0", nil), "signed index"),
		named(kill("blockvolume:0", []eqRule{{"kill -KILL", []eqAnswer{sent}}, {"/proc/4242", []eqAnswer{{out: "absent\n"}}}}, "expect", "refused"), "expected refusal recorded"),
		named(kill("blockvolume:0", nil, "expect", "ok"), "bad expect"),
	}
	noHolder := kill("blockvolume:0", nil)
	noHolder.name, noHolder.vols = "no holder recorded", eqVolumes()
	noStart := kill("blockvolume:0", nil)
	noStart.name = "no start time recorded"
	noStart.vols = faultVols(false)
	noStart.vols[0].PIDStart = ""
	master := kill("blockmaster", []eqRule{{"kill -KILL", []eqAnswer{{out: "sent " + cmdline(masterArgv)}}}, {"/proc/77", []eqAnswer{{out: "absent\n"}}}})
	master.name = "master from its record"
	b, _ := json.Marshal(faultProc{Name: "blockmaster", PID: "77", Start: "700"})
	master.vars = faultVars(t, map[string]string{masterVar: string(b)})
	cases = append(cases, noHolder, noStart, master)
	cases = append(cases, eqCase{name: "no history", params: map[string]string{"process": "blockvolume:0"}, vars: vars, vols: faultVols(false), noHistory: true},
		eqCase{name: "missing configuration", params: map[string]string{"process": "blockvolume:0"}, vars: faultVars(t, map[string]string{"filer": ""}), vols: faultVols(false)})
	checkProcEquivalent(t, "v3_op_kill", v3OpKillFrozen, cases)
}

func TestDeclaredRestartEquivalent(t *testing.T) {
	vars := faultVars(t, nil)
	own := faultVars(t, map[string]string{"own_script": "/own.sh", "owner": "tok"})
	restartRules := func(over map[string][]eqAnswer) []eqRule {
		base := []eqRule{
			{"/proc/4242", []eqAnswer{{out: "absent\n"}}},
			{"ss -Hltnp", []eqAnswer{{}, {out: ownListeners}}},
			{"mkdir -p", []eqAnswer{{}}},
			{"setsid", []eqAnswer{{out: "5555\n"}}},
			{"/proc/5555", []eqAnswer{{out: "start 200\n"}}},
			{"/own.sh record", []eqAnswer{{}}},
			{"/status?volume=v1", []eqAnswer{statusOf("v1", 3, true)}},
			{"nvme-fabrics", []eqAnswer{{}}},
			{"subsysnqn", []eqAnswer{{out: scanOne}}},
		}
		for k, as := range over {
			for i := range base {
				if base[i].match == k {
					base[i].answers = as
				}
			}
		}
		return base
	}
	c := func(name string, rules []eqRule, killed bool, extra ...string) eqCase {
		p := map[string]string{"process": "blockvolume:0", "ready_seconds": "2"}
		for i := 0; i+1 < len(extra); i += 2 {
			p[extra[i]] = extra[i+1]
		}
		return eqCase{name: name, params: p, vars: vars, vols: faultVols(killed), rules: rules}
	}
	withOwn := c("recorded as owned", restartRules(nil), true)
	withOwn.vars = own
	cases := []eqCase{
		c("the kernel kept its controller", restartRules(nil), true),
		c("no controller left: connect", restartRules(map[string][]eqAnswer{"subsysnqn": {{out: "end\n"}, {out: scanOne}}}), true),
		c("scan unreadable", restartRules(map[string][]eqAnswer{"subsysnqn": {{errOut: "boom", code: 3}}}), true),
		c("connect fails", restartRules(map[string][]eqAnswer{"subsysnqn": {{out: "end\n"}}, "nvme-fabrics": {{errOut: "bad", code: 1}}}), true),
		c("an incomplete controller: no connect, then the device", restartRules(map[string][]eqAnswer{"subsysnqn": {{out: "ctrl nvme3\nend\n"}, {out: scanOne}}}), true),
		c("below the recorded epoch, bound 0", restartRules(map[string][]eqAnswer{"/status?volume=v1": {statusOf("v1", 2, true)}}), true, "ready_seconds", "0"),
		c("above the recorded epoch", restartRules(map[string][]eqAnswer{"/status?volume=v1": {statusOf("v1", 5, true)}}), true),
		c("not healthy, bound 0", restartRules(map[string][]eqAnswer{"/status?volume=v1": {statusOf("v1", 3, false)}}), true, "ready_seconds", "0"),
		c("exits at once", restartRules(map[string][]eqAnswer{"/proc/5555": {{out: "absent\n"}}}), true),
		c("an endpoint is held", restartRules(map[string][]eqAnswer{"ss -Hltnp": {{out: ownListeners}}}), true),
		c("launch fails", restartRules(map[string][]eqAnswer{"setsid": {{errOut: "no pid file", code: 1}}}), true),
		c("still running", restartRules(map[string][]eqAnswer{"/proc/4242": {{out: "start 100\n"}}}), true),
		c("state unreadable", restartRules(map[string][]eqAnswer{"/proc/4242": {{errOut: "x", code: 3}}}), true),
		c("never killed", restartRules(nil), false),
		c("unknown name", nil, true, "process", "nfs"),
		c("no such index", nil, true, "process", "blockvolume:7"),
		c("expected refusal recorded", restartRules(nil), true, "expect", "refused"),
		withOwn,
	}
	noArgv := c("command line unknown", restartRules(nil), true)
	noArgv.vols[0].Extra["argv"] = "[]"
	stopped := c("killed, then stopped", restartRules(nil), true)
	stopped.vols[0].PID, stopped.vols[0].PIDStart = "", ""
	// The blockmaster: handed to its Go handler, which the frozen handler ran
	// inline (the spawn, the ledger, a master answer, then the unit).
	mk, _ := json.Marshal(faultProc{Name: "blockmaster", PID: "77", Start: "700", KilledPID: "77", Argv: masterArgv})
	masterRules := []eqRule{
		{"/proc/77", []eqAnswer{{out: "absent\n"}}},
		{"cat /w/owned", []eqAnswer{{out: masterLedger}, {out: masterLedger + "88 800 blockmaster-restart1\n"}}},
		{"/own.sh spawn", []eqAnswer{{out: "88\n"}}},
		{"/proc/88", []eqAnswer{{out: "start 800\n"}}},
		{"v3ctl", []eqAnswer{{out: `{"outcome":"refused","volume":"v1","error":"not created"}`, code: 1}}},
	}
	master := eqCase{name: "master", params: map[string]string{"process": "blockmaster", "ready_seconds": "2"}, vars: faultVars(t, map[string]string{masterVar: string(mk), "own_script": "/own.sh", "owner": "tok"}), vols: faultVols(false), rules: masterRules}
	masterNotKilled := master
	masterNotKilled.name = "master never killed"
	mn, _ := json.Marshal(faultProc{Name: "blockmaster", PID: "77", Start: "700"})
	masterNotKilled.vars = faultVars(t, map[string]string{masterVar: string(mn)})
	cases = append(cases, noArgv, stopped, master, masterNotKilled,
		eqCase{name: "no history", params: map[string]string{"process": "blockvolume:0"}, vars: vars, vols: faultVols(true), noHistory: true},
		eqCase{name: "master, no history", params: map[string]string{"process": "blockmaster"}, vars: vars, vols: faultVols(true), noHistory: true})
	checkProcEquivalent(t, "v3_op_restart", v3OpRestartFrozen, cases)
}

// Red controls: the check catches an attach that does not record the epoch
// and a kill that does not record the killed process; the green twin
// passes.
func TestProcessEquivalenceRedControls(t *testing.T) {
	for name, c := range map[string]struct {
		action string
		frozen tr.ActionHandlerFunc
		cs     []eqCase
	}{
		"attach that does not record the epoch it saw": {"v3_op_attach", func(ctx2 context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
			out, err := v3OpAttachFrozen(ctx2, actx, act)
			v, _ := loadVolume(actx, "v1")
			v.Epoch = 0
			saveVolume(actx, v)
			return out, err
		}, []eqCase{{name: "ok", params: map[string]string{"volume": "v1"}, rules: attachRules(nil)}}},
		"kill that forgets the killed process": {"v3_op_kill", func(ctx2 context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
			out, err := v3OpKillFrozen(ctx2, actx, act)
			v, _ := loadVolume(actx, "v1")
			delete(v.Extra, "killed_pid")
			saveVolume(actx, v)
			return out, err
		}, []eqCase{{name: "ok", params: map[string]string{"process": "blockvolume:0"}, vars: faultVars(t, nil), vols: faultVols(false),
			rules: []eqRule{{"kill -KILL", []eqAnswer{{out: "sent " + cmdline(holderArgv)}}}, {"/proc/4242", []eqAnswer{{out: "absent\n"}}}}}}},
	} {
		rep := &countReports{}
		checkProcEquivalentTo(t, rep, c.action, c.frozen, c.cs)
		if rep.n == 0 {
			t.Errorf("%s: not caught", name)
		}
		green := map[string]tr.ActionHandlerFunc{"v3_op_attach": v3OpAttachFrozen, "v3_op_kill": v3OpKillFrozen}[c.action]
		rep = &countReports{}
		checkProcEquivalentTo(t, rep, c.action, green, c.cs)
		if rep.n != 0 {
			t.Errorf("%s: green twin: %d reports", name, rep.n)
		}
	}
}
