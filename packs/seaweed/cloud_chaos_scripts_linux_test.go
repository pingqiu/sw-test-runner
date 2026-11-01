//go:build linux

package seaweed

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The fault scripts run for real through sh -ec against a local multi-thread
// victim. A stub systemctl maps unit -> MainPID (0 once killed, like systemd);
// sudo either passes through or, for the negatives, does nothing.
type scriptRig struct {
	t   *testing.T
	dir string
	env []string
}

func newScriptRig(t *testing.T, sudo string) *scriptRig {
	r := &scriptRig{t: t, dir: t.TempDir()}
	bin := filepath.Join(r.dir, "bin")
	os.MkdirAll(filepath.Join(r.dir, "units"), 0700)
	os.MkdirAll(bin, 0700)
	os.WriteFile(filepath.Join(bin, "systemctl"), []byte("#!/bin/sh\ncat \"$RIG/units/$5\" 2>/dev/null || echo 0\n"), 0700)
	os.WriteFile(filepath.Join(bin, "sudo"), []byte("#!/bin/sh\n"+sudo+"\n"), 0700)
	r.env = append(os.Environ(), "RIG="+r.dir, "PATH="+bin+":"+os.Getenv("PATH"))
	return r
}

// victim starts a three-thread process as the unit's MainPID and reaps it.
func (r *scriptRig) victim(unit string) (int, <-chan struct{}) {
	if _, e := exec.LookPath("python3"); e != nil {
		r.t.Skip("python3 required")
	}
	cmd := exec.Command("python3", "-c", "import threading,time\nfor _ in range(2): threading.Thread(target=time.sleep,args=(60,),daemon=True).start()\ntime.sleep(60)")
	if e := cmd.Start(); e != nil {
		r.t.Fatal(e)
	}
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	r.t.Cleanup(func() { cmd.Process.Kill(); syscall.Kill(cmd.Process.Pid, syscall.SIGCONT) })
	for i := 0; i < 100; i++ { // wait until the helper threads exist
		if tasks, _ := os.ReadDir("/proc/" + strconv.Itoa(cmd.Process.Pid) + "/task"); len(tasks) >= 3 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	os.WriteFile(filepath.Join(r.dir, "units", unit), []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0600)
	return cmd.Process.Pid, done
}

func (r *scriptRig) run(script string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-ec", script)
	cmd.Env = r.env
	out, e := cmd.Output()
	return string(out), e
}

func threadStates(pid int) string {
	var states []string
	tasks, _ := filepath.Glob("/proc/" + strconv.Itoa(pid) + "/task/*/stat")
	for _, f := range tasks {
		b, _ := os.ReadFile(f)
		states = append(states, strings.Fields(string(b[strings.LastIndex(string(b), ")")+1:]))[0])
	}
	return strings.Join(states, "")
}

func TestChaosScriptsAgainstARealProcess(t *testing.T) {
	r := newScriptRig(t, `[ "$1" = -n ] && shift; exec "$@"`)
	pid, done := r.victim("u-volume")
	p := strconv.Itoa(pid)

	out, e := r.run(chaosStopScript("u-volume"))
	f := strings.Fields(out)
	if e != nil || len(f) != 4 || f[0] != "PID" || f[1] != p || f[3] != "STOPPED" || strings.Trim(threadStates(pid), "T") != "" {
		t.Fatalf("stop: %q %v states=%s", out, e, threadStates(pid))
	}
	if out, e = r.run(chaosContScript(f[1], f[2])); e != nil || strings.TrimSpace(out) != "LIVE" || strings.Contains(threadStates(pid), "T") {
		t.Fatalf("cont: %q %v states=%s", out, e, threadStates(pid))
	}
	if out, e = r.run(chaosKillScript("u-volume")); e != nil || !strings.HasPrefix(out, "OLD "+p+" ") || !strings.HasSuffix(out, "\nGONE\n") {
		t.Fatalf("kill: %q %v", out, e)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("victim survived the kill script")
	}
	next, _ := r.victim("u-volume-g2")
	if out, e = r.run(chaosIdentityScript("u-volume-g2")); e != nil || !strings.HasPrefix(out, "NEW "+strconv.Itoa(next)+" ") {
		t.Fatalf("identity: %q %v", out, e)
	}
	// A killed unit has MainPID 0: freezing it must refuse, as on real systemd.
	os.WriteFile(filepath.Join(r.dir, "units", "u-volume"), []byte("0\n"), 0600)
	if out, e = r.run(chaosStopScript("u-volume")); e == nil || strings.Contains(out, "STOPPED") {
		t.Fatalf("stop on a killed unit: %q %v", out, e)
	}
}

// With a signal that never lands, each witness loop must time out and refuse.
func TestChaosScriptWitnessesRefuseWithoutTheEffect(t *testing.T) {
	for _, name := range []string{"kill", "stop", "cont"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newScriptRig(t, "exit 0")
			pid, _ := r.victim("u")
			start := ""
			if name == "cont" {
				syscall.Kill(pid, syscall.SIGSTOP)
				time.Sleep(200 * time.Millisecond)
				b, _ := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
				start = strings.Fields(string(b[strings.LastIndex(string(b), ")")+1:]))[19]
			}
			script := map[string]string{"kill": chaosKillScript("u"), "stop": chaosStopScript("u"), "cont": chaosContScript(strconv.Itoa(pid), start)}[name]
			if out, e := r.run(script); e == nil || strings.Contains(out, "GONE") || strings.Contains(out, "STOPPED") || strings.Contains(out, "LIVE") {
				t.Fatalf("%s witnessed without the effect: %q %v", name, out, e)
			}
		})
	}
}
