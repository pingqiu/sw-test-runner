package v3block

// Runner deadlines: a run that cannot end is the runner's defect, whatever
// the product does. Every registered typed operation has a deadline; at it
// the operation completes unknown and the run moves on; a child that
// survives the kill is recorded.

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/declared"
)

// stuckOn never answers a command holding match; others go to inner.
type stuckOn struct {
	match string
	inner runner
}

func (s stuckOn) Run(ctx context.Context, cmd string) (string, string, int, error) {
	if strings.Contains(cmd, s.match) {
		select {}
	}
	return s.inner.Run(ctx, cmd)
}

func (s stuckOn) RunRoot(ctx context.Context, cmd string) (string, string, int, error) {
	if strings.Contains(cmd, s.match) {
		select {}
	}
	return s.inner.RunRoot(ctx, cmd)
}

func deadlineCtx(t *testing.T, node runner) (*tr.ActionContext, string) {
	t.Helper()
	old := opNode
	opNode = func(*tr.ActionContext, string) (runner, error) { return node, nil }
	t.Cleanup(func() { opNode = old })
	p := filepath.Join(t.TempDir(), "history.jsonl")
	h, err := tr.OpenHistory(p)
	if err != nil {
		t.Fatal(err)
	}
	actx := &tr.ActionContext{History: h, Vars: eqBaseVars(), Log: func(string, ...interface{}) {}}
	vs := eqVolumes()
	vs[0].PID, vs[0].PIDStart = "4242", "999"
	for _, v := range vs {
		saveVolume(actx, v)
	}
	return actx, p
}

// A read whose dd never answers (a dead device under the plain connect):
// unknown at the device deadline, the run continues with the next lba, and
// with a child that survives the kill, its pid and start time are recorded.
func TestDeviceIOEndsAtItsRunnerDeadline(t *testing.T) {
	defer func(d func(time.Duration) time.Duration, p time.Duration) {
		blockActions.Deadline, blockActions.Poll = d, p
	}(blockActions.Deadline, blockActions.Poll)
	blockActions.Deadline, blockActions.Poll = func(time.Duration) time.Duration { return 200 * time.Millisecond }, 5*time.Millisecond
	for _, unkillable := range []bool{false, true} {
		dev := (&fakeNode{}).on("subsysnqn", ok(oneDevice)).on("skip=1 ", ok(strings.Repeat("a", 64)+"\n"))
		host := &declared.SimHost{Inner: stuckOn{match: "skip=0 ", inner: dev}, Unkillable: unkillable}
		actx, p := deadlineCtx(t, host)
		start := time.Now()
		out, err := blockActions.Handler("v3_op_read")(context.Background(), actx, tr.Action{Params: map[string]string{"volume": "v1", "lbas": "0 1"}})
		if err != nil || time.Since(start) > 10*time.Second {
			t.Fatalf("unkillable=%v: %v %v after %s", unkillable, out, err, time.Since(start))
		}
		_ = actx.History.Close()
		rs := historyRecords(t, p)
		if len(rs) != 5 || rs[1].Outcome != tr.OutcomeUnknown || rs[3].Outcome != tr.OutcomeOK {
			t.Fatalf("unkillable=%v: records %+v", unkillable, rs)
		}
		if d, _ := rs[1].Result["detail"].(string); !strings.HasPrefix(d, "timeout after ") {
			t.Fatalf("detail %q", d)
		}
		b, _ := json.Marshal(rs[1].Result["abandoned"])
		if got := string(b); unkillable != strings.Contains(got, `"pid":"40002","start":"777"`) {
			t.Fatalf("unkillable=%v: abandoned %s", unkillable, got)
		}
	}
}

