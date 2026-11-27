package declared

// Device actions (kind "device"). A device action does its I/O through the
// subject's block device on the step's node:
//
//   - Device: the device is resolved before each invocation by a device
//     resolver the product declares (Resolver, resolver.go) for Subsystem (a
//     template), waited for up to WithinSeconds. A scan that cannot be read,
//     or a subsystem that never becomes exactly one device, ends the
//     operation unknown: no I/O goes to a device the runner could not
//     establish.
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
	// Result is recorded on ok; ResultUnanswered is added to the result of
	// an I/O that gave no answer (its deadline ended it, or it could not run).
	Result           map[string]string `json:"result,omitempty"`
	ResultUnanswered map[string]string `json:"result_unanswered,omitempty"`
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
	if d.Resolver == "" || d.WithinSeconds < 0 || strings.TrimSpace(d.Cite) == "" {
		return errors.New("device: a resolver, within_seconds and cite")
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
	for _, t := range io.ResultUnanswered {
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
	dev, why := x.DeviceOf(ctx, s.node, d.Resolver, nqn, time.Duration(d.WithinSeconds)*time.Second)
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
	if err != nil {
		// No answer: its deadline ended it (what survived is abandoned and
		// watched), or it could not be run.
		res := map[string]any{"detail": err.Error()}
		if t, ok := AsDetached(err); ok {
			res = Abandon(ctx, t)
		}
		for k, tpl := range io.ResultUnanswered {
			v, _ := s.expand(tpl)
			res[k] = v
		}
		return tr.OutcomeUnknown, res
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
