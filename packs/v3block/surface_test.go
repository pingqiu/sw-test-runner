package v3block

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	tr "github.com/pingqiu/sw-test-runner"
)

// fakeNode answers each command by the first rule whose substring it holds,
// and records every command issued. A rule may answer differently on later
// calls (the last answer repeats).
type fakeNode struct {
	mu    sync.Mutex
	rules []fakeRule
	calls []string
}

type fakeRule struct {
	match   string
	answers []fakeAnswer
	n       int
}

type fakeAnswer struct {
	out, errOut string
	code        int
}

func (f *fakeNode) on(match string, answers ...fakeAnswer) *fakeNode {
	f.rules = append(f.rules, fakeRule{match: match, answers: answers})
	return f
}

func (f *fakeNode) Run(_ context.Context, cmd string) (string, string, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, cmd)
	for i := range f.rules {
		r := &f.rules[i]
		if strings.Contains(cmd, r.match) {
			a := r.answers[min(r.n, len(r.answers)-1)]
			r.n++
			return a.out, a.errOut, a.code, nil
		}
	}
	return "", "no rule", 127, nil
}

func (f *fakeNode) RunRoot(ctx context.Context, cmd string) (string, string, int, error) {
	return f.Run(ctx, cmd)
}

func (f *fakeNode) issued(sub string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if strings.Contains(c, sub) {
			return true
		}
	}
	return false
}

func ok(out string) fakeAnswer { return fakeAnswer{out: out} }

const servingV1 = `{"VolumeID":"v1","Epoch":1,"Healthy":true,"AuthorityRole":"primary","FrontendPrimaryReady":true}`

func testVolume() (*opVolume, vsHost, opEnv) {
	h := vsHost{Server: "a-v1", Data: "127.0.0.1:21010", Ctrl: "127.0.0.1:21011", Status: "127.0.0.1:21012", NVMe: "127.0.0.1:21013"}
	return &opVolume{Name: "v1", NQN: "nqn.2026-11.io.seaweedfs:op-run-1-v1", LayoutID: strings.Repeat("ab", 16), Hosts: []vsHost{h}}, h, opEnv{bin: "/b", root: "/r", master: "m", filer: "f:29588", deadline: time.Minute}
}

// A listener already on one of the volume's endpoints (another run's, or a
// leftover) refuses the start: nothing is launched and nothing connected.
func TestStartHolderRefusesAnOccupiedEndpoint(t *testing.T) {
	v, _, e := testVolume()
	f := (&fakeNode{}).on("ss -Hltnp", ok("LISTEN 0 4096 127.0.0.1:21012 0.0.0.0:* users:((\"blockvolume\",pid=777,fd=9))"))
	out, res := attachOn(t, e, f, v, 64, time.Second)
	if out != tr.OutcomeUnknown || !strings.Contains(res["detail"].(string), "already listens") {
		t.Fatalf("%s %v", out, res)
	}
	for _, c := range []string{"setsid", "nvme-fabrics", "dd "} {
		if f.issued(c) {
			t.Errorf("%q was issued after the endpoint check refused", c)
		}
	}
}

// The new child dies while another healthy service answers the status port:
// readiness is never credited, nothing is connected.
func TestStartHolderDoesNotCreditAnotherServiceWhenItsChildDied(t *testing.T) {
	v, _, e := testVolume()
	f := (&fakeNode{}).
		on("ss -Hltnp", ok(""), ok("LISTEN 0 4096 127.0.0.1:21012 0.0.0.0:* users:((\"blockvolume\",pid=777,fd=9))")).
		on("mkdir -p", ok("")).
		on("setsid", ok("4242\n")).
		on("/proc/4242", ok("start 100\n"), ok("absent\n")).
		on("/status?volume=v1", ok(servingV1))
	out, res := attachOn(t, e, f, v, 64, 2*time.Second)
	if out != tr.OutcomeUnknown || res["detail"] != "blockvolume exited before serving" {
		t.Fatalf("%s %v", out, res)
	}
	if f.issued("nvme-fabrics") {
		t.Error("a dead child's run connected the kernel host")
	}
}

// The child runs and the status serves, but the listeners belong to another
// process: still not ready, nothing connected.
func TestStartHolderRequiresTheChildToHoldItsListeners(t *testing.T) {
	v, _, e := testVolume()
	f := (&fakeNode{}).
		on("ss -Hltnp", ok(""), ok("LISTEN 0 4096 127.0.0.1:21012 0.0.0.0:* users:((\"other\",pid=777,fd=9))\nLISTEN 0 4096 127.0.0.1:21013 0.0.0.0:* users:((\"other\",pid=777,fd=10))")).
		on("mkdir -p", ok("")).
		on("setsid", ok("4242\n")).
		on("/proc/4242", ok("start 100\n")).
		on("/status?volume=v1", ok(servingV1))
	out, res := attachOn(t, e, f, v, 64, 600*time.Millisecond)
	if out != tr.OutcomeUnknown || !strings.Contains(res["detail"].(string), "own listeners") {
		t.Fatalf("%s %v", out, res)
	}
	if f.issued("nvme-fabrics") {
		t.Error("connected although the listeners are another process's")
	}
}

