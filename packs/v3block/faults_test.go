package v3block

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/randomrun"
)

// Unit tests of the process faults against a fake node. Each action has a red
// control: a product (or process) behaviour that a wrong action would report
// as ok, and that the action must not.

var holderArgv = []string{"/w/bin/blockvolume", "--master", "127.0.0.1:21003", "--volume-id", "v1", "--status-addr", "127.0.0.1:21012"}
var masterArgv = []string{"/w/bin/blockmaster", "--listen", "127.0.0.1:21003", "--authority-store", "/w/master/authority"}

func cmdline(argv []string) string {
	return base64.StdEncoding.EncodeToString([]byte(strings.Join(argv, "\x00") + "\x00"))
}

func statusAt(epoch uint64, healthy bool) string {
	return fmt.Sprintf(`{"VolumeID":"v1","Epoch":%d,"Healthy":%v,"AuthorityRole":"primary","FrontendPrimaryReady":true}`, epoch, healthy)
}

const masterLedger = "11 50 weed-master\n77 700 blockmaster\n4242 100 blockvolume-a-v1\n"

// faultSetup: one volume v1 (blockvolume:0) whose holder runs as pid 4242
// (start 100) at epoch 3, and a run configuration with a ledger at /w/owned.
func faultSetup(t *testing.T) (*tr.ActionContext, string, opEnv) {
	t.Helper()
	actx, p := genRunCtx(t)
	v, _, e := testVolume()
	v.PID, v.PIDStart, v.Epoch = "4242", "100", 3
	v.Extra = map[string]string{"intent": intentHeld}
	saveVolume(actx, v)
	actx.Vars[specOrderVar] = "v1"
	e.master, e.work, e.ownScript, e.owner, e.client = "127.0.0.1:21003", "/w", "/own.sh", "tok", "c1"
	pol, err := parseUnit(testUnit)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(pol)
	actx.Vars[unitPolicyVar] = string(b)
	return actx, p, e
}

// testUnit has the [Service] lines of the product's shipped holder unit that
// the runner reads (a fixture: at run time the policy is read from the
// product tree, never from here).
const testUnit = "[Unit]\nStartLimitBurst=10\n\n[Service]\nType=simple\nExecStart=/usr/local/bin/blockvolume \\\n  --master=${SWB_MASTER_ADDR} \\\n  --restart-note=Restart=always \\\n  $SWB_VOLUME_EXTRA_ARGS\n# blockvolume drains and closes on SIGTERM\nKillSignal=SIGTERM\nTimeoutStopSec=30s\nRestart=on-failure\nRestartPreventExitStatus=78\nRestartSec=5s\n\n[Install]\nWantedBy=multi-user.target\n"

// closeRecords closes the history and returns its records but the end record.
func closeRecords(t *testing.T, actx *tr.ActionContext, p string) []tr.HistoryRecord {
	t.Helper()
	if err := actx.History.Close(); err != nil {
		t.Fatal(err)
	}
	var out []tr.HistoryRecord
	for _, r := range records(t, p) {
		// The end record, and the run's guest_connect record (the connect
		// read-back, tested on its own), are set aside.
		if r.Kind != tr.HistoryEnd && r.Verb != "guest_connect" {
			out = append(out, r)
		}
	}
	return out
}