// A Go handler whose command never answers at all (the node itself hangs):
// the operation completes unknown at the handler's runner deadline.
func TestExceptionEndsAtItsRunnerDeadline(t *testing.T) {
	defer func(d time.Duration) { exceptionDeadlineOverride = d }(exceptionDeadlineOverride)
	exceptionDeadlineOverride = 300 * time.Millisecond
	for name, c := range map[string]struct {
		h      tr.ActionHandlerFunc
		params map[string]string
		stuck  string
	}{
		"stop: the guest disconnect hangs": {blockActions.Handler("v3_op_stop"), map[string]string{"volume": "v1"}, "delete_controller"},
		"attach: the listener check hangs": {blockActions.Handler("v3_op_attach"), map[string]string{"volume": "v1"}, "ss -Hltnp"},
		"flush: the device scan hangs":     {blockActions.Handler("v3_op_flush"), map[string]string{"volume": "v1"}, "subsysnqn"},
	} {
		actx, p := deadlineCtx(t, stuckOn{match: c.stuck, inner: &fakeNode{}})
		start := time.Now()
		out, err := c.h(context.Background(), actx, tr.Action{Params: c.params})
		if err != nil || out["value"] != tr.OutcomeUnknown || time.Since(start) > 5*time.Second {
			t.Errorf("%s: %v %v after %s", name, out, err, time.Since(start))
			continue
		}
		_ = actx.History.Close()
		rs := historyRecords(t, p)
		var done *tr.HistoryRecord
		for i := range rs {
			if rs[i].Kind == tr.HistoryComplete {
				done = &rs[i]
			}
		}
		if done == nil || done.Result["detail"] != "timeout after 0s" {
			t.Errorf("%s: records %+v", name, rs)
		}
	}
}

