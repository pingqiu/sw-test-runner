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
	defer func(d, p time.Duration) { deviceDeadlineOverride, devicePoll = d, p }(deviceDeadlineOverride, devicePoll)
	deviceDeadlineOverride, devicePoll = 200*time.Millisecond, 5*time.Millisecond
	for _, unkillable := range []bool{false, true} {
		dev := (&fakeNode{}).on("subsysnqn", ok(oneDevice)).on("skip=1 ", ok(strings.Repeat("a", 64)+"\n"))
		host := &declared.SimHost{Inner: stuckOn{match: "skip=0 ", inner: dev}, Unkillable: unkillable}
		actx, p := deadlineCtx(t, host)
		start := time.Now()
		out, err := v3OpRead(context.Background(), actx, tr.Action{Params: map[string]string{"volume": "v1", "lbas": "0 1"}})
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
		"stop: the guest disconnect hangs": {v3OpStop, map[string]string{"volume": "v1"}, "delete_controller"},
		"attach: the listener check hangs": {v3OpAttach, map[string]string{"volume": "v1"}, "ss -Hltnp"},
		"flush: the device scan hangs":     {v3OpFlush, map[string]string{"volume": "v1"}, "subsysnqn"},
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
		if len(rs) != 3 || rs[1].Result["detail"] != "timeout after 0s" {
			t.Errorf("%s: records %+v", name, rs)
		}
	}
}

// missingDeadlines lists the typed operations (v3_op_*) among names that
// have no runner deadline in the description.
func missingDeadlines(names []string) []string {
	var out []string
	for _, n := range names {
		if !strings.HasPrefix(n, "v3_op_") || n == "v3_op_spec" {
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
	}
	// The device deadline is the runner's, below the kernel's default
	// ctrl_loss_tmo (600 s), and inside the deadlines of its operations.
	dd := time.Duration(guest.DeviceIODeadlineSecs) * time.Second
	if dd <= 0 || dd >= 600*time.Second {
		t.Fatalf("device I/O runner deadline %s", dd)
	}
	for _, n := range []string{"v3_op_read", "v3_op_write", "v3_op_flush"} {
		if d, _ := runnerDeadline(n, nil); d <= dd {
			t.Errorf("%s: deadline %s does not leave room for the device deadline %s", n, d, dd)
		}
	}
}

// The guest connect: by default the product's plain connect (no timeout
// option); a declared variant adds its options in the declared order and is
// named as a variant in the run header and in every attach.
func TestGuestConnect(t *testing.T) {
	plain, err := selectConnect("")
	if err != nil || plain.Options != "" || !strings.HasPrefix(plain.Label, "plain") {
		t.Fatalf("default: %+v %v", plain, err)
	}
	f := (&fakeNode{}).on("nvme-fabrics", ok(""))
	if why := nvmeConnect(context.Background(), f, "127.0.0.1", "4420", "nqn.x:v1", plain.Options); why != "" || len(f.calls) != 1 ||
		f.calls[0] != "printf %s transport=tcp,traddr=127.0.0.1,trsvcid=4420,nqn=nqn.x:v1 > /dev/nvme-fabrics" {
		t.Fatalf("plain connect: %q %q", why, f.calls)
	}
	v, err := selectConnect("fast_faults")
	if err != nil || v.Options != ",fast_io_fail_tmo=5,ctrl_loss_tmo=60" || v.Label != "guest connect variant: fast_io_fail_tmo=5,ctrl_loss_tmo=60" {
		t.Fatalf("variant: %+v %v", v, err)
	}
	f = (&fakeNode{}).on("nvme-fabrics", ok(""))
	if why := nvmeConnect(context.Background(), f, "127.0.0.1", "4420", "nqn.x:v1", v.Options); why != "" ||
		!strings.Contains(f.calls[0], "nqn=nqn.x:v1,fast_io_fail_tmo=5,ctrl_loss_tmo=60 > /dev/nvme-fabrics") {
		t.Fatalf("variant connect: %q %q", why, f.calls)
	}
	if _, err := selectConnect("nope"); err == nil {
		t.Fatal("an undeclared connect was accepted")
	}
	h, err := guestHeader(func(k string) string { return map[string]string{guest.Connect.SelectVar: "fast_faults"}[k] })
	if err != nil || h["guest_connect"] != v.Label {
		t.Fatalf("variant header: %v %v", h, err)
	}
	if h, err := guestHeader(func(string) string { return "" }); err != nil || len(h) != 0 {
		t.Fatalf("plain header: %v %v", h, err)
	}
	if why := nvmeConnect(context.Background(), &fakeNode{}, "127.0.0.1", "4420", "nqn.x:v1", ",x=1;reboot"); why == "" {
		t.Fatal("malformed options were accepted")
	}
}
