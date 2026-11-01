//go:build linux

package seaweed

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The partition and netem scripts run for real through sh -ec with stub
// sudo/iptables/tc/systemd-run/ip/modinfo that log every call and keep the
// chain/qdisc state in files. The auto-heal timer must come first, and the
// witnesses must see the effect present, then absent.
func chaos2Rig(t *testing.T, broken string) (dir string, run func(script string) (string, error), calls func() []string) {
	dir = t.TempDir()
	bin := filepath.Join(dir, "bin")
	os.MkdirAll(bin, 0700)
	stub := func(name, body string) {
		os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\necho \""+name+" $*\" >> \"$RIG/calls\"\n"+body+"\n"), 0700)
	}
	stub("sudo", `[ "$1" = -n ] && shift; exec "$@"`)
	stub("systemd-run", `exit 0`)
	stub("ip", `echo "10.84.1.10 via 10.84.1.1 dev eth0 src 10.84.1.21 uid 0"`)
	stub("modinfo", map[bool]string{true: "exit 1", false: "exit 0"}[broken == "no-netem"])
	stub("modprobe", map[bool]string{true: `echo "FATAL: Module sch_netem not found"; exit 1`, false: `echo "insmod /lib/modules/x/sch_netem.ko"`}[broken == "no-netem"])
	stub("iptables", `state="$RIG/chain"
case "$1" in
-N) touch "$state";;
-X) [ "`+broken+`" = sticky ] || rm -f "$state";;
-S) [ -e "$state" ];;
-C) [ -e "$state.jump$2" ];;
-I) [ "`+broken+`" = no-jump ] || touch "$state.jump$2";;
-D) [ -e "$state.jump$2" ] || exit 1; rm -f "$state.jump$2";;
esac`)
	stub("tc", `case "$2" in
add) [ "`+broken+`" = no-qdisc ] || touch "$RIG/qdisc";;
del) rm -f "$RIG/qdisc";;
show) [ -e "$RIG/qdisc" ] && echo "qdisc netem 8001: root delay 50ms 10ms loss 1%" || echo "qdisc mq 0: root";;
esac`)
	env := append(os.Environ(), "RIG="+dir, "PATH="+bin+":"+os.Getenv("PATH"))
	run = func(script string) (string, error) {
		c := exec.Command("sh", "-ec", script)
		c.Env = env
		out, e := c.Output()
		return string(out), e
	}
	calls = func() []string {
		b, _ := os.ReadFile(filepath.Join(dir, "calls"))
		return strings.Split(strings.TrimSpace(string(b)), "\n")
	}
	return
}

func firstIndex(calls []string, prefix string) int {
	for i, c := range calls {
		if strings.HasPrefix(c, prefix) {
			return i
		}
	}
	return -1
}

func TestChaos2PartitionScripts(t *testing.T) {
	dir, run, calls := chaos2Rig(t, "")
	out, e := run(chaosPartitionScript("sw-cloud-r-autoheal-C3"))
	if e != nil || strings.TrimSpace(out) != "PARTITIONED" {
		t.Fatalf("partition: %q %v", out, e)
	}
	c := calls()
	timer, chain := firstIndex(c, "systemd-run --unit=sw-cloud-r-autoheal-C3 --on-active=150"), firstIndex(c, "iptables -N SWCHAOS")
	if timer < 0 || chain < 0 || timer > chain {
		t.Fatalf("auto-heal timer must be installed before the partition: %v", c)
	}
	for _, want := range []string{"iptables -A SWCHAOS -p tcp --dport 22 -j RETURN", "iptables -A SWCHAOS -p tcp --sport 22 -j RETURN", "iptables -A SWCHAOS -j DROP", "iptables -I INPUT 1 -j SWCHAOS", "iptables -I OUTPUT 1 -j SWCHAOS"} {
		if firstIndex(c, want) < 0 {
			t.Fatalf("missing %q in %v", want, c)
		}
	}
	if out, e = run(chaosPartitionHealScript()); e != nil || strings.TrimSpace(out) != "HEALED" {
		t.Fatalf("heal: %q %v", out, e)
	}
	if _, err := os.Stat(filepath.Join(dir, "chain")); err == nil {
		t.Fatal("chain still present after heal")
	}
	// A jump that never landed must not be reported as a partition.
	_, run, _ = chaos2Rig(t, "no-jump")
	if out, e = run(chaosPartitionScript("t")); e == nil || strings.Contains(out, "PARTITIONED") {
		t.Fatalf("partition without its jump witnessed: %q %v", out, e)
	}
	// A chain that survives the heal must not be reported healed.
	_, run, _ = chaos2Rig(t, "sticky")
	if out, e = run(chaosPartitionScript("t")); e != nil {
		t.Fatalf("sticky partition: %q %v", out, e)
	}
	if out, e = run(chaosPartitionHealScript()); e == nil || strings.Contains(out, "HEALED") {
		t.Fatalf("heal witnessed with the chain still present: %q %v", out, e)
	}
}

func TestChaos2NetemScripts(t *testing.T) {
	_, run, calls := chaos2Rig(t, "")
	out, e := run(chaosNetemScript("10.84.1.10", "sw-cloud-r-autoheal-C6"))
	if e != nil || strings.TrimSpace(out) != "NETEM eth0" {
		t.Fatalf("netem: %q %v", out, e)
	}
	c := calls()
	timer, add := firstIndex(c, "systemd-run --unit=sw-cloud-r-autoheal-C6 --on-active=150"), firstIndex(c, "tc qdisc add dev eth0 root netem delay 50ms 10ms loss 1%")
	if timer < 0 || add < 0 || timer > add {
		t.Fatalf("auto-heal timer must be installed before the qdisc: %v", c)
	}
	if out, e = run(chaosNetemClearScript("10.84.1.10")); e != nil || strings.TrimSpace(out) != "CLEARED" {
		t.Fatalf("clear: %q %v", out, e)
	}
	for _, broken := range []string{"no-netem", "no-qdisc"} {
		_, run, calls = chaos2Rig(t, broken)
		if out, e = run(chaosNetemScript("10.84.1.10", "t")); e == nil || strings.Contains(out, "NETEM") {
			t.Fatalf("%s: netem witnessed: %q %v", broken, out, e)
		}
		if broken == "no-netem" && firstIndex(calls(), "systemd-run") >= 0 {
			t.Fatal("timer installed although sch_netem is missing")
		}
	}
}

// The run-7 probe is read-only: it reports, never installs, never fails.
func TestChaos2NetemProbeIsReadOnly(t *testing.T) {
	for broken, want := range map[string]string{"": "SCH_NETEM AVAILABLE", "no-netem": "SCH_NETEM MISSING"} {
		_, run, calls := chaos2Rig(t, broken)
		out, e := run(chaosNetemProbe)
		if e != nil || !strings.Contains(out, "KERNEL ") || !strings.Contains(out, want) {
			t.Fatalf("%q: %q %v", broken, out, e)
		}
		for _, c := range calls() {
			if strings.HasPrefix(c, "sudo") || strings.HasPrefix(c, "systemd-run") || strings.HasPrefix(c, "tc ") {
				t.Fatal("probe changed state:", c)
			}
		}
	}
}