// missingDeadlines lists the typed operations (v3_op_*) among names that
// have no runner deadline in the description.
func missingDeadlines(names []string) []string {
	var out []string
	for _, n := range names {
		if !strings.HasPrefix(n, "v3_op_") || n == "v3_op_spec" { // v3_op_spec: runner setup
			continue
		}
		if _, err := runnerDeadline(n, nil); err != nil {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// Every registered typed operation, declared or a Go handler, has a runner
// deadline. Red control: a handler registered without one is caught.
func TestEveryTypedOperationHasARunnerDeadline(t *testing.T) {
	r := tr.NewRegistry()
	RegisterPack(r)
	if m := missingDeadlines(r.List()); len(m) != 0 {
		t.Fatalf("no runner deadline: %v", m)
	}
	if m := missingDeadlines(append(r.List(), "v3_op_new_handler")); len(m) != 1 || m[0] != "v3_op_new_handler" {
		t.Fatalf("red control: %v", m)
	}
	for _, x := range blockActions.File.Exceptions {
		if x.RunnerDeadlineSeconds <= 0 {
			t.Errorf("%s: no runner deadline", x.Action)
		}
	}
	for _, a := range blockActions.File.Actions {
		for _, v := range a.Variants {
			if v.RunnerDeadlineSeconds <= 0 {
				t.Errorf("%s: a variant without a runner deadline", a.Action)
			}
		}
		for _, st := range a.Steps {
			if st.RunnerDeadlineSeconds <= 0 {
				t.Errorf("%s: a step without a runner deadline", a.Action)
			}
		}
	}
	// Each device I/O deadline is the runner's, below the kernel's default
	// ctrl_loss_tmo (600 s), and inside the deadline of its operation.
	for _, n := range []string{"v3_op_read", "v3_op_write", "v3_op_flush"} {
		var io *declared.IO
		for _, a := range blockActions.File.Actions {
			if a.Action == n {
				io = a.IO
			}
		}
		if io == nil {
			t.Fatalf("%s: not a declared device action", n)
		}
		dd := time.Duration(io.RunnerDeadlineSeconds) * time.Second
		if dd <= 0 || dd >= 600*time.Second {
			t.Fatalf("%s: device I/O runner deadline %s", n, dd)
		}
		if d, _ := runnerDeadline(n, nil); d <= dd {
			t.Errorf("%s: deadline %s does not leave room for the device deadline %s", n, d, dd)
		}
	}
}

// The guest connect: empty is the product's plain connect, today's exact
// string; a declared variant's options are appended under the kernel's
// option names in the declared order and named as a variant in the header
// The guest connect: empty is the documented connect (release evidence),
// with the page's options; csi_plain is today's plain string; a declared
// variant appends its options in the declared order and is named as a
// variant in the header; an undeclared name is refused.
func TestGuestConnect(t *testing.T) {
	const plainCmd = "printf %s transport=tcp,traddr=127.0.0.1,trsvcid=4420,nqn=nqn.x:v1 > /dev/nvme-fabrics"
	doc, err := selectConnect("")
	if err != nil || !doc.Evidence || doc.Name != "documented" || doc.Options != ",ctrl_loss_tmo=180,reconnect_delay=2" ||
		doc.Declared["ctrl_loss_tmo"] != 180 || doc.Declared["reconnect_delay"] != 2 || len(doc.Declared) != 2 {
		t.Fatalf("default: %+v %v", doc, err)
	}
	f := (&fakeNode{}).on("nvme-fabrics", ok(""))
	if why := nvmeConnect(context.Background(), f, "127.0.0.1", "4420", "nqn.x:v1", doc.Options); why != "" ||
		f.calls[0] != "printf %s transport=tcp,traddr=127.0.0.1,trsvcid=4420,nqn=nqn.x:v1,ctrl_loss_tmo=180,reconnect_delay=2 > /dev/nvme-fabrics" {
		t.Fatalf("documented connect: %q %q", why, f.calls)
	}
	plain, err := selectConnect("csi_plain")
	if err != nil || plain.Evidence || plain.Options != "" || plain.Label != "guest connect variant: plain" {
		t.Fatalf("csi_plain: %+v %v", plain, err)
	}
	f = (&fakeNode{}).on("nvme-fabrics", ok(""))
	if why := nvmeConnect(context.Background(), f, "127.0.0.1", "4420", "nqn.x:v1", plain.Options); why != "" || f.calls[0] != plainCmd {
		t.Fatalf("plain connect: %q %q", why, f.calls)
	}
	if ff, err := selectConnect("fast_faults"); err != nil || ff.Evidence || ff.Options != ",fast_io_fail_tmo=5,ctrl_loss_tmo=60" {
		t.Fatalf("fast_faults: %+v %v", ff, err)
	}
	for _, bad := range []string{"ctrl-loss-tmo=180", "host_connect_settings", "plain"} {
		if _, err := selectConnect(bad); err == nil {
			t.Errorf("%q was accepted: a connect is declared", bad)
		}
	}
	h, err := guestHeader(func(k string) string { return map[string]string{guest.Connect.SelectVar: "csi_plain"}[k] })
	if err != nil || h["guest_connect"] != plain.Label {
		t.Fatalf("variant header: %v %v", h, err)
	}
	if h, err := guestHeader(func(string) string { return "" }); err != nil || len(h) != 0 {
		t.Fatalf("documented header: %v %v", h, err)
	}
	if why := nvmeConnect(context.Background(), &fakeNode{}, "127.0.0.1", "4420", "nqn.x:v1", ",x=1;reboot"); why == "" {
		t.Fatal("malformed options were accepted")
	}
}

// A variant row's options are checked at load: - or _ spellings name the
// same kernel option; an unknown name, a non-integer and a repeat are
// refused.
func TestVariantOptionsAtLoad(t *testing.T) {
	opts := guest.Connect.Options
	got, err := variantOptions(opts, json.RawMessage(`{"ctrl-loss-tmo": 180, "reconnect_delay": 2, "keep-alive-tmo": -1}`))
	if err != nil || got["ctrl_loss_tmo"] != 180 || got["reconnect_delay"] != 2 || got["keep_alive_tmo"] != -1 || len(got) != 3 {
		t.Fatalf("%v %v", got, err)
	}
	for _, bad := range []string{
		`{"ctrl_loss": 180}`, `{"nr_io_queues": 4}`, `{"ctrl_loss_tmo": "180"}`, `{"ctrl_loss_tmo": 1.5}`,
		`{"ctrl_loss_tmo": 1, "ctrl-loss-tmo": 2}`, `{"ctrl_loss_tmo": 1, "ctrl_loss_tmo": 2}`, `[1]`,
	} {
		if _, err := variantOptions(opts, json.RawMessage(bad)); err == nil {
			t.Errorf("%s was accepted", bad)
		}
	}
}

// The attach's history record names the connect in effect, from the run's
// declared variant; an undeclared name refuses the step before anything
// sysfsAnswer is the read-back of one controller with these values.
func sysfsAnswer(loss, delay string) fakeAnswer {
	return ok("ctrl nvme3\nctrl_loss_tmo " + loss + "\nreconnect_delay " + delay + "\nfast_io_fail_tmo off\nkato 5\nend\n")
}

// attachNode answers a whole attach of v1: free endpoints, the child, its
// own listeners, serving, the connect, one device, and the read-back.
func attachNode(readback fakeAnswer) *fakeNode {
	own := "LISTEN 0 4096 127.0.0.1:20012 0.0.0.0:* users:((\"blockvolume\",pid=4242,fd=9))\nLISTEN 0 4096 127.0.0.1:20013 0.0.0.0:* users:((\"blockvolume\",pid=4242,fd=10))"
	return (&fakeNode{}).
		on("fast_io_fail_tmo kato", readback).
		on("ss -Hltnp", ok(""), ok(own)).
		on("mkdir -p", ok("")).
		on("setsid", ok("4242\n")).
		on("/proc/4242", ok("start 100\n")).
		on("/status?volume=v1", ok(`{"VolumeID":"v1","Epoch":1,"Healthy":true,"AuthorityRole":"primary","FrontendPrimaryReady":true}`)).
		on("nvme-fabrics", ok("")).
		on("subsysnqn", ok(oneDevice))
}

// The attach records the connect in effect and the kernel's read-back. A
// read-back equal to the declared values is recorded (attach ok, one
// guest_connect record); one that differs fails the run at that attach,
// before any other operation; a variant without declared values only
// records; an undeclared name refuses the step before anything runs.
func TestAttachReadsTheConnectBack(t *testing.T) {
	defer func(d time.Duration) { exceptionDeadlineOverride = d }(exceptionDeadlineOverride)
	exceptionDeadlineOverride = 5 * time.Second
	attach := func(variant string, readback fakeAnswer) (error, []tr.HistoryRecord) {
		actx, p := deadlineCtx(t, attachNode(readback))
		vs := eqVolumes()
		vs[0].PID, vs[0].PIDStart = "", ""
		saveVolume(actx, vs[0])
		if variant != "" {
			actx.Vars["guest_connect_variant"] = variant
		}
		_, err := blockActions.Handler("v3_op_attach")(context.Background(), actx, tr.Action{Params: map[string]string{"volume": "v1"}})
		_ = actx.History.Close()
		return err, historyRecords(t, p)
	}
	byVerb := func(rs []tr.HistoryRecord, kind, verb string) []tr.HistoryRecord {
		var out []tr.HistoryRecord
		for _, r := range rs {
			if r.Kind == kind && r.Verb == verb {
				out = append(out, r)
			}
		}
		return out
	}

	err, rs := attach("", sysfsAnswer("180", "2"))
	done := byVerb(rs, tr.HistoryComplete, "attach")
	gc := byVerb(rs, tr.HistoryObserve, "guest_connect")
	if err != nil || len(done) != 1 || done[0].Outcome != tr.OutcomeOK || len(gc) != 1 || gc[0].Result["match"] != true || gc[0].Result["evidence"] != "true" {
		t.Fatalf("a match: %v, records %+v", err, rs)
	}
	eff, _ := done[0].Result["guest_connect_effective"].(map[string]any)
	vals, _ := eff["values"].(map[string]any)
	if eff["ctrl"] != "nvme3" || vals["ctrl_loss_tmo"] != "180" || vals["reconnect_delay"] != "2" || vals["fast_io_fail_tmo"] != "off" || vals["keep_alive_tmo"] != "5" {
		t.Fatalf("the read-back recorded: %v", eff)
	}
	if a := byVerb(rs, tr.HistoryInvoke, "attach"); a[0].Args["guest_connect"] != "documented connect (standalone, evidence): ctrl_loss_tmo=180,reconnect_delay=2" {
		t.Fatalf("the attach names %v", a[0].Args["guest_connect"])
	}

	// Red controls: the kernel holds another value, or cannot show one.
	for name, rb := range map[string]fakeAnswer{
		"the kernel's default loss timeout": sysfsAnswer("600", "2"),
		"another reconnect delay":           sysfsAnswer("180", "10"),
		"no readback file":                  ok("ctrl nvme3\nctrl_loss_tmo -\nreconnect_delay 2\nfast_io_fail_tmo -\nkato -\nend\n"),
		"the read-back fails":               {errOut: "cat: permission denied", code: 3},
	} {
		err, rs := attach("", rb)
		done := byVerb(rs, tr.HistoryComplete, "attach")
		gc := byVerb(rs, tr.HistoryObserve, "guest_connect")
		if err == nil || !strings.Contains(err.Error(), "the run stops before any other operation") || len(done) != 1 || done[0].Outcome != tr.OutcomeUnknown ||
			len(gc) != 1 || gc[0].Result["match"] != false {
			t.Errorf("%s: %v, records %+v", name, err, rs)
		}
	}

	// csi_plain declares nothing: the read-back is recorded, not compared.
	if err, rs := attach("csi_plain", sysfsAnswer("600", "10")); err != nil || byVerb(rs, tr.HistoryComplete, "attach")[0].Outcome != tr.OutcomeOK {
		t.Fatalf("csi_plain: %v %+v", err, rs)
	}
	// An undeclared name refuses the step before anything runs.
	actx, _ := deadlineCtx(t, &fakeNode{})
	actx.Vars["guest_connect_variant"] = "ctrl-loss-tmo=1"
	if _, err := blockActions.Handler("v3_op_attach")(context.Background(), actx, tr.Action{Params: map[string]string{"volume": "v1"}}); err == nil || actx.History.Open() != 0 {
		t.Fatalf("an undeclared connect was not refused: %v", err)
	}
	_ = actx.History.Close()
}

// A device read abandoned at its deadline is watched: still alive at the
// run's end (v3_history_end), it is recorded child_exit unknown, alive
// true, naming the read's invoke seq.
func TestAbandonedDeviceIOIsWatchedToTheEnd(t *testing.T) {
	defer func(d func(time.Duration) time.Duration, p time.Duration) {
		blockActions.Deadline, blockActions.Poll = d, p
	}(blockActions.Deadline, blockActions.Poll)
	blockActions.Deadline, blockActions.Poll = func(time.Duration) time.Duration { return 200 * time.Millisecond }, 5*time.Millisecond
	dev := (&fakeNode{}).on("subsysnqn", ok(oneDevice))
	host := &declared.SimHost{Inner: stuckOn{match: "skip=0 ", inner: dev}, Unkillable: true}
	actx, p := deadlineCtx(t, host)
	if _, err := blockActions.Handler("v3_op_read")(context.Background(), actx, tr.Action{Params: map[string]string{"volume": "v1", "lbas": "0"}}); err != nil {
		t.Fatal(err)
	}
	if declared.Watched(actx.History) != 1 {
		t.Fatalf("%d watched, want 1", declared.Watched(actx.History))
	}
	if _, err := v3HistoryEnd(context.Background(), actx, tr.Action{}); err != nil {
		t.Fatal(err)
	}
	var ce []tr.HistoryRecord
	for _, r := range historyRecords(t, p) {
		if r.Verb == "child_exit" {
			ce = append(ce, r)
		}
	}
	if len(ce) != 1 || ce[0].Result["outcome"] != tr.OutcomeUnknown || ce[0].Result["alive"] != true || ce[0].Result["op_seq"] != float64(1) || ce[0].Volume != "v1" {
		t.Fatalf("child_exit %+v", ce)
	}
}

// Every disconnect the runner performs inside an operation (the planned
// stop's and the move's hand-over) writes an observe record "disconnect".
func TestDisconnectIsRecorded(t *testing.T) {
	for name, h := range map[string]tr.ActionHandlerFunc{"stop": blockActions.Handler("v3_op_stop"), "move": blockActions.Handler("v3_op_move")} {
		f := (&fakeNode{}).on("delete_controller", ok("deleted 1\n")).on("subsysnqn", ok("end\n"))
		actx, p := deadlineCtx(t, f)
		if _, err := h(context.Background(), actx, tr.Action{Params: map[string]string{"volume": "v1"}}); err != nil {
			t.Fatal(err)
		}
		_ = actx.History.Close()
		var got []tr.HistoryRecord
		for _, r := range historyRecords(t, p) {
			if r.Verb == "disconnect" {
				got = append(got, r)
			}
		}
		if len(got) != 1 || got[0].Client != "runner" || got[0].Volume != "v1" || got[0].Result["ok"] != true ||
			got[0].Result["controllers"] != float64(1) || got[0].Result["nqn"] != "nqn.2026-11.io.seaweedfs:op-v1" {
			t.Fatalf("%s: disconnect records %+v", name, got)
		}
	}
	// A failed disconnect is recorded too, not ok.
	f := (&fakeNode{}).on("delete_controller", fakeAnswer{errOut: "permission denied", code: 4})
	actx, p := deadlineCtx(t, f)
	_, _ = blockActions.Handler("v3_op_stop")(context.Background(), actx, tr.Action{Params: map[string]string{"volume": "v1"}})
	_ = actx.History.Close()
	n := 0
	for _, r := range historyRecords(t, p) {
		if r.Verb == "disconnect" {
			n++
			if r.Result["ok"] != false || r.Result["controllers"] != nil {
				t.Fatalf("failed disconnect %+v", r)
			}
		}
	}
	if n != 1 {
		t.Fatalf("%d disconnect records for the failed one", n)
	}
}
