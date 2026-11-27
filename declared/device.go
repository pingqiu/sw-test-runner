package declared

// Device actions (kind "device"). A device action does its I/O through the
// subject's block device on the step's node:
//
//   - Device: the device is resolved by a declared lookup before each
//     invocation. Resolver "nvme_namespace" is the one namespace of the one
//     kernel NVMe controller of a subsystem NQN (Subsystem, a template),
//     waited for up to WithinSeconds. A sysfs scan that cannot be read, or a
//     subsystem that never becomes exactly one device, ends the operation
//     unknown: no I/O goes to a device the runner could not establish.
//   - Each: one operation per item of a list param ({{item}}), each with its
//     own invoke and complete.
//   - Input: a runner-generated input, made on the node BEFORE the invoke and
//     its sha256 checked against the runner's own ({{input.sha256}},
//     {{input.path}}, {{input.text}}): a mismatch is the runner's failure and
//     records nothing, so no content reaches the device that the history
//     names nowhere. Kind "repeat": Text repeated to Bytes bytes.
//   - IO: the I/O command, run DETACHED under its own runner deadline (the
//     runner's bound on one I/O, not a product bound): a child stuck in
//     uninterruptible sleep on a dead device is killed at it, and what
//     survives is abandoned and watched (watch.go). Exit 0 (and, with
//     OKStdout, a trimmed stdout matching it, {{io.stdout}}) is ok; any
//     other answer is unknown with Failure as the detail. A device I/O has
//     no refusal.
//
// The whole operation runs under the action's runner deadline (bounded.go).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	tr "github.com/pingqiu/sw-test-runner"
)

// DeviceLookup is a device action's lookup of the subject's block device.
type DeviceLookup struct {
	Resolver      string `json:"resolver"`
	Subsystem     string `json:"subsystem"`
	WithinSeconds int    `json:"within_seconds"`
	Cite          string `json:"cite"`
}

// Input is a runner-generated input of a device action.
type Input struct {
	Kind    string `json:"kind"`
	Text    string `json:"text"`
	Bytes   int    `json:"bytes"`
	Path    string `json:"path"`
	Command []Word `json:"command"`
	Cite    string `json:"cite"`
}

// IO is a device action's I/O command and the reading of its answer.
type IO struct {
	Command               []Word `json:"command"`
	AsRoot                bool   `json:"as_root,omitempty"`
	RunnerDeadlineSeconds int    `json:"runner_deadline_seconds"`
	Cite                  string `json:"cite"`
	OKStdout              string `json:"ok_stdout,omitempty"`
	Failure               string `json:"failure"`
	// Result is recorded on ok; ResultAbandoned is added to the result of
	// an I/O its deadline ended.
	Result          map[string]string `json:"result,omitempty"`
	ResultAbandoned map[string]string `json:"result_abandoned,omitempty"`
}

// What a scan of the kernel's NVMe controllers says about one subsystem.
const (
	NVMeAbsent     = "absent"     // no controller of the subsystem: a scan that read everything
	NVMeOne        = "one"        // one controller with exactly one namespace: its device
	NVMeIncomplete = "incomplete" // a controller without exactly one namespace (still coming up, or not ours)
	NVMeUnreadable = "unreadable" // the scan failed: nothing may be concluded
)

var nsName = regexp.MustCompile(`c[0-9]+n`)