func vol1(t *testing.T, actx *tr.ActionContext) *opVolume {
	t.Helper()
	v, err := loadVolume(actx, "v1")
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// killed puts v1 in the state a successful kill leaves.
func killed(t *testing.T, actx *tr.ActionContext) {
	v := vol1(t, actx)
	b, _ := json.Marshal(holderArgv)
	v.Extra["killed_pid"], v.Extra["argv"] = "4242", string(b)
	saveVolume(actx, v)
}

func TestKillHolderRecordsAndConfirmsTheExit(t *testing.T) {
	actx, p, e := faultSetup(t)
	f := (&fakeNode{}).
		on("kill -KILL", ok("sent "+cmdline(holderArgv)+"\n")).
		on("/proc/4242", ok("absent\n"))
	out, err := e.kill(context.Background(), actx, f, "blockvolume:0", time.Second)
	if err != nil || out["value"] != tr.OutcomeOK {
		t.Fatalf("%v %v", out, err)
	}
	v := vol1(t, actx)
	var argv []string
	_ = json.Unmarshal([]byte(v.Extra["argv"]), &argv)
	if v.Extra["killed_pid"] != "4242" || !slices.Equal(argv, holderArgv) || v.PID != "4242" {
		t.Errorf("state after the kill: %+v", v)
	}
	recs := closeRecords(t, actx, p)
	if len(recs) != 2 || recs[0].Kind != tr.HistoryInvoke || recs[1].Outcome != tr.OutcomeOK {
		t.Fatalf("records %+v", recs)
	}
	in := recs[0]
	if in.Verb != "kill" || in.Op != "Env.Kill" || in.Volume != "" || in.Args["process"] != "blockvolume:0" || in.Args["pid"] != float64(4242) || in.Args["signal"] != "KILL" {
		t.Errorf("invoke %+v", in)
	}
	// The identity check and the signal are one shell, by pid and start time.
	if !f.issued("kill -KILL 4242") || !f.issued(`= 100 ]`) {
		t.Errorf("kill not by recorded identity: %v", f.calls)
	}
}

// Red control: a kill whose signal went out but whose process still runs (or
// whose state cannot be read) is unknown, never ok.
func TestKillIsUnknownWhileThePIDLives(t *testing.T) {
	for name, after := range map[string]fakeAnswer{
		"still running": ok("start 100\n"),
		"unreadable":    {errOut: "boom", code: 3},
	} {
		t.Run(name, func(t *testing.T) {
			actx, _, e := faultSetup(t)
			f := (&fakeNode{}).on("kill -KILL", ok("sent "+cmdline(holderArgv))).on("/proc/4242", after)
			out, err := e.kill(context.Background(), actx, f, "blockvolume:0", 600*time.Millisecond)
			if err != nil || out["value"] != tr.OutcomeUnknown {
				t.Fatalf("%v %v", out, err)
			}
			// The signal went out: the process is the killed one, a restart may follow.
			if vol1(t, actx).Extra["killed_pid"] != "4242" {
				t.Error("a sent kill was not recorded in the state")
			}
		})
	}
	// A recycled pid after the kill (another start time) is the killed process gone.
	actx, _, e := faultSetup(t)
	f := (&fakeNode{}).on("kill -KILL", ok("sent "+cmdline(holderArgv))).on("/proc/4242", ok("start 999\n"))
	if out, _ := e.kill(context.Background(), actx, f, "blockvolume:0", time.Second); out["value"] != tr.OutcomeOK {
		t.Errorf("a recycled pid: %v", out)
	}
}

// Refused changes nothing: a process that is not running, an identity that
// cannot be read, and a name the runner does not know.
func TestKillRefusesAndChangesNothing(t *testing.T) {
	for name, a := range map[string]fakeAnswer{
		"not running, or another process holds the pid": ok("gone\n"),
		"identity unreadable":                           {errOut: "cat: denied", code: 3},
	} {
		t.Run(name, func(t *testing.T) {
			actx, p, e := faultSetup(t)
			f := (&fakeNode{}).on("kill -KILL", a)
			out, _ := e.kill(context.Background(), actx, f, "blockvolume:0", time.Second)
			if out["value"] != tr.OutcomeRefused || vol1(t, actx).Extra["killed_pid"] != "" {
				t.Fatalf("%v, state %+v", out, vol1(t, actx))
			}
			if recs := closeRecords(t, actx, p); len(recs) != 2 || recs[1].Outcome != tr.OutcomeRefused {
				t.Errorf("records %+v", recs)
			}
		})
	}
	for _, name := range []string{"", "nfs", "blockvolume", "blockvolume:1", "blockvolume:x", "blockvolume:-0", "blockvolume:+0", "blockvolume: 0", "weed-master"} {
		actx, p, e := faultSetup(t)
		f := &fakeNode{}
		out, err := e.kill(context.Background(), actx, f, name, time.Second)
		if err != nil || out["value"] != tr.OutcomeRefused || len(f.calls) != 0 {
			t.Errorf("%q: %v %v, commands %v", name, out, err, f.calls)
		}
		if recs := closeRecords(t, actx, p); len(recs) != 2 || recs[0].Args["process"] != name {
			t.Errorf("%q: records %+v", name, recs)
		}
	}
	// A volume without a holder (never attached, or stopped): nothing to kill.
	actx, _, e := faultSetup(t)
	v := vol1(t, actx)
	v.PID, v.PIDStart = "", ""
	saveVolume(actx, v)
	f := &fakeNode{}
	if out, _ := e.kill(context.Background(), actx, f, "blockvolume:0", time.Second); out["value"] != tr.OutcomeRefused || len(f.calls) != 0 {
		t.Errorf("no holder: %v %v", out, f.calls)
	}
}

func TestKillMasterByItsLedgerIdentity(t *testing.T) {
	actx, _, e := faultSetup(t)
	f := (&fakeNode{}).
		on("cat /w/owned", ok(masterLedger)).
		on("kill -KILL", ok("sent "+cmdline(masterArgv))).
		on("/proc/77", ok("absent\n"))
	if out, _ := e.kill(context.Background(), actx, f, "blockmaster", time.Second); out["value"] != tr.OutcomeOK {
		t.Fatalf("%v", out)
	}
	if !f.issued("kill -KILL 77") || !f.issued("= 700 ]") {
		t.Errorf("not by the ledger's identity: %v", f.calls)
	}
	var m faultProc
	_ = json.Unmarshal([]byte(actx.Vars[masterVar]), &m)
	if m.PID != "77" || m.KilledPID != "77" || !slices.Equal(m.Argv, masterArgv) {
		t.Errorf("master state %+v", m)
	}
	// An unreadable or malformed ledger is no blockmaster: refused, nothing sent.
	for _, a := range []fakeAnswer{{errOut: "no such file", code: 1}, ok("77 blockmaster\n"), ok("11 50 weed-master\n")} {
		actx, _, e := faultSetup(t)
		f := (&fakeNode{}).on("cat /w/owned", a)
		if out, _ := e.kill(context.Background(), actx, f, "blockmaster", time.Second); out["value"] != tr.OutcomeRefused || f.issued("kill") {
			t.Errorf("ledger %q: %v", a.out, out)
		}
	}
}

func restartHolderNode(status string, ctrls string) *fakeNode {
	own := "LISTEN 0 4096 127.0.0.1:21012 0.0.0.0:* users:((\"blockvolume\",pid=5555,fd=9))\nLISTEN 0 4096 127.0.0.1:21013 0.0.0.0:* users:((\"blockvolume\",pid=5555,fd=10))"
	return (&fakeNode{}).
		on("/proc/4242", ok("absent\n")).
		on("ss -Hltnp", ok(""), ok(own)).
		on("mkdir -p", ok("")).
		on("setsid", ok("5555\n")).
		on("/proc/5555", ok("start 200\n")).
		on("/own.sh record", ok("")).
		on("/status?volume=v1", ok(status)).
		on("nvme-fabrics", ok("")).
		on("subsysnqn", ok(ctrls))
}

func TestRestartHolderWithItsOwnCommandLine(t *testing.T) {
	for name, c := range map[string]struct {
		ctrls   string
		connect bool
	}{
		"the kernel kept its controller": {"ctrl nvme3\nns nvme3c3n1\nend\n", false},
		"no controller left":             {"end\n", true},
	} {
		t.Run(name, func(t *testing.T) {
			actx, p, e := faultSetup(t)
			killed(t, actx)
			f := restartHolderNode(statusAt(3, true), c.ctrls)
			if c.connect { // after the connect, one device
				f.rules[len(f.rules)-1].answers = []fakeAnswer{ok("end\n"), ok("ctrl nvme3\nns nvme3c3n1\nend\n")}
			}
			out, err := e.restart(context.Background(), actx, f, "blockvolume:0", 2*time.Second)
			if err != nil || out["value"] != tr.OutcomeOK {
				t.Fatalf("%v %v", out, err)
			}
			if f.issued("nvme-fabrics") != c.connect {
				t.Errorf("connect issued %v, want %v", f.issued("nvme-fabrics"), c.connect)
			}
			if !f.issued("' sh "+strings.Join(quoteAll(holderArgv), " ")+" >/dev/null") || !f.issued("/own.sh record /w tok blockvolume-a-v1 5555") {
				t.Errorf("not the killed command line, or not recorded as owned: %v", f.calls)
			}
			v := vol1(t, actx)
			if v.PID != "5555" || v.PIDStart != "200" || v.Extra["killed_pid"] != "" || v.Extra["argv"] != "" {
				t.Errorf("state %+v", v)
			}
			recs := closeRecords(t, actx, p)
			if len(recs) != 2 || recs[0].Verb != "restart" || recs[0].Op != "Env.Restart" || recs[0].Args["process"] != "blockvolume:0" {
				t.Errorf("records %+v", recs)
			}
		})
	}
}

// Red controls for the restart: a holder that serves BELOW the epoch it had
// (a stale status) or never serves is not ready; a killed process that still
// runs, or a holder the runner never killed (a planned stop), is refused and
// nothing is launched.
func TestRestartHolderIsNotOkUnlessItServesAgain(t *testing.T) {
	for name, st := range map[string]string{"below the recorded epoch": statusAt(2, true), "not healthy": statusAt(3, false)} {
		t.Run(name, func(t *testing.T) {
			actx, _, e := faultSetup(t)
			killed(t, actx)
			f := restartHolderNode(st, "ctrl nvme3\nns nvme3c3n1\nend\n")
			if out, _ := e.restart(context.Background(), actx, f, "blockvolume:0", 600*time.Millisecond); out["value"] != tr.OutcomeUnknown {
				t.Fatalf("%v", out)
			}
		})
	}
	actx, _, e := faultSetup(t)
	killed(t, actx)
	f := restartHolderNode(statusAt(3, true), "end\n")
	f.rules[0].answers = []fakeAnswer{ok("start 100\n")} // the killed pid still runs
	if out, _ := e.restart(context.Background(), actx, f, "blockvolume:0", time.Second); out["value"] != tr.OutcomeRefused || f.issued("setsid") {
		t.Errorf("still running: %v", out)
	}
	actx, _, e = faultSetup(t) // never killed
	f = restartHolderNode(statusAt(3, true), "end\n")
	if out, _ := e.restart(context.Background(), actx, f, "blockvolume:0", time.Second); out["value"] != tr.OutcomeRefused || f.issued("setsid") {
		t.Errorf("never killed: %v", out)
	}
	actx, _, e = faultSetup(t) // killed, then a planned stop cleared the holder
	killed(t, actx)
	v := vol1(t, actx)
	v.PID, v.PIDStart = "", ""
	saveVolume(actx, v)
	if out, _ := e.restart(context.Background(), actx, f, "blockvolume:0", time.Second); out["value"] != tr.OutcomeRefused || f.issued("setsid") {
		t.Errorf("stopped: %v", out)
	}
}

const resolveRefused = `{"outcome":"refused","volume":"v1","error":"not created"}`

func masterKilled(actx *tr.ActionContext) {
	b, _ := json.Marshal(faultProc{Name: "blockmaster", PID: "77", Start: "700", KilledPID: "77", Argv: masterArgv})
	actx.Vars[masterVar] = string(b)
}

func TestRestartMasterThroughTheOwnershipScript(t *testing.T) {
	actx, p, e := faultSetup(t)
	masterKilled(actx)
	f := (&fakeNode{}).
		on("/proc/77", ok("absent\n")).
		on("cat /w/owned", ok(masterLedger), ok(masterLedger+"88 800 blockmaster-restart1\n")).
		on("/own.sh spawn", ok("88\n")).
		on("/proc/88", ok("start 800\n")).
		on("v3ctl resolve", fakeAnswer{out: resolveRefused, code: 1})
	out, err := e.restart(context.Background(), actx, f, "blockmaster", 2*time.Second)
	if err != nil || out["value"] != tr.OutcomeOK {
		t.Fatalf("%v %v", out, err)
	}
	if !f.issued("bash /own.sh spawn /w tok blockmaster-restart1 21003 -- " + strings.Join(quoteAll(masterArgv), " ")) {
		t.Errorf("spawn: %v", f.calls)
	}
	var m faultProc
	_ = json.Unmarshal([]byte(actx.Vars[masterVar]), &m)
	if m.PID != "88" || m.Start != "800" || m.KilledPID != "" {
		t.Errorf("master state %+v", m)
	}
	if recs := closeRecords(t, actx, p); len(recs) != 2 || recs[1].Outcome != tr.OutcomeOK || recs[1].Result["pid"] != "88" {
		t.Errorf("records %+v", recs)
	}
}

// Red control: the restarted master holds its port but does not answer (no
// answer, or one that does not name the request): unknown, not ok. A spawn
// that refused before starting anything is refused.
func TestRestartMasterIsNotOkUntilItAnswers(t *testing.T) {
	for name, a := range map[string]fakeAnswer{
		"transport failure": {out: `{"outcome":"unknown","volume":"v1"}`, code: 3},
		"garbage":           {out: "<html>", code: 0},
		"another volume":    {out: `{"outcome":"refused","volume":"v9"}`, code: 1},
	} {
		t.Run(name, func(t *testing.T) {
			actx, _, e := faultSetup(t)
			masterKilled(actx)
			f := (&fakeNode{}).
				on("/proc/77", ok("absent\n")).
				on("cat /w/owned", ok(masterLedger), ok(masterLedger+"88 800 blockmaster-restart1\n")).
				on("/own.sh spawn", ok("88\n")).
				on("/proc/88", ok("start 800\n")).
				on("v3ctl resolve", a)
			if out, _ := e.restart(context.Background(), actx, f, "blockmaster", 700*time.Millisecond); out["value"] != tr.OutcomeUnknown {
				t.Fatalf("%v", out)
			}
		})
	}
	actx, _, e := faultSetup(t)
	masterKilled(actx)
	f := (&fakeNode{}).
		on("/proc/77", ok("absent\n")).
		on("cat /w/owned", ok(masterLedger)).
		on("/own.sh spawn", fakeAnswer{errOut: "m0-own: port 21003 already listens (pids 99): blockmaster-restart1 not started", code: 1})
	if out, _ := e.restart(context.Background(), actx, f, "blockmaster", time.Second); out["value"] != tr.OutcomeRefused {
		t.Errorf("spawn refused: %v", out)
	}
}

// healthNode answers every look of a run whose master (pid 77) and holder
// (pid 4242) run and serve; each test spoils one answer.
func healthNode() *fakeNode {
	return (&fakeNode{}).
		on("cat /w/owned", ok(masterLedger)).
		on("/proc/77", ok("start 700\n")).
		on("/proc/4242", ok("start 100\n")).
		on("v3ctl resolve", fakeAnswer{out: resolveRefused, code: 1}).
		on("/status?volume=v1", ok(statusAt(3, true))).
		on("ss -Hltnp", ok(statusListener("4242")))
}

func statusListener(pid string) string {
	return "LISTEN 0 4096 127.0.0.1:21012 0.0.0.0:* users:((\"blockvolume\",pid=" + pid + ",fd=9))\n" +
		"LISTEN 0 4096 [::ffff:127.0.0.1]:21012 [::]:* users:((\"blockvolume\",pid=" + pid + ",fd=11))"
}

// The healthy record names, per holder, the process that holds the volume's
// status listener now, with its start time, read from the host. Red control:
// a holder restarted behind the runner's back (a supervisor, after a lost
// lease) must show its new pid and start time, not the runner's record.
func TestHealthyNamesTheLiveHolder(t *testing.T) {
	holder := func(f *fakeNode) map[string]any {
		actx, p, e := faultSetup(t)
		if _, err := e.healthy(context.Background(), actx, f, 0); err != nil {
			t.Fatal(err)
		}
		hs, _ := healthyRecord(t, actx, p)["holders"].(map[string]any)
		h, _ := hs["v1"].(map[string]any)
		if len(hs) != 1 || h == nil {
			t.Fatalf("holders %v", hs)
		}
		return h
	}
	if h := holder(healthNode()); h["pid"] != float64(4242) || h["start"] != "100" {
		t.Errorf("the recorded holder: %v", h)
	}
	f := healthNode()
	f.rules = append([]fakeRule{
		{match: "ss -Hltnp", answers: []fakeAnswer{ok(statusListener("6000"))}},
		{match: "/proc/6000", answers: []fakeAnswer{ok("start 900\n")}},
		{match: "/proc/4242", answers: []fakeAnswer{ok("absent\n")}},
	}, f.rules...)
	if h := holder(f); h["pid"] != float64(6000) || h["start"] != "900" {
		t.Errorf("restarted behind the runner's back: %v", h)
	}
	// Nobody, or two processes, on the status port: no pid is named.
	for _, ss := range []string{"", statusListener("6000") + "\n" + statusListener("6001")} {
		f := healthNode()
		f.rules = append([]fakeRule{{match: "ss -Hltnp", answers: []fakeAnswer{ok(ss)}}, {match: "/proc/600", answers: []fakeAnswer{ok("start 1\n")}}}, f.rules...)
		if h := holder(f); h["pid"] != nil || h["error"] == nil {
			t.Errorf("ss %q: %v", ss, h)
		}
	}
}

func healthyRecord(t *testing.T, actx *tr.ActionContext, p string) map[string]any {
	t.Helper()
	recs := closeRecords(t, actx, p)
	if len(recs) != 1 || recs[0].Kind != tr.HistoryObserve || recs[0].Verb != "healthy" || recs[0].Client != "runner" || recs[0].Volume != "" {
		t.Fatalf("records %+v", recs)
	}
	return recs[0].Result
}

func TestHealthyWhenEverythingRunsAnswersAndServes(t *testing.T) {
	actx, p, e := faultSetup(t)
	out, err := e.healthy(context.Background(), actx, healthNode(), 0)
	if err != nil || out["healthy"] != "true" {
		t.Fatalf("%v %v", out, err)
	}
	r := healthyRecord(t, actx, p)
	if r["healthy"] != true || len(r["down"].([]any)) != 0 || len(r["not_serving"].([]any)) != 0 {
		t.Errorf("result %v", r)
	}
}

// Red controls: each way of not being healthy must be seen.
func TestHealthyCatchesEachWayOfNotBeingHealthy(t *testing.T) {
	for name, c := range map[string]struct {
		rule        string
		a           fakeAnswer
		down, notSv []string
	}{
		"killed holder not restarted":  {"/proc/4242", ok("absent\n"), []string{"blockvolume:0"}, nil},
		"holder pid recycled":          {"/proc/4242", ok("start 1\n"), []string{"blockvolume:0"}, nil},
		"holder not serving":           {"/status?volume=v1", ok(statusAt(3, false)), nil, []string{"v1"}},
		"holder status no answer":      {"/status?volume=v1", fakeAnswer{code: 7}, nil, []string{"v1"}},
		"master killed":                {"/proc/77", ok("absent\n"), []string{"blockmaster"}, nil},
		"master runs, does not answer": {"v3ctl resolve", fakeAnswer{out: `{"outcome":"unknown","volume":"v1"}`, code: 3}, nil, nil},
		"ledger unreadable":            {"cat /w/owned", fakeAnswer{code: 1}, []string{"blockmaster"}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			actx, p, e := faultSetup(t)
			f := healthNode()
			f.rules = append([]fakeRule{{match: c.rule, answers: []fakeAnswer{c.a}}}, f.rules...)
			if out, _ := e.healthy(context.Background(), actx, f, 0); out["healthy"] != "false" {
				t.Fatalf("%v", out)
			}
			r := healthyRecord(t, actx, p)
			strs := func(k string) []string {
				var s []string
				for _, x := range r[k].([]any) {
					s = append(s, x.(string))
				}
				return s
			}
			if r["healthy"] != false || !slices.Equal(strs("down"), c.down) || !slices.Equal(strs("not_serving"), c.notSv) {
				t.Errorf("result %v", r)
			}
			if name == "master runs, does not answer" && r["master_answers"] != false {
				t.Errorf("result %v", r)
			}
		})
	}
}

