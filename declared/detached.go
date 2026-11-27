package declared

// Bounded commands. A run that cannot end is the runner's defect, whatever
// the product does, so no command may hold the runner past its deadline:
//
//   - Call runs a short command in process and stops waiting at the
//     context's end (the command's own goroutine is left to finish).
//   - RunDetached runs a command DETACHED on the node, in its own session,
//     with its output and exit code in files the runner polls. The runner
//     never holds a pipe to it, so a child in uninterruptible sleep (dd on
//     a dead NVMe device) cannot block it. At the deadline the session is
//     killed; whatever survives the kill is abandoned, and its pid and
//     start time are returned so the history and the teardown know of it.
//
// Every protocol command starts with a no-op marker (": runner-io <verb>
// <dir> ..."); the launch's marker carries the command in base64, which
// SimHost reads to stand in for a host in tests.

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Proc names a process by pid and start time (field 22 of /proc/PID/stat).
type Proc struct {
	PID   string `json:"pid"`
	Start string `json:"start"`
}

// ProtocolCallBound bounds each protocol command (launch, poll, kill).
var ProtocolCallBound = 20 * time.Second

// Call runs cmd through run and returns at the context's end at the latest:
// then the error says no answer came, and the command is left to finish.
func Call(ctx context.Context, run func(context.Context, string) (string, string, int, error), cmd string) (string, string, int, error) {
	if err := ctx.Err(); err != nil {
		return "", "", -1, fmt.Errorf("not run: %w", err)
	}
	type answer struct {
		out, errOut string
		code        int
		err         error
	}
	ch := make(chan answer, 1)
	go func() {
		o, e, c, err := run(ctx, cmd)
		ch <- answer{o, e, c, err}
	}()
	select {
	case a := <-ch:
		return a.out, a.errOut, a.code, a.err
	case <-ctx.Done():
		return "", "", -1, fmt.Errorf("no answer: %w", ctx.Err())
	}
}

// Detached is the outcome of one detached command that did not answer in
// time: Abandoned lists what survived the kill.
type Detached struct {
	Deadline  time.Duration
	Abandoned []Proc
}

func (d *Detached) Error() string {
	return fmt.Sprintf("timeout after %ds", int(d.Deadline/time.Second))
}

var seq atomic.Uint64

// procStartShell prints "<prefix> <pid> <start>" for pid variable p.
func procStartShell(prefix, p string) string {
	return fmt.Sprintf(`s=$(cat /proc/%[2]s/stat 2>/dev/null); s=${s##*) }; set -- $s; echo "%[1]s %[2]s ${20}"`, prefix, p)
}

