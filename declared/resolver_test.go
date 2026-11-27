package declared

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A resolver that is not NVMe: the executor runs whatever the product
// declares. It lists "dev <name>" lines of a loop-device table.
const loopResolver = `{"loop": {"cite": "x.go:1",
 "scan": {"command": ["looptab", "{{subsystem}}"], "end": "done", "lines": {"dev": "^dev (loop[0-9]+)$", "note": "^note .*$"},
   "absent": {"dev": 0}, "one": {"dev": 1}, "device_line": "dev", "device": "/dev/{{dev.1}}",
   "unreadable": "looptab exit {{exit}}: {{stderr_first_line}}", "bad_line": "looptab printed {{line}}",
   "incomplete": "{{subsystem}}: {{count.dev}} devices", "no_device": "no device of {{subsystem}}",
   "no_device_by_deadline": "no device of {{subsystem}} by the deadline", "every_ms": 10},
 "connect": {"command": ["loopattach", "{{subsystem}}", "{{host}}", "{{port}}{{options}}"],
   "require": [{"value": "{{port}}", "pattern": "^[0-9]+$", "failure": "bad port {{port}}"}], "failure": "attach: {{stderr_first_line}}"},
 "disconnect": {"command": ["loopdetach", "{{subsystem}}"], "count_pattern": "^removed ([0-9]+)$", "failure": "detach exit {{exit}}",
   "confirm_within_seconds": 1, "confirm_every_ms": 10, "not_confirmed": "unconfirmed: {{why}}",
   "remains": "{{subsystem}} remains", "remains_by_deadline": "{{subsystem}} remains at the deadline"}}}`

func loopExecutor(t *testing.T) *Executor {
	t.Helper()
	f, err := Parse([]byte(head0 + `"device_resolvers": ` + loopResolver + `, "actions": []}`))
	if err != nil {
		t.Fatal(err)
	}
	return &Executor{File: f}
}

// head0 is the test header without its resolvers.
var head0 = head[:strings.Index(head, `"device_resolvers"`)]

func TestResolverIsTheProducts(t *testing.T) {
	x := loopExecutor(t)
	ctx := context.Background()
	for name, c := range map[string]struct {
		a           answer
		state, want string
	}{
		"one":                 {answer{out: "note x\ndev loop3\ndone\n"}, DeviceOne, "/dev/loop3"},
		"none":                {answer{out: "done\n"}, DeviceAbsent, ""},
		"two":                 {answer{out: "dev loop3\ndev loop4\ndone\n"}, DeviceIncomplete, "s1: 2 devices"},
		"not complete":        {answer{out: "dev loop3\n"}, DeviceUnreadable, "looptab exit 0: "},
		"a line of no kind":   {answer{out: "dev /x\ndone\n"}, DeviceUnreadable, "looptab printed dev /x"},
		"the command failing": {answer{errOut: "denied", code: 2}, DeviceUnreadable, "looptab exit 2: denied"},
	} {
		n := &ruleNode{}
		n.on("looptab", c.a)
		state, dev, why := x.DeviceScan(ctx, n, "loop", "s1")
		got := dev
		if state != DeviceOne {
			got = why
		}
		if state != c.state || got != c.want || n.calls[0] != "looptab s1" {
			t.Errorf("%s: %s %q %q, calls %q", name, state, dev, why, n.calls)
		}
	}
	// The lookup waits within its bound, then names the absent subsystem.
	n := (&ruleNode{}).on("looptab", answer{out: "done\n"}, answer{out: "done\n"}, answer{out: "dev loop9\ndone\n"})
	if dev, why := x.DeviceOf(ctx, n, "loop", "s1", time.Second); dev != "/dev/loop9" || why != "" {
		t.Errorf("lookup: %q %q", dev, why)
	}
	if _, why := x.DeviceOf(ctx, (&ruleNode{}).on("looptab", answer{out: "done\n"}), "loop", "s1", 0); why != "no device of s1" {
		t.Errorf("absent: %q", why)
	}
	// Red controls: a required value that does not match runs nothing; an
	// unknown resolver is an error, never a device.
	n = (&ruleNode{}).on("loopattach", answer{})
	if why := x.DeviceConnect(ctx, n, "loop", "10.0.0.1:x", "s1", ""); why != "bad port x" || len(n.calls) != 0 {
		t.Errorf("connect require: %q %q", why, n.calls)
	}
	n = (&ruleNode{}).on("loopattach", answer{})
	if why := x.DeviceConnect(ctx, n, "loop", "10.0.0.1:7", "s1", ",o=1"); why != "" || n.calls[0] != "loopattach s1 10.0.0.1 7,o=1" {
		t.Errorf("connect: %q %q", why, n.calls)
	}
	if state, _, why := x.DeviceScan(ctx, &ruleNode{}, "nvme", "s1"); state != DeviceUnreadable || why == "" {
		t.Errorf("an undeclared resolver: %s %q", state, why)
	}
	// A disconnect is certified only by a scan that finds none.
	n = (&ruleNode{}).on("loopdetach", answer{out: "removed 2\n"}).on("looptab", answer{out: "dev loop3\ndone\n"}, answer{out: "done\n"})
	if c, why := x.DeviceDisconnect(ctx, n, "loop", "s1"); c != 2 || why != "" {
		t.Errorf("disconnect: %d %q", c, why)
	}
	n = (&ruleNode{}).on("loopdetach", answer{out: "removed 1\n"}).on("looptab", answer{code: 1})
	if _, why := x.DeviceDisconnect(ctx, n, "loop", "s1"); why != "unconfirmed: looptab exit 1: " {
		t.Errorf("unconfirmed: %q", why)
	}
	n = (&ruleNode{}).on("loopdetach", answer{out: "removed 1\n"}).on("looptab", answer{out: "dev loop3\ndone\n"})
	if _, why := x.DeviceDisconnect(ctx, n, "loop", "s1"); why != "s1 remains" {
		t.Errorf("remains: %q", why)
	}
}

// Parse refuses a resolver that misses a part, and a step naming a
// resolver that is not declared.
func TestResolverParseRefusals(t *testing.T) {
	for name, c := range map[string][2]string{
		"no device line kind":   {`"device_line": "dev"`, `"device_line": "disk"`},
		"a count of no kind":    {`"absent": {"dev": 0}`, `"absent": {"disk": 0}`},
		"a bad require pattern": {`"pattern": "^[0-9]+$"`, `"pattern": "^[0-9+$"`},
		"no confirm bound":      {`"confirm_within_seconds": 1`, `"confirm_within_seconds": 0`},
		"a two-group count":     {`"^removed ([0-9]+)$"`, `"^(removed) ([0-9]+)$"`},
	} {
		bad := strings.Replace(loopResolver, c[0], c[1], 1)
		if bad == loopResolver {
			t.Fatalf("%s: the seed did not apply", name)
		}
		if _, err := Parse([]byte(head0 + `"device_resolvers": ` + bad + `, "actions": []}`)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := Parse([]byte(strings.Replace(deviceDesc, `"resolver": "nvme_namespace"`, `"resolver": "nvme_tcp"`, 1))); err == nil {
		t.Error("a device naming an undeclared resolver was accepted")
	}
}