// A volume without a holder (stopped, deleted, never attached) is not
// expected to serve; wait_seconds looks again until healthy.
func TestHealthySkipsVolumesWithoutAHolderAndWaits(t *testing.T) {
	actx, p, e := faultSetup(t)
	v := vol1(t, actx)
	v.PID, v.PIDStart, v.Extra["intent"] = "", "", intentNotHeld
	saveVolume(actx, v)
	f := healthNode()
	f.rules = append([]fakeRule{{match: "/status?volume=v1", answers: []fakeAnswer{{code: 7}}}}, f.rules...)
	if out, _ := e.healthy(context.Background(), actx, f, 0); out["healthy"] != "true" {
		t.Errorf("a volume without a holder: %v", out)
	}
	_ = closeRecords(t, actx, p)

	actx, p, e = faultSetup(t)
	f = healthNode()
	f.rules = append([]fakeRule{{match: "/status?volume=v1", answers: []fakeAnswer{ok(statusAt(3, false)), ok(statusAt(4, true))}}}, f.rules...)
	if out, _ := e.healthy(context.Background(), actx, f, 3*time.Second); out["healthy"] != "true" {
		t.Fatalf("%v", out)
	}
	if r := healthyRecord(t, actx, p); r["attempts"] != float64(2) {
		t.Errorf("result %v", r)
	}
}