// NVMeScan reads every controller's subsystem NQN in sysfs and the
// namespaces of the ones that match nqn. A read error, or a command that
// fails, is unreadable: absent and present are concluded only from a scan
// that completed.
func NVMeScan(ctx context.Context, node Runner, nqn string) (state, dev, why string) {
	cmd := fmt.Sprintf(`for d in /sys/class/nvme/nvme*; do [ -e "$d" ] || continue; q=$(cat "$d/subsysnqn") || exit 3; [ "$q" = %s ] || continue; echo "ctrl $(basename "$d")"; for n in "$d"/nvme*n*; do [ -e "$n" ] && echo "ns $(basename "$n")"; done; done; echo end`, ShellQuote(nqn))
	stdout, stderr, code, err := Call(ctx, node.Run, cmd)
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if err != nil || code != 0 || len(lines) == 0 || lines[len(lines)-1] != "end" {
		return NVMeUnreadable, "", fmt.Sprintf("sysfs scan exit %d: %s", code, FirstLine(stderr))
	}
	var ctrls, nss []string
	for _, l := range lines[:len(lines)-1] {
		switch {
		case strings.HasPrefix(l, "ctrl "):
			ctrls = append(ctrls, l[5:])
		case strings.HasPrefix(l, "ns "):
			nss = append(nss, nsName.ReplaceAllString(l[3:], "n"))
		default:
			return NVMeUnreadable, "", "sysfs scan printed " + FirstLine(l)
		}
	}
	switch {
	case len(ctrls) == 0:
		return NVMeAbsent, "", ""
	case len(ctrls) == 1 && len(nss) == 1:
		return NVMeOne, "/dev/" + nss[0], ""
	}
	return NVMeIncomplete, "", fmt.Sprintf("subsystem %s: %d controllers, %d namespaces", nqn, len(ctrls), len(nss))
}

// NVMeDevice waits until the subsystem has exactly one controller with one
// namespace and returns its device. An unreadable scan or a subsystem that
// never becomes exactly one device is a reason, not a device.
func NVMeDevice(ctx context.Context, node Runner, nqn string, bound time.Duration) (string, string) {
	end := time.Now().Add(bound)
	for {
		state, dev, why := NVMeScan(ctx, node, nqn)
		if state == NVMeOne {
			return dev, ""
		}
		if state == NVMeUnreadable {
			return "", why
		}
		if time.Now().After(end) {
			if state == NVMeAbsent {
				return "", "no controller of " + nqn
			}
			return "", why
		}
		if !Pause(ctx, 250*time.Millisecond) {
			return "", "no single device of " + nqn + " by the runner deadline"
		}
	}
}

var nvmeHost, nvmePort, nvmeNQN = regexp.MustCompile(`^[0-9.]+$`), regexp.MustCompile(`^[0-9]+$`), regexp.MustCompile(`^[A-Za-z0-9.:_-]+$`)
var nvmeOptions = regexp.MustCompile(`^(,[a-z_]+=-?[0-9]+)*$`)

// NVMeConnect connects the kernel NVMe/TCP host to nqn at host:port through
// the kernel's fabrics interface, the call nvme-cli makes (no tool needed);
// extra is ",key=value..." of kernel options. "" or the reason it failed.
func NVMeConnect(ctx context.Context, node Runner, host, port, nqn, extra string) string {
	if !nvmeHost.MatchString(host) || !nvmePort.MatchString(port) || !nvmeNQN.MatchString(nqn) {
		return "nvme connect: malformed address or nqn"
	}
	if !nvmeOptions.MatchString(extra) {
		return "nvme connect: malformed options " + extra
	}
	opts := fmt.Sprintf("transport=tcp,traddr=%s,trsvcid=%s,nqn=%s%s", host, port, nqn, extra)
	if _, stderr, code, err := Call(ctx, node.RunRoot, fmt.Sprintf("printf %%s %s > /dev/nvme-fabrics", ShellQuote(opts))); err != nil || code != 0 {
		return "nvme connect: " + FirstLine(stderr)
	}
	return ""
}

// Pause waits d, or less when ctx ends; false when ctx has ended.
func Pause(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return ctx.Err() == nil
	}
}