// The good path, for contrast: its own listeners, then the connect and one
// device.
func TestStartHolderReadyFromItsOwnListeners(t *testing.T) {
	v, _, e := testVolume()
	own := "LISTEN 0 4096 127.0.0.1:21012 0.0.0.0:* users:((\"blockvolume\",pid=4242,fd=9))\nLISTEN 0 4096 127.0.0.1:21013 0.0.0.0:* users:((\"blockvolume\",pid=4242,fd=10))"
	f := (&fakeNode{}).
		on("ss -Hltnp", ok(""), ok(own)).
		on("mkdir -p", ok("")).
		on("setsid", ok("4242\n")).
		on("/proc/4242", ok("start 100\n")).
		on("/status?volume=v1", ok(servingV1)).
		on("nvme-fabrics", ok("")).
		on("subsysnqn", ok("ctrl nvme3\nns nvme3c3n1\nend\n"))
	out, res := attachOn(t, e, f, v, 64, 2*time.Second)
	if out != tr.OutcomeOK || res["device"] != "/dev/nvme3n1" || v.PID != "4242" || v.PIDStart != "100" {
		t.Fatalf("%s %v %+v", out, res, v)
	}
}

func TestNVMeScanKeepsAbsentUnreadableAndPresentApart(t *testing.T) {
	ctx := context.Background()
	for name, c := range map[string]struct {
		a    fakeAnswer
		want string
	}{
		"complete, no controller": {ok("end\n"), nvmeAbsent},
		"one device":              {ok("ctrl nvme3\nns nvme3c3n1\nend\n"), nvmeOne},
		"controller, no ns yet":   {ok("ctrl nvme3\nend\n"), nvmeIncomplete},
		"two namespaces":          {ok("ctrl nvme3\nns nvme3c3n1\nns nvme3c3n2\nend\n"), nvmeIncomplete},
		"read error":              {fakeAnswer{errOut: "cat: Permission denied", code: 3}, nvmeUnreadable},
		"partial output":          {ok("ctrl nvme3\n"), nvmeUnreadable},
		"garbage":                 {ok("x\nend\n"), nvmeUnreadable},
	} {
		f := (&fakeNode{}).on("subsysnqn", c.a)
		if got, _, _ := nvmeScan(ctx, f, "nqn.x"); got != c.want {
			t.Errorf("%s: %s, want %s", name, got, c.want)
		}
	}
	// An unreadable scan refuses I/O at once; it is not "no device yet".
	f := (&fakeNode{}).on("subsysnqn", fakeAnswer{errOut: "boom", code: 3})
	if dev, why := deviceOf(ctx, f, "nqn.x", time.Second); dev != "" || !strings.Contains(why, "scan") {
		t.Errorf("unreadable scan gave %q %q", dev, why)
	}
}

func TestDisconnectIsCertifiedOnlyByAReadableAbsence(t *testing.T) {
	ctx := context.Background()
	// The delete fails: not disconnected.
	f := (&fakeNode{}).on("delete_controller", fakeAnswer{errOut: "write error", code: 4})
	if why := nvmeDisconnect(ctx, f, "nqn.x"); why == "" {
		t.Error("a failed delete certified the disconnect")
	}
	// The delete runs, the confirming scan cannot read sysfs: not disconnected.
	f = (&fakeNode{}).on("delete_controller", ok("")).on("subsysnqn", fakeAnswer{errOut: "boom", code: 3})
	if why := nvmeDisconnect(ctx, f, "nqn.x"); !strings.Contains(why, "cannot confirm") {
		t.Errorf("an unreadable confirmation gave %q", why)
	}
	// A complete scan finds none: disconnected.
	f = (&fakeNode{}).on("delete_controller", ok("")).on("subsysnqn", ok("end\n"))
	if why := nvmeDisconnect(ctx, f, "nqn.x"); why != "" {
		t.Errorf("a clean disconnect gave %q", why)
	}
}

func TestProcessIdentityAndExit(t *testing.T) {
	ctx := context.Background()
	f := (&fakeNode{}).on("/proc/10", ok("start 555\n"))
	if procState(ctx, f, "10", "555") != procRunning {
		t.Error("the recorded process is not running")
	}
	if procState(ctx, f, "10", "444") != procGone {
		t.Error("a recycled pid (another start time) counts as the recorded process")
	}
	if procState(ctx, f, "10", "") != procUnreadable {
		t.Error("a process without a recorded start time has an identity")
	}
	f = (&fakeNode{}).on("/proc/10", fakeAnswer{errOut: "boom", code: 3})
	if why := waitGone(ctx, f, "10", "555", 500*time.Millisecond); why == "" || !strings.Contains(why, "could not be read") {
		t.Errorf("an unreadable check was taken for an exit: %q", why)
	}
	f = (&fakeNode{}).on("/proc/10", ok("absent\n"))
	if why := waitGone(ctx, f, "10", "555", time.Second); why != "" {
		t.Errorf("a gone process: %q", why)
	}
}