// A holder fault names the volume it resolved to in its invoke args; the
// record's Volume stays "" and a master fault carries no volume key. Red
// control: two volumes, so a resolution to another index's name is seen.
func TestFaultArgsNameTheResolvedVolume(t *testing.T) {
	actx, p, e := faultSetup(t)
	vs, err := specVolumes([]string{"v1", "v2"}, 21000, "nqn.x:")
	if err != nil {
		t.Fatal(err)
	}
	v2 := vs[1]
	v2.PID, v2.PIDStart = "4343", "300"
	saveVolume(actx, v2)
	actx.Vars[specOrderVar] = "v1 v2"
	f := (&fakeNode{}).
		on("cat /w/owned", ok(masterLedger)).
		on("kill -KILL", ok("gone\n"))
	ctx := context.Background()
	_, _ = e.kill(ctx, actx, f, "blockvolume:1", time.Second)
	_, _ = e.restart(ctx, actx, f, "blockvolume:0", time.Second)
	_, _ = e.kill(ctx, actx, f, "blockmaster", time.Second)
	_, _ = e.kill(ctx, actx, f, "blockvolume:2", time.Second) // unknown: nothing resolved
	var invokes []tr.HistoryRecord
	for _, r := range closeRecords(t, actx, p) {
		if r.Kind == tr.HistoryInvoke {
			invokes = append(invokes, r)
		}
	}
	if len(invokes) != 4 {
		t.Fatalf("invokes %+v", invokes)
	}
	for i, want := range []any{"v2", "v1", nil, nil} {
		r := invokes[i]
		got, has := r.Args["volume"]
		if r.Volume != "" || (want == nil) == has || (has && got != want) {
			t.Errorf("%s %v: Volume %q, args volume %v (present %v), want %v", r.Verb, r.Args["process"], r.Volume, got, has, want)
		}
	}
	if invokes[0].Args["pid"] != float64(4343) {
		t.Errorf("blockvolume:1 resolved to pid %v, want v2's 4343", invokes[0].Args["pid"])
	}
}

func TestDecodeArgv(t *testing.T) {
	if a, err := decodeArgv(cmdline(holderArgv)); err != nil || !slices.Equal(a, holderArgv) {
		t.Errorf("%q %v", a, err)
	}
	for _, bad := range []string{"", "!!", base64.StdEncoding.EncodeToString([]byte("\x00"))} {
		if _, err := decodeArgv(bad); err == nil {
			t.Errorf("%q decoded", bad)
		}
	}
}

func TestFaultActionsRegisteredAndNeedAHistory(t *testing.T) {
	r := tr.NewRegistry()
	RegisterPack(r)
	for _, n := range []string{"v3_op_kill", "v3_op_restart", "v3_op_healthy"} {
		if !r.Has(n) {
			t.Errorf("%s not registered", n)
		}
		if genHandlers[n] == nil {
			t.Errorf("%s not in genHandlers", n)
		}
	}
	actx := &tr.ActionContext{Vars: map[string]string{}, Log: func(string, ...interface{}) {}}
	for _, h := range []tr.ActionHandlerFunc{blockActions.Handler("v3_op_kill"), v3OpRestart, v3OpHealthy} {
		if _, err := h(context.Background(), actx, tr.Action{Params: map[string]string{"process": "blockmaster"}}); err == nil || !strings.Contains(err.Error(), "no history") {
			t.Errorf("ran without a history: %v", err)
		}
	}
}

// A single look looks once and waits for nothing, even when the look is not
// healthy, and says so in its record.
func TestHealthySingleLookLooksOnceAndSaysSo(t *testing.T) {
	actx, p, e := faultSetup(t)
	f := healthNode()
	f.rules = append([]fakeRule{{match: "/status?volume=v1", answers: []fakeAnswer{ok(statusAt(3, false)), ok(statusAt(4, true))}}}, f.rules...)
	out, err := e.singleLook(context.Background(), actx, f)
	if err != nil || out["healthy"] != "false" {
		t.Fatalf("%v %v", out, err)
	}
	r := healthyRecord(t, actx, p)
	if r["look"] != "single" || r["attempts"] != float64(1) || len(r["not_serving"].([]any)) != 1 || r["holders"] == nil {
		t.Errorf("result %v", r)
	}
	// A normal record carries no look.
	actx, p, e = faultSetup(t)
	if _, err := e.healthy(context.Background(), actx, healthNode(), 0); err != nil {
		t.Fatal(err)
	}
	if r := healthyRecord(t, actx, p); r["look"] != nil {
		t.Errorf("a normal healthy record says look %v", r["look"])
	}
	if _, err := v3OpHealthy(context.Background(), actx, tr.Action{Action: "v3_op_healthy", Params: map[string]string{"look": "twice"}}); err == nil {
		t.Error("look=twice accepted")
	}
}