// RepeatSHA is the sha256 of text repeated to n bytes: the content a
// "repeat" input puts down, computed by the runner so an I/O whose outcome
// is unknown still names the value it attempted.
func RepeatSHA(text string, n int) string {
	if text == "" || n <= 0 {
		return ""
	}
	b := []byte(strings.Repeat(text, n/len(text)+1))[:n]
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (a *Action) checkDevice(defined func(map[string]bool) func(string) error, subjects map[string]bool, lists map[string]bool) error {
	if len(a.Variants) != 0 || len(a.Steps) != 0 || a.Retry != nil || len(a.OnOKSet) != 0 {
		return errors.New("a device action has device, io and optionally each and input only")
	}
	if a.RunnerDeadlineSeconds <= 0 {
		return errors.New("a device action needs its runner_deadline_seconds")
	}
	d, io := a.Device, a.IO
	if d == nil || io == nil {
		return errors.New("a device action needs device and io")
	}
	if a.Each != "" && !lists[a.Each] {
		return fmt.Errorf("each %q is not a list param", a.Each)
	}
	names := map[string]bool{}
	if a.Each != "" {
		names["item"] = true
	}
	if in := a.Input; in != nil {
		if in.Kind != "repeat" || in.Bytes <= 0 || in.Text == "" || in.Path == "" || len(in.Command) == 0 || strings.TrimSpace(in.Cite) == "" {
			return errors.New("input: kind repeat with text, bytes, path, command and cite")
		}
		for _, t := range []string{in.Text, in.Path} {
			if err := defined(names)(t); err != nil {
				return fmt.Errorf("input: %w", err)
			}
		}
		names["input.text"], names["input.path"] = true, true
		if err := checkWords(in.Command, defined(names)); err != nil {
			return fmt.Errorf("input: %w", err)
		}
		names["input.sha256"] = true
	}
	for k, t := range a.OnInvokeSet {
		if i := strings.IndexByte(k, '.'); i <= 0 || !subjects[k[:i]] {
			return fmt.Errorf("on_invoke_set %q: not <subject>.<field>", k)
		}
		if err := defined(names)(t); err != nil {
			return err
		}
	}
	for _, v := range a.Record.Args {
		if err := defined(names)(v.Template); err != nil {
			return err
		}
	}
	if d.Resolver != "nvme_namespace" || d.WithinSeconds < 0 || strings.TrimSpace(d.Cite) == "" {
		return errors.New("device: resolver nvme_namespace, within_seconds and cite")
	}
	if err := defined(names)(d.Subsystem); err != nil || d.Subsystem == "" {
		return fmt.Errorf("device: subsystem: %v", err)
	}
	names["device"] = true
	if len(io.Command) == 0 || io.RunnerDeadlineSeconds <= 0 || strings.TrimSpace(io.Cite) == "" || io.Failure == "" {
		return errors.New("io: command, runner_deadline_seconds, cite and failure are required")
	}
	if io.RunnerDeadlineSeconds >= a.RunnerDeadlineSeconds {
		return errors.New("io: its runner deadline must lie inside the action's")
	}
	if io.OKStdout != "" {
		if _, err := regexp.Compile(io.OKStdout); err != nil {
			return fmt.Errorf("io: ok_stdout: %w", err)
		}
	}
	if err := checkWords(io.Command, defined(names)); err != nil {
		return fmt.Errorf("io: %w", err)
	}
	for _, t := range io.ResultAbandoned {
		if err := defined(names)(t); err != nil {
			return fmt.Errorf("io: %w", err)
		}
	}
	answer := map[string]bool{"exit": true, "stderr_first_line": true}
	for k := range names {
		answer[k] = true
	}
	if err := defined(answer)(io.Failure); err != nil {
		return fmt.Errorf("io: %w", err)
	}
	answer["io.stdout"] = true
	for _, t := range io.Result {
		if err := defined(answer)(t); err != nil {
			return fmt.Errorf("io: %w", err)
		}
	}
	return nil
}

// device runs a device action: one bounded operation per item.
func (x *Executor) device(s *scope, actx *tr.ActionContext, act tr.Action, a *Action, expect string) (map[string]string, error) {
	items := []string{""}
	if a.Each != "" {
		items = strings.Fields(s.vals[a.Each])
	}
	dirty := false
	save := func() {
		if dirty {
			for _, sub := range s.subjects {
				sub.Save()
			}
		}
	}
	defer save()
	var last map[string]string
	for _, item := range items {
		if a.Each != "" {
			s.vals["item"] = item
		}
		if in := a.Input; in != nil {
			if err := x.makeInput(s, a, in); err != nil {
				return nil, fmt.Errorf("%s: %w", a.Action, err)
			}
		}
		for _, k := range sortedKeys(a.OnInvokeSet) {
			val, err := s.expand(a.OnInvokeSet[k])
			i := strings.IndexByte(k, '.')
			if err == nil {
				err = s.subjects[k[:i]].Set(k[i+1:], val)
			}
			if err != nil {
				return nil, fmt.Errorf("%s: on_invoke_set %s: %w", a.Action, k, err)
			}
			dirty = true
		}
		vol, args, err := x.recordOf(s, a, expect)
		if err != nil {
			return nil, err
		}
		last, err = x.bounded(s, actx, act, a, vol, args, func(ctx context.Context) (string, map[string]any) {
			return x.deviceIO(s, ctx, a)
		})
		if err != nil {
			return nil, err
		}
	}
	return last, nil
}

// makeInput generates the input on the node and checks its hash.
func (x *Executor) makeInput(s *scope, a *Action, in *Input) error {
	text, err := s.expand(in.Text)
	if err != nil {
		return err
	}
	path, err := s.expand(in.Path)
	if err != nil {
		return err
	}
	want := RepeatSHA(text, in.Bytes)
	s.vals["input.text"], s.vals["input.path"], s.vals["input.sha256"] = text, path, want
	cmd, err := s.command(in.Command)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(s.ctx, x.opDeadline(s, a))
	defer cancel()
	stdout, stderr, code, err := Call(ctx, s.node.Run, cmd)
	if err != nil || code != 0 || strings.TrimSpace(stdout) != want {
		return fmt.Errorf("the input generator made %q (exit %d, %s), not %s: runner failure, nothing written", FirstLine(stdout), code, FirstLine(stderr), want)
	}
	return nil
}

// deviceIO resolves the device and runs the I/O once.
func (x *Executor) deviceIO(s *scope, ctx context.Context, a *Action) (string, map[string]any) {
	d, io := a.Device, a.IO
	nqn, err := s.expand(d.Subsystem)
	if err != nil {
		return tr.OutcomeUnknown, map[string]any{"detail": err.Error()}
	}
	dev, why := NVMeDevice(ctx, s.node, nqn, time.Duration(d.WithinSeconds)*time.Second)
	if why != "" {
		return tr.OutcomeUnknown, map[string]any{"detail": why}
	}
	s.vals["device"] = dev
	cmd, err := s.command(io.Command)
	if err != nil {
		return tr.OutcomeUnknown, map[string]any{"detail": err.Error()}
	}
	dir, err := s.expand(x.File.RunnerIODir)
	if err != nil {
		return tr.OutcomeUnknown, map[string]any{"detail": err.Error()}
	}
	deadline := time.Duration(io.RunnerDeadlineSeconds) * time.Second
	if x.Deadline != nil {
		deadline = x.Deadline(deadline)
	}
	stdout, stderr, code, err := RunDetached(ctx, s.node, io.AsRoot, dir, cmd, deadline, x.poll())
	if t, ok := AsDetached(err); ok {
		res := Abandon(ctx, t)
		for k, tpl := range io.ResultAbandoned {
			v, _ := s.expand(tpl)
			res[k] = v
		}
		return tr.OutcomeUnknown, res
	}
	if err != nil {
		return tr.OutcomeUnknown, map[string]any{"detail": err.Error()}
	}
	s.vals["exit"], s.vals["stderr_first_line"] = fmt.Sprint(code), FirstLine(stderr)
	out := strings.TrimSpace(stdout)
	if code != 0 || (io.OKStdout != "" && !regexp.MustCompile(io.OKStdout).MatchString(out)) {
		msg, _ := s.expand(io.Failure)
		return tr.OutcomeUnknown, map[string]any{"detail": msg}
	}
	s.vals["io.stdout"] = out
	if len(io.Result) == 0 {
		return tr.OutcomeOK, nil
	}
	res := map[string]any{}
	for k, tpl := range io.Result {
		v, _ := s.expand(tpl)
		res[k] = v
	}
	return tr.OutcomeOK, res
}