// RunDetached runs cmd detached under dir on node (as root with asRoot) and
// waits for it until deadline, polling every poll. It answers as Run does;
// at the deadline (or the context's end) it kills the command's session and
// returns a *Detached error naming what was abandoned. A launch that fails
// returns its error: the command did not run.
func RunDetached(ctx context.Context, node Runner, asRoot bool, dir, cmd string, deadline, poll time.Duration) (string, string, int, error) {
	run := node.Run
	if asRoot {
		run = node.RunRoot
	}
	short := func(c string) (string, string, int, error) {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ProtocolCallBound)
		defer cancel()
		return Call(cctx, run, c)
	}
	end := time.Now().Add(deadline)
	d := fmt.Sprintf("%s/%d-%d", strings.TrimRight(dir, "/"), time.Now().UnixNano(), seq.Add(1))
	q := ShellQuote
	inner := fmt.Sprintf("bash -c %s >%s 2>%s </dev/null; echo $? >%s; mv %s %s", q(cmd), q(d+"/out"), q(d+"/err"), q(d+"/rc.tmp"), q(d+"/rc.tmp"), q(d+"/rc"))
	launch := fmt.Sprintf(": runner-io launch %s %s; mkdir -p %s || exit 1; setsid bash -c %s >/dev/null 2>&1 </dev/null & p=$!; %s",
		q(d), base64.StdEncoding.EncodeToString([]byte(cmd)), q(d), q(inner), procStartShell("launched", "$p"))
	out, errOut, code, err := short(launch)
	if err != nil {
		// The command did not run: the launch's own answer.
		return out, errOut, code, err
	}
	f := strings.Fields(strings.TrimSpace(out))
	if code != 0 || len(f) < 2 || f[0] != "launched" {
		return "", "", -1, fmt.Errorf("launch exit %d: %s", code, FirstLine(out+" "+errOut))
	}
	pid := f[1]
	if _, err := strconv.Atoi(pid); err != nil {
		return "", "", -1, fmt.Errorf("launch answered pid %q", pid)
	}
	fetch := fmt.Sprintf(": runner-io poll %s; if [ -e %s ]; then echo done; cat %s; base64 -w0 %s; echo; base64 -w0 %s; echo; rm -f %s %s %s; rmdir %s 2>/dev/null; exit 0; else echo running; fi",
		q(d), q(d+"/rc"), q(d+"/rc"), q(d+"/out"), q(d+"/err"), q(d+"/rc"), q(d+"/out"), q(d+"/err"), q(d))
	for {
		if o, _, c, err := short(fetch); err == nil && c == 0 {
			if done, so, se, rc, ok := parseFetch(o); ok && done {
				return so, se, rc, nil
			}
		}
		if time.Now().After(end) || ctx.Err() != nil {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(min(poll, time.Until(end)+time.Millisecond)):
		}
	}
	// The deadline: kill the session unless it finished meanwhile; what
	// survives the kill is abandoned.
	kill := fmt.Sprintf(": runner-io kill %s %s; if [ -e %s ]; then echo done; exit 0; fi; env kill -KILL -- -%s 2>/dev/null; sleep 1; for q in $(pgrep -g %s 2>/dev/null); do %s; done; echo killed",
		q(d), pid, q(d+"/rc"), pid, pid, procStartShell("left", "$q"))
	o, _, _, kerr := short(kill)
	if kerr == nil && strings.TrimSpace(o) == "done" {
		if fo, _, c, err := short(fetch); err == nil && c == 0 {
			if done, so, se, rc, ok := parseFetch(fo); ok && done {
				return so, se, rc, nil
			}
		}
	}
	t := &Detached{Deadline: deadline}
	killed := false
	for _, l := range strings.Split(o, "\n") {
		lf := strings.Fields(l)
		switch {
		case len(lf) >= 2 && lf[0] == "left":
			p := Proc{PID: lf[1]}
			if len(lf) > 2 {
				p.Start = lf[2]
			}
			t.Abandoned = append(t.Abandoned, p)
		case len(lf) == 1 && lf[0] == "killed":
			killed = true
		}
	}
	if kerr != nil || !killed {
		// The kill's answer is unknown: the whole session may survive.
		t.Abandoned = append(t.Abandoned, Proc{PID: pid, Start: f2(f)})
	}
	return "", "", -1, t
}

func f2(f []string) string {
	if len(f) > 2 {
		return f[2]
	}
	return ""
}

// parseFetch reads a poll's answer: "running", or "done", the exit code and
// the two base64 bodies.
func parseFetch(o string) (done bool, stdout, stderr string, code int, ok bool) {
	lines := strings.Split(o, "\n")
	if len(lines) > 0 && strings.TrimSpace(lines[0]) == "running" {
		return false, "", "", 0, true
	}
	if len(lines) < 4 || strings.TrimSpace(lines[0]) != "done" {
		return false, "", "", 0, false
	}
	rc, err := strconv.Atoi(strings.TrimSpace(lines[1]))
	if err != nil {
		return false, "", "", 0, false
	}
	so, err1 := base64.StdEncoding.DecodeString(strings.TrimSpace(lines[2]))
	se, err2 := base64.StdEncoding.DecodeString(strings.TrimSpace(lines[3]))
	if err1 != nil || err2 != nil {
		return false, "", "", 0, false
	}
	return true, string(so), string(se), rc, true
}

// AsDetached reports whether err is a detached command's timeout.
func AsDetached(err error) (*Detached, bool) {
	var d *Detached
	ok := errors.As(err, &d)
	return d, ok
}