// The block handlers keep the generic run's handler verb contract
// (randomrun/doc.go): a kill and a restart record invoke and complete with
// verb kill / restart; a healthy record has verb healthy and answers
// {"value": "ok", "healthy": "true"|"false"}; a single look says
// result.look "single".
func TestBlockHandlersKeepTheHandlerVerbContract(t *testing.T) {
	actx, p, e := faultSetup(t)
	f := (&fakeNode{}).on("kill -KILL", ok("sent "+cmdline(holderArgv)+"\n")).on("/proc/4242", ok("absent\n"))
	if _, err := e.kill(context.Background(), actx, f, "blockvolume:0", time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := e.restart(context.Background(), actx, (&fakeNode{}), "blockvolume:0", time.Second); err != nil {
		t.Fatal(err)
	}
	var verbs []string
	for _, r := range closeRecords(t, actx, p) {
		if r.Kind == tr.HistoryInvoke || r.Kind == tr.HistoryComplete {
			verbs = append(verbs, r.Kind+" "+r.Verb)
		}
	}
	want := []string{tr.HistoryInvoke + " " + randomrun.VerbKill, tr.HistoryComplete + " " + randomrun.VerbKill,
		tr.HistoryInvoke + " " + randomrun.VerbRestart, tr.HistoryComplete + " " + randomrun.VerbRestart}
	if !slices.Equal(verbs, want) {
		t.Fatalf("records %v, want %v", verbs, want)
	}
	for name, look := range map[string]func(*tr.ActionContext, opEnv) (map[string]string, error){
		"healthy": func(a *tr.ActionContext, e opEnv) (map[string]string, error) {
			return e.healthy(context.Background(), a, healthNode(), 0)
		},
		"single look": func(a *tr.ActionContext, e opEnv) (map[string]string, error) {
			return e.singleLook(context.Background(), a, healthNode())
		},
	} {
		actx, p, e := faultSetup(t)
		out, err := look(actx, e)
		if err != nil || out["value"] != tr.OutcomeOK || (out[randomrun.HealthyKey] != randomrun.HealthyTrue && out[randomrun.HealthyKey] != "false") ||
			(name == "healthy" && len(out) != 2) || (name == "single look" && (len(out) != 3 || out[randomrun.DownKey] != "")) {
			t.Fatalf("%s: answer %v %v", name, out, err)
		}
		recs := closeRecords(t, actx, p)
		if len(recs) != 1 || recs[0].Verb != randomrun.VerbHealthy {
			t.Fatalf("%s: records %+v", name, recs)
		}
		if l := recs[0].Result["look"]; (name == "single look") != (l == randomrun.LookSingle) {
			t.Fatalf("%s: look %v", name, l)
		}
	}
}

// --- the holder's unit (supervise) ---

const oldExit = "/r/a-v1.1.log.exit"

// exited puts v1 in the state of a holder launched through the wrapper.
func exited(t *testing.T, actx *tr.ActionContext) {
	v := vol1(t, actx)
	b, _ := json.Marshal(holderArgv)
	v.Extra["launch_argv"], v.Extra["exit_file"] = string(b), oldExit
	saveVolume(actx, v)
}

// exitAnswer is the exit file's content and the host's clock, ago before now.
func exitAnswer(status int, ago time.Duration) fakeAnswer {
	const now = int64(2_000_000_000_000_000_000)
	return ok(fmt.Sprintf("%d %d\n%d\n", status, now-int64(ago), now))
}

// unitNode: the holder 4242 is gone; its exit file answers exit; a relaunch
// starts 5555, which serves.
func unitNode(exit fakeAnswer) *fakeNode {
	own := "LISTEN 0 4096 127.0.0.1:21012 0.0.0.0:* users:((\"blockvolume\",pid=5555,fd=9))\nLISTEN 0 4096 127.0.0.1:21013 0.0.0.0:* users:((\"blockvolume\",pid=5555,fd=10))"
	return (&fakeNode{}).
		on("setsid", ok("5555\n")). // first: the launch names its own exit file
		on("/proc/4242", ok("absent\n")).
		on(oldExit, exit).
		on("a-v1.1.log", ok("blockvolume: exiting status=75 reason=lease_expired\n")).
		on("ss -Hltnp", ok(""), ok(own)).
		on("mkdir -p", ok("")).
		on("/proc/5555", ok("start 200\n")).
		on("/own.sh record", ok("")).
		on("/status?volume=v1", ok(statusAt(3, true))).
		on("subsysnqn", ok("ctrl nvme3\nns nvme3c3n1\nend\n"))
}

func unitRecords(recs []tr.HistoryRecord) []tr.HistoryRecord {
	var out []tr.HistoryRecord
	for _, r := range recs {
		if r.Verb == "unit_restart" {
			out = append(out, r)
		}
	}
	return out
}

func readsTheLog(f *fakeNode) bool {
	for _, c := range f.calls {
		if strings.Contains(c, "a-v1.1.log") && !strings.Contains(c, oldExit) {
			return true
		}
	}
	return false
}

// A held volume's holder that exited 75 (fenced: lease expired) is launched
// again with its recorded command line, and the relaunch is its own record.
func TestSuperviseRelaunchesAFencedHolder(t *testing.T) {
	actx, p, e := faultSetup(t)
	exited(t, actx)
	f := unitNode(exitAnswer(75, 10*time.Second))
	if err := e.supervise(context.Background(), actx, f); err != nil {
		t.Fatal(err)
	}
	v := vol1(t, actx)
	if v.PID != "5555" || v.PIDStart != "200" || v.Extra["exit_file"] == oldExit || !strings.HasSuffix(v.Extra["exit_file"], ".log.exit") {
		t.Errorf("state %+v", v)
	}
	if !f.issued("' sh " + strings.Join(quoteAll(holderArgv), " ") + " >/dev/null") {
		t.Error("not relaunched with its recorded command line")
	}
	if readsTheLog(f) {
		t.Error("the holder's log was read")
	}
	us := unitRecords(closeRecords(t, actx, p))
	if len(us) != 1 {
		t.Fatalf("unit records %+v", us)
	}
	r := us[0]
	if r.Kind != tr.HistoryObserve || r.Client != "runner" || r.Result["volume"] != "v1" || r.Result["exit_status"] != float64(75) ||
		r.Result["old_pid"] != "4242" || r.Result["old_start"] != "100" || r.Result["new_pid"] != "5555" || r.Result["new_start"] != "200" || r.Result["launch"] != tr.OutcomeOK {
		t.Errorf("unit_restart %+v", r.Result)
	}
}

// Red control: the relaunch waits RestartSec after the exit, not less.
func TestSuperviseWaitsRestartSecAfterTheExit(t *testing.T) {
	actx, _, e := faultSetup(t)
	exited(t, actx)
	f := unitNode(exitAnswer(75, 4500*time.Millisecond))
	start := time.Now()
	if err := e.supervise(context.Background(), actx, f); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < 450*time.Millisecond || d > 3*time.Second {
		t.Errorf("relaunched %s after the look; the exit was 4.5 s before it, RestartSec 5 s", d)
	}
	if vol1(t, actx).PID != "5555" {
		t.Error("not relaunched")
	}
}

// Red controls: the unit does not relaunch after status 78 or 0, without a
// status from the process (whatever the log says), for a volume stopped ok,
// for a holder the runner killed (the schedule restarts it), or while the
// holder runs.
func TestSuperviseLeavesAloneWhatTheUnitWouldNotRestart(t *testing.T) {
	for name, c := range map[string]struct {
		exit  fakeAnswer
		setup func(v *opVolume)
		rule  *fakeRule
	}{
		"status 78 (no restart)":             {exit: exitAnswer(78, time.Minute)},
		"status 0 (a planned exit)":          {exit: exitAnswer(0, time.Minute)},
		"no status written, the log says 75": {exit: ok("none\n2000000000000000000\n")},
		"exit file unreadable":               {exit: fakeAnswer{errOut: "denied", code: 3}},
		"volume stopped ok":                  {exit: exitAnswer(75, time.Minute), setup: func(v *opVolume) { v.PID, v.PIDStart, v.Extra["intent"] = "", "", intentNotHeld }},
		"killed by the runner":               {exit: exitAnswer(137, time.Minute), setup: func(v *opVolume) { v.Extra["killed_pid"] = "4242" }},
		"holder still running":               {exit: exitAnswer(75, time.Minute), rule: &fakeRule{match: "/proc/4242", answers: []fakeAnswer{ok("start 100\n")}}},
	} {
		t.Run(name, func(t *testing.T) {
			actx, p, e := faultSetup(t)
			exited(t, actx)
			if c.setup != nil {
				v := vol1(t, actx)
				c.setup(v)
				saveVolume(actx, v)
			}
			f := unitNode(c.exit)
			if c.rule != nil {
				f.rules = append([]fakeRule{*c.rule}, f.rules...)
			}
			if err := e.supervise(context.Background(), actx, f); err != nil {
				t.Fatal(err)
			}
			if f.issued("setsid") || readsTheLog(f) {
				t.Errorf("relaunched, or read the log: %v", f.calls)
			}
			if us := unitRecords(closeRecords(t, actx, p)); len(us) != 0 {
				t.Errorf("unit records %+v", us)
			}
		})
	}
}

// The supervision step runs before each healthy look, and after a master restart.
func TestHealthyAndMasterRestartSupervise(t *testing.T) {
	actx, p, e := faultSetup(t)
	exited(t, actx)
	f := unitNode(exitAnswer(75, time.Minute))
	f.on("cat /w/owned", ok(masterLedger)).on("/proc/77", ok("start 700\n")).on("v3ctl resolve", fakeAnswer{out: resolveRefused, code: 1})
	if out, err := e.healthy(context.Background(), actx, f, 0); err != nil || out["healthy"] != "true" {
		t.Fatalf("%v %v", out, err)
	}
	recs := closeRecords(t, actx, p)
	if len(recs) != 2 || recs[0].Verb != "unit_restart" || recs[1].Verb != "healthy" {
		t.Fatalf("records %+v", recs)
	}

	actx, p, e = faultSetup(t)
	exited(t, actx)
	masterKilled(actx)
	f = unitNode(exitAnswer(75, time.Minute))
	f.rules = append([]fakeRule{
		{match: "/proc/77", answers: []fakeAnswer{ok("absent\n")}},
		{match: "cat /w/owned", answers: []fakeAnswer{ok(masterLedger), ok(masterLedger + "88 800 blockmaster-restart1\n")}},
		{match: "/own.sh spawn", answers: []fakeAnswer{ok("88\n")}},
		{match: "/proc/88", answers: []fakeAnswer{ok("start 800\n")}},
		{match: "v3ctl resolve", answers: []fakeAnswer{{out: resolveRefused, code: 1}}},
	}, f.rules...)
	if out, err := e.restart(context.Background(), actx, f, "blockmaster", 2*time.Second); err != nil || out["value"] != tr.OutcomeOK {
		t.Fatalf("%v %v", out, err)
	}
	recs = closeRecords(t, actx, p)
	if len(recs) != 3 || recs[1].Kind != tr.HistoryComplete || recs[2].Verb != "unit_restart" {
		t.Errorf("records %+v", recs)
	}
}

// --- the unit policy, read from the product's unit file ---

func TestParseUnitReadsThePolicy(t *testing.T) {
	p, err := parseUnit(testUnit)
	if err != nil {
		t.Fatal(err)
	}
	// The continuation line naming "Restart=always" inside ExecStart is not a key.
	if p.Restart != "on-failure" || !slices.Equal(p.PreventStatus, []int{78}) || p.RestartSec != 5 || p.TimeoutStopSec != 30 || p.restartSec() != 5*time.Second {
		t.Errorf("policy %+v", p)
	}
	for status, want := range map[int]bool{75: true, 1: true, 137: true, 0: false, 78: false, 143: false, 129: false} {
		if p.restartable(status) != want {
			t.Errorf("status %d: restartable %v", status, !want)
		}
	}
	for _, s := range []string{"1min 30s", "500ms", "90"} {
		if _, err := systemdSeconds(s); err != nil {
			t.Errorf("%q: %v", s, err)
		}
	}
	// Red controls: what the runner cannot emulate is refused, never guessed.
	for name, text := range map[string]string{
		"no Restart":                "[Service]\nRestartSec=5s\n",
		"Restart only in [Unit]":    "[Unit]\nRestart=on-failure\n[Service]\nRestartSec=5s\n",
		"Restart=on-abnormal":       "[Service]\nRestart=on-abnormal\nRestartSec=5s\n",
		"prevent by signal name":    "[Service]\nRestart=on-failure\nRestartPreventExitStatus=78 SIGKILL\nRestartSec=5s\n",
		"no RestartSec":             "[Service]\nRestart=on-failure\n",
		"RestartSec=infinity":       "[Service]\nRestart=on-failure\nRestartSec=infinity\n",
		"TimeoutStopSec unreadable": "[Service]\nRestart=on-failure\nRestartSec=5s\nTimeoutStopSec=soon\n",
	} {
		if _, err := parseUnit(text); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

const productSHA = "14dff69b0f7cc139f9bde7849dec6ab34063c947"

// The policy is read from the product checkout at the commit the run built,
// once; unreadable, the runner refuses to emulate (and relaunches nothing).
func TestUnitPolicyReadFromTheProductTree(t *testing.T) {
	actx, _, e := faultSetup(t)
	delete(actx.Vars, unitPolicyVar)
	actx.Vars["product_src"] = "/src"
	f := (&fakeNode{}).
		on("cat /w/product.sha", ok(productSHA+"\n")).
		on("git -C /src show "+productSHA+":deploy/systemd/sw-blockvolume.service", ok(testUnit))
	p, err := loadUnitPolicy(context.Background(), actx, f, e)
	if err != nil || p.File != "deploy/systemd/sw-blockvolume.service" || p.Commit != productSHA || p.RestartSec != 5 {
		t.Fatalf("%+v %v", p, err)
	}
	if actx.Vars[unitPolicyVar] == "" {
		t.Error("the policy was not kept for the run")
	}
	h := unitHeader(p)
	if h["emulated"] != true || h["note"] != "a unit-emulated run is not evidence for the unit's own behaviour" || h["cite"] != "deploy/systemd/sw-blockvolume.service at "+productSHA || h["policy"].(unitPolicy).Commit != productSHA {
		t.Errorf("header %v", h)
	}
	for name, f := range map[string]*fakeNode{
		"file missing at the commit": (&fakeNode{}).on("cat /w/product.sha", ok(productSHA)).on("git -C", fakeAnswer{errOut: "fatal: path does not exist", code: 128}),
		"commit unreadable":          (&fakeNode{}).on("cat /w/product.sha", fakeAnswer{code: 1}),
		"unit not emulable":          (&fakeNode{}).on("cat /w/product.sha", ok(productSHA)).on("git -C", ok("[Service]\nRestart=on-watchdog\nRestartSec=5s\n")),
	} {
		actx, _, e := faultSetup(t)
		delete(actx.Vars, unitPolicyVar)
		actx.Vars["product_src"] = "/src"
		exited(t, actx)
		if _, err := loadUnitPolicy(context.Background(), actx, f, e); err == nil || !strings.Contains(err.Error(), "refuses to emulate") {
			t.Errorf("%s: %v", name, err)
		}
		if err := e.supervise(context.Background(), actx, f); err == nil || f.issued("setsid") {
			t.Errorf("%s: supervised without a policy: %v", name, err)
		}
	}
}

// A run's header carries the pack's unit entry (randomrun.Runner.Header).
func TestGenRunHeaderSaysUnitEmulated(t *testing.T) {
	f := &fakeProduct{}
	f.install(t)
	actx, p := genRunCtx(t)
	cfg, err := genConfig(genAct("5"), actx)
	if err != nil {
		t.Fatal(err)
	}
	g, err := NewGenerator(cfg)
	if err != nil {
		t.Fatal(err)
	}
	pol, _ := parseUnit(testUnit)
	r := genRunner("v3_gen_run", "host", g.m)
	r.Header = withUnit(r.Header, pol)
	if _, err := r.Run(context.Background(), actx, g.Generator, func(string) string { return "" }); err != nil {
		t.Fatal(err)
	}
	_ = actx.History.Close()
	h := records(t, p)[0]
	u, _ := h.Result["unit"].(map[string]any)
	if h.Verb != "gen_header" || u["emulated"] != true || u["note"] != unitNotEvidence {
		t.Errorf("header %+v", h.Result)
	}
}

// --- the intended state: held, not held, unsettled ---

// The answer sets the state: ok sets the operation's target, unknown leaves
// the volume UNSETTLED (an unknown stop is never "not held"), a refusal or a
// runner error changes nothing. Red control: an unknown taken for its target.
func TestIntentIsSetByTheAnswer(t *testing.T) {
	for _, c := range []struct {
		before, target, answer string
		err                    error
		want                   string
	}{
		{intentHeld, intentNotHeld, tr.OutcomeOK, nil, intentNotHeld},
		{intentHeld, intentNotHeld, tr.OutcomeUnknown, nil, intentUnsettled},
		{intentNotHeld, intentHeld, tr.OutcomeUnknown, nil, intentUnsettled},
		{intentNotHeld, intentHeld, tr.OutcomeOK, nil, intentHeld},
		{intentHeld, intentNotHeld, tr.OutcomeRefused, nil, intentHeld},
		{intentHeld, intentNotHeld, "", fmt.Errorf("no history"), intentHeld},
	} {
		v := &opVolume{Extra: map[string]string{"intent": c.before}}
		out, err := lifecycle(v, c.target)(map[string]string{"value": c.answer}, c.err)
		if v.Extra["intent"] != c.want || err != c.err || out["value"] != c.answer {
			t.Errorf("%s, %s answered %q: intent %q, want %q", c.before, c.target, c.answer, v.Extra["intent"], c.want)
		}
	}
	// The lifecycle operations (declared) record their answer through the
	// intended state: attach holds, stop and move release, unknown
	// unsettles, a refusal nothing.
	want := map[string]string{"v3_op_attach": intentHeld, "v3_op_stop": intentNotHeld, "v3_op_move": intentNotHeld}
	for _, a := range blockActions.File.Actions {
		target, ok := want[a.Action]
		if !ok {
			continue
		}
		delete(want, a.Action)
		set := a.OnOutcomeSet
		if len(set) != 2 || set[tr.OutcomeOK]["volume.intent"] != target || set[tr.OutcomeUnknown]["volume.intent"] != intentUnsettled {
			t.Errorf("%s does not set the intended state from its answer: %v", a.Action, set)
		}
	}
	if len(want) != 0 {
		t.Errorf("not declared: %v", want)
	}
}
func unsettledSetup(t *testing.T) (*tr.ActionContext, string, opEnv) {
	actx, p, e := faultSetup(t)
	v := vol1(t, actx)
	v.Extra["intent"] = intentUnsettled
	// Completed long ago: past its recovery deadline.
	v.Extra["unsettled_at"] = strconv.FormatInt(time.Now().Add(-time.Hour).UnixNano(), 10)
	saveVolume(actx, v)
	return actx, p, e
}

func wsShow(lease, ack string) fakeAnswer {
	return ok("volume: v1 (query snapshot, not authority to serve)\nreplica: r1\nepoch: 3\nlease state: " + lease + "\nholder: r1@3 (logical replica)\nrecorded ACK: " + ack + "\nhost: unknown\n")
}

// An UNSETTLED volume is placed by observation, and the placement is a
// "settled" record. Red controls: a holder alive but not serving, or gone
// while the workspace lease is still granted, is the third state (reported,
// not healthy), never placed.
func TestUnsettledIsPlacedByObservation(t *testing.T) {
	for name, c := range map[string]struct {
		rules  []fakeRule
		state  string // "" : third state
		pidSet bool
	}{
		"same pid still serving: held": {nil, intentHeld, true},
		"gone, lease expired: not held": {[]fakeRule{{match: "/proc/4242", answers: []fakeAnswer{ok("absent\n")}},
			{match: "ops workspace show", answers: []fakeAnswer{wsShow("expired", "none")}}}, intentNotHeld, false},
		"gone, ACK recorded: not held": {[]fakeRule{{match: "/proc/4242", answers: []fakeAnswer{ok("absent\n")}},
			{match: "ops workspace show", answers: []fakeAnswer{wsShow("granted", "r1@3 HEAD generation 4")}}}, intentNotHeld, false},
		"gone, lease revoking: not held": {[]fakeRule{{match: "/proc/4242", answers: []fakeAnswer{ok("absent\n")}},
			{match: "ops workspace show", answers: []fakeAnswer{wsShow("revoking", "none")}}}, intentNotHeld, false},
		"alive, not serving: third state": {[]fakeRule{{match: "/status?volume=v1", answers: []fakeAnswer{ok(statusAt(3, false))}}}, "", true},
		"alive, another pid serves: third state": {[]fakeRule{{match: "ss -Hltnp", answers: []fakeAnswer{ok(statusListener("6000"))}},
			{match: "/proc/6000", answers: []fakeAnswer{ok("start 900\n")}}}, "", true},
		"gone, lease granted, no ACK: third state": {[]fakeRule{{match: "/proc/4242", answers: []fakeAnswer{ok("absent\n")}},
			{match: "ops workspace show", answers: []fakeAnswer{wsShow("granted", "none")}}}, "", true},
		"gone, workspace unreadable: third state": {[]fakeRule{{match: "/proc/4242", answers: []fakeAnswer{ok("absent\n")}},
			{match: "ops workspace show", answers: []fakeAnswer{{code: 3}}}}, "", true},
	} {
		t.Run(name, func(t *testing.T) {
			actx, p, e := unsettledSetup(t)
			f := healthNode()
			f.rules = append(append(c.rules, fakeRule{match: "/status?volume=v1", answers: []fakeAnswer{ok(statusAt(3, true))}}), f.rules...)
			if c.state == intentNotHeld { // a released volume must not be served
				f.rules = append([]fakeRule{{match: "/status?volume=v1", answers: []fakeAnswer{{code: 7}}}}, f.rules...)
			}
			out, err := e.healthy(context.Background(), actx, f, 0)
			if err != nil {
				t.Fatal(err)
			}
			v := vol1(t, actx)
			recs := closeRecords(t, actx, p)
			last := recs[len(recs)-1].Result
			var settled []tr.HistoryRecord
			for _, r := range recs {
				if r.Verb == "settled" {
					settled = append(settled, r)
				}
			}
			third, _ := last["third_state"].([]any)
			if c.state == "" {
				if out["healthy"] != "false" || len(third) != 1 || len(settled) != 0 || v.Extra["intent"] != intentUnsettled {
					t.Fatalf("third state not reported: %v, settled %+v, intent %q", last, settled, v.Extra["intent"])
				}
				if w := third[0].(map[string]any); w["volume"] != "v1" || w["why"] == "" {
					t.Errorf("third_state %v", third)
				}
				return
			}
			if out["healthy"] != "true" || len(third) != 0 || len(settled) != 1 || settled[0].Result["state"] != c.state || settled[0].Result["volume"] != "v1" || settled[0].Result["evidence"] == nil {
				t.Fatalf("not settled %s: %v, settled %+v", c.state, last, settled)
			}
			if v.Extra["intent"] != c.state || (v.PID != "") != c.pidSet {
				t.Errorf("state %+v", v)
			}
		})
	}
}

// Of a NOT HELD volume healthy demands only that no holder serves it. Red
// control: one that is served is reported and not healthy.
func TestNotHeldMustNotBeServed(t *testing.T) {
	for _, served := range []bool{false, true} {
		actx, p, e := faultSetup(t)
		v := vol1(t, actx)
		v.PID, v.PIDStart, v.Extra["intent"] = "", "", intentNotHeld
		saveVolume(actx, v)
		f := healthNode()
		a := fakeAnswer{code: 7}
		if served {
			a = ok(statusAt(4, true))
		}
		f.rules = append([]fakeRule{{match: "/status?volume=v1", answers: []fakeAnswer{a}}}, f.rules...)
		out, _ := e.healthy(context.Background(), actx, f, 0)
		r := healthyRecord(t, actx, p)
		sn, _ := r["served_not_held"].([]any)
		if (out["healthy"] == "true") == served || (len(sn) == 1) != served {
			t.Errorf("served %v: %v", served, r)
		}
	}
}

// Red control: the unit relaunches only the holder of a HELD volume, not of
// an UNSETTLED or NOT HELD one (an unknown stop, then a fence: exit 75).
func TestSuperviseOnlyHeldVolumes(t *testing.T) {
	for _, intent := range []string{intentUnsettled, intentNotHeld, intentHeld} {
		actx, _, e := faultSetup(t)
		exited(t, actx)
		v := vol1(t, actx)
		v.Extra["intent"] = intent
		saveVolume(actx, v)
		f := unitNode(exitAnswer(75, time.Minute))
		if err := e.supervise(context.Background(), actx, f); err != nil {
			t.Fatal(err)
		}
		if f.issued("setsid") != (intent == intentHeld) {
			t.Errorf("intent %s: relaunched %v", intent, f.issued("setsid"))
		}
	}
}

// A fault loop's header carries the unit entry too; a loop without faults
// does not emulate the unit and says nothing.
func TestLoopRunHeaderSaysUnitEmulated(t *testing.T) {
	for _, faults := range []string{"", "0.3"} {
		f := &fakeProduct{}
		f.install(t)
		actx, p := genRunCtx(t)
		act := loopAct("10")
		if faults != "" {
			act.Params["faults"], act.Params["fault_processes"], act.Params["fault_restart_within"] = faults, "blockvolume:0", "3"
		}
		if _, err := v3LoopRun(context.Background(), actx, act); err != nil {
			t.Fatal(err)
		}
		_ = actx.History.Close()
		h := records(t, p)[0]
		u, has := h.Result["unit"].(map[string]any)
		if h.Verb != "loop_header" || has != (faults != "") || (has && (u["emulated"] != true || u["note"] != unitNotEvidence)) {
			t.Errorf("faults %q: header %+v", faults, h.Result)
		}
	}
}

// An UNSETTLED volume in neither consistent state is pending until its
// recovery deadline (product/checks.json: completion + regrant 65 s +
// start_wait 30 s; with a kill of its holder before, max(kill + regrant,
// completion) + start_wait), the third state only after it. Red control: a
// third state reported before the deadline.
func TestUnsettledIsPendingUntilTheRecoveryDeadline(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	old := nowFn
	nowFn = func() time.Time { return now }
	t.Cleanup(func() { nowFn = old })
	ago := func(s int) string { return strconv.FormatInt(now.Add(-time.Duration(s)*time.Second).UnixNano(), 10) }
	for name, c := range map[string]struct {
		end, kill string
		pending   bool
		left      float64
	}{
		"gone, lease granted, +10 s: pending":           {ago(10), "", true, 85},
		"gone, lease granted, +100 s: third state":      {ago(100), "", false, 0},
		"a kill 100 s before, op at -50 s: third state": {ago(50), ago(100), false, 0},
		"a kill 20 s before, op at -10 s: pending":      {ago(10), ago(20), true, 75},
		"a kill after the op does not count":            {ago(10), ago(5), true, 85},
	} {
		t.Run(name, func(t *testing.T) {
			actx, p, e := faultSetup(t)
			v := vol1(t, actx)
			v.Extra["intent"], v.Extra["unsettled_at"] = intentUnsettled, c.end
			if c.kill != "" {
				v.Extra["killed_at"] = c.kill
			}
			saveVolume(actx, v)
			f := healthNode()
			f.rules = append([]fakeRule{{match: "/proc/4242", answers: []fakeAnswer{ok("absent\n")}},
				{match: "ops workspace show", answers: []fakeAnswer{wsShow("granted", "none")}}}, f.rules...)
			if out, err := e.healthy(context.Background(), actx, f, 0); err != nil || out["healthy"] != "false" {
				t.Fatalf("%v %v", out, err)
			}
			r := healthyRecord(t, actx, p)
			third, _ := r["third_state"].([]any)
			pend, _ := r["unsettled_pending"].([]any)
			if c.pending {
				if len(third) != 0 || len(pend) != 1 {
					t.Fatalf("not pending: %v", r)
				}
				w := pend[0].(map[string]any)
				if w["volume"] != "v1" || w["why"] == "" || w["deadline_in_s"] != c.left {
					t.Errorf("pending %v, want deadline_in_s %v", w, c.left)
				}
			} else if len(third) != 1 || len(pend) != 0 {
				t.Errorf("not third state: %v", r)
			}
		})
	}
	// A kill of the holder records when it completed.
	actx, _, e := faultSetup(t)
	f := (&fakeNode{}).on("kill -KILL", ok("sent "+cmdline(holderArgv))).on("/proc/4242", ok("absent\n"))
	if out, _ := e.kill(context.Background(), actx, f, "blockvolume:0", time.Second); out["value"] != tr.OutcomeOK || vol1(t, actx).Extra["killed_at"] != strconv.FormatInt(now.UnixNano(), 10) {
		t.Errorf("kill completion not recorded: %v %+v", out, vol1(t, actx).Extra)
	}
	// An unknown lifecycle answer records its completion.
	v := &opVolume{Extra: map[string]string{}}
	_, _ = lifecycle(v, intentNotHeld)(map[string]string{"value": tr.OutcomeUnknown}, nil)
	if v.Extra["unsettled_at"] != strconv.FormatInt(now.UnixNano(), 10) {
		t.Errorf("completion not recorded: %+v", v.Extra)
	}
}
