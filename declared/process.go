package declared

// Process actions (kind "process"): a long-lived process the runner starts,
// kills and restarts BY THE IDENTITY IT RECORDED, never by a name or a
// pattern. A process is identified by its pid AND its start time (field 22
// of /proc/PID/stat): a pid alone may be recycled after the first process
// exits.
//
// Op "start" launches the declared argv detached, under the wrapper that
// keeps the process's own pid and writes its exit status to <log>.exit
// (WrapperLaunch); reads its start time; records it as owned in the
// subject's record (Set: pid, pid_start, launch_argv as a JSON list,
// exit_file); runs the Owned probes (the ownership ledger); then polls the
// Ready probes every EveryMs until all hold, while the recorded process
// runs, within the int param Ready.WithinParam (seconds); then the After
// probes (a connect, a device lookup). Before probes run before the launch
// (endpoints free, directories, a lookup the argv needs). Any probe that
// fails before readiness, or after it, ends the operation unknown with its
// detail (a self-reported command passes its own outcome on); a ready probe
// that fails only means "not ready yet". A process that exits before it is
// ready is unknown, whatever made it exit.
//
// Op "kill" sends SIGKILL to the subject's recorded process (fields pid,
// pid_start) in one shell that first checks the identity: nothing is sent
// when the pid is gone or now names another process (refused, nothing
// changed). The kill is certified only by a later check that finds the
// process gone within the int param WithinParam; until then it is unknown.
// The killed process is recorded (Set: killed_pid, killed_argv, its
// command line read from /proc before the signal).
//
// Op "restart" relaunches a process the runner killed (killed_pid is the
// recorded pid, and it no longer runs) with the command line it ran
// (killed_argv), as "start" does; a new identity recorded clears
// killed_pid and killed_argv. Anything else is refused: nothing started.
//
// A process param (kind "process") is resolved by the product (Executor
// Process); a name it cannot resolve is a refusal the history records,
// not the runner's error.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	tr "github.com/pingqiu/sw-test-runner"
)

// ProcessStep is a process action's step.
type ProcessStep struct {
	Op      string `json:"op"`
	Subject string `json:"subject"`
	// Name is the process's name in the details ("<name> exited ...").
	Name string `json:"name"`
	Cite string `json:"cite"`
	// start, restart
	Argv           []Word            `json:"argv,omitempty"`
	Log            string            `json:"log,omitempty"`
	Before         []Probe           `json:"before,omitempty"`
	Owned          []Probe           `json:"owned,omitempty"`
	Ready          *Ready            `json:"ready,omitempty"`
	After          []Probe           `json:"after,omitempty"`
	ResultLaunched map[string]Value  `json:"result_launched,omitempty"`
	ResultReady    map[string]Value  `json:"result_ready,omitempty"`
	OnReadySet     map[string]string `json:"on_ready_set,omitempty"`
	// kill
	WithinParam string `json:"within_param,omitempty"`
}

// Ready is the readiness of a started process.
type Ready struct {
	WithinParam string  `json:"within_param"`
	EveryMs     int     `json:"every_ms"`
	Probes      []Probe `json:"probes"`
	// NotReady is the detail when the bound passes ({{ready.within}} is the
	// bound), NotReadyByDeadline when the runner deadline ends the wait.
	NotReady           string `json:"not_ready"`
	NotReadyByDeadline string `json:"not_ready_by_deadline"`
}

// Probe is one check or step around a process.
//
//   - "command": Command runs; with Decode (self_reported) its outcome is
//     the step's when not ok, and OnOKSet updates subjects; without, exit 0
//     holds and anything else fails with Failure ({{exit}},
//     {{stderr_first_line}}).
//   - "ports_free": no TCP listener on the port of any of Addresses.
//   - "listeners_held": the launched process holds a TCP listener on the
//     port of each of Addresses.
//   - "json": Command's stdout decodes against Schema (as encoding/json
//     decodes a struct of those fields) and every Holds row holds; Capture
//     names values {{ready.<name>}} from paths.
//   - "nvme_connect": connect the kernel NVMe/TCP host to Subsystem at
//     Address with Options (",key=value..."); with UnlessPresent only when
//     a scan finds no controller of the subsystem.
//   - "device": the one namespace of Subsystem within WithinSeconds,
//     {{device}} and the result field Into.
type Probe struct {
	Name          string                     `json:"name"`
	Kind          string                     `json:"kind"`
	When          *Cond                      `json:"when,omitempty"`
	Command       []Word                     `json:"command,omitempty"`
	AsRoot        bool                       `json:"as_root,omitempty"`
	Decode        *Decode                    `json:"decode,omitempty"`
	Failure       string                     `json:"failure,omitempty"`
	OnOKSet       map[string]string          `json:"on_ok_set,omitempty"`
	Addresses     []string                   `json:"addresses,omitempty"`
	Schema        map[string]json.RawMessage `json:"schema,omitempty"`
	Holds         []Hold                     `json:"holds,omitempty"`
	Capture       map[string]string          `json:"capture,omitempty"`
	Address       string                     `json:"address,omitempty"`
	Subsystem     string                     `json:"subsystem,omitempty"`
	Options       string                     `json:"options,omitempty"`
	UnlessPresent bool                       `json:"unless_present,omitempty"`
	WithinSeconds int                        `json:"within_seconds,omitempty"`
	Into          string                     `json:"into,omitempty"`
	Cite          string                     `json:"cite"`
}

// Hold is one condition on a decoded value: equal to a template, or (as
// unsigned integers) above or at least it.
type Hold struct {
	Path    string  `json:"path"`
	Equals  *string `json:"equals,omitempty"`
	Above   string  `json:"above,omitempty"`
	AtLeast string  `json:"at_least,omitempty"`
}

// Process states.
const (
	ProcRunning    = "running"    // the pid runs with the recorded start time
	ProcGone       = "gone"       // no such pid, or a different process now holds it
	ProcUnreadable = "unreadable" // the check failed: nothing may be concluded
)

var digitsRe = regexp.MustCompile(`^[0-9]+$`)

// ProcStartCmd is the shell that reads a pid's start time: field 22 of
// PROC/PID/stat. The command name (field 2) may hold spaces and
// parentheses, so the text is cut after the LAST ')'; the start time is
// then the 20th remaining field, which the shell must write ${20}. It
// prints "start N", "absent" for no such pid, or fails (exit 3) when the
// pid exists but its stat cannot be read.
func ProcStartCmd(proc, pid string) string {
	return fmt.Sprintf(`if [ -e %[1]s/%[2]s ]; then s=$(cat %[1]s/%[2]s/stat) || exit 3; s=${s##*) }; set -- $s; [ -n "${20}" ] || exit 3; echo "start ${20}"; else echo absent; fi`, proc, pid)
}

// ProcStart reads a pid's start time: ProcGone when the pid does not exist,
// ProcUnreadable when the read failed.
func ProcStart(ctx context.Context, node Runner, pid string) (string, string) {
	if !digitsRe.MatchString(pid) {
		return "", ProcGone
	}
	stdout, _, code, err := Call(ctx, node.Run, ProcStartCmd("/proc", pid))
	out := strings.TrimSpace(stdout)
	switch {
	case err != nil || code != 0:
		return "", ProcUnreadable
	case out == "absent":
		return "", ProcGone
	case regexp.MustCompile(`^start [0-9]+$`).MatchString(out):
		return out[6:], ProcRunning
	}
	return "", ProcUnreadable
}

// ProcState says whether the process (pid, start) still runs; without a
// recorded start time nothing may be concluded.
func ProcState(ctx context.Context, node Runner, pid, start string) string {
	if start == "" {
		return ProcUnreadable
	}
	got, state := ProcStart(ctx, node, pid)
	if state == ProcRunning && got != start {
		return ProcGone // the pid now names another process
	}
	return state
}

// WaitGone waits until the process (pid, start) is gone, certified only by
// a successful check that finds it gone: "" or why not.
func WaitGone(ctx context.Context, node Runner, pid, start string, bound time.Duration) string {
	end := time.Now().Add(bound)
	for {
		last := ProcState(ctx, node, pid, start)
		if last == ProcGone {
			return ""
		}
		if time.Now().After(end) {
			if last == ProcUnreadable {
				return fmt.Sprintf("pid %s: its state could not be read within %s", pid, bound)
			}
			return fmt.Sprintf("pid %s still running after %s", pid, bound)
		}
		if !Pause(ctx, 250*time.Millisecond) {
			return fmt.Sprintf("pid %s: not gone by the runner deadline", pid)
		}
	}
}

// KillCmd checks that pid still runs with the recorded start time, reads
// its command line, and sends SIGKILL, in one shell: it prints "gone" (no
// such pid, or another process now holds it: nothing sent), "sent <base64
// of /proc/PID/cmdline>", or exits 3 (its identity could not be read:
// nothing sent) or 4 (the kill call failed).
func KillCmd(proc, pid, start string) string {
	return fmt.Sprintf(`if [ -e %[1]s/%[2]s ]; then s=$(cat %[1]s/%[2]s/stat) || exit 3; s=${s##*) }; set -- $s; [ -n "${20}" ] || exit 3; [ "${20}" = %[3]s ] || { echo gone; exit 0; }; a=$(base64 -w0 < %[1]s/%[2]s/cmdline) || exit 3; kill -KILL %[2]s || exit 4; echo "sent $a"; else echo gone; fi`, proc, pid, start)
}

// DecodeArgv reads a base64 /proc/PID/cmdline into its argv.
func DecodeArgv(b64 string) ([]string, error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, err
	}
	s := strings.TrimRight(string(raw), "\x00")
	if s == "" {
		return nil, fmt.Errorf("empty command line")
	}
	return strings.Split(s, "\x00"), nil
}

// WrapperLaunch starts argv under the wrapper and prints the process's pid.
// The wrapper (its own session) starts the process with its output in
// logPath, writes its pid to logPath.pid, waits for it and writes "<wait
// status> <unix ns>" to logPath.exit (each file written whole, by a
// rename). A wait status above 128 is a signal (128+n), as the shell
// reports it. The process keeps its own pid: it is the wrapper's child.
func WrapperLaunch(argv []string, logPath string) string {
	pidf, exitf := logPath+".pid", logPath+".exit"
	wrap := fmt.Sprintf(`"$@" >%[1]s 2>&1 </dev/null & p=$!; echo "$p" >%[2]s.tmp && mv %[2]s.tmp %[2]s; wait "$p"; s=$?; echo "$s $(date +%%s%%N)" >%[3]s.tmp && mv %[3]s.tmp %[3]s`,
		ShellQuote(logPath), ShellQuote(pidf), ShellQuote(exitf))
	q := make([]string, len(argv))
	for i, a := range argv {
		q[i] = ShellQuote(a)
	}
	return fmt.Sprintf(`setsid sh -c %s sh %s >/dev/null 2>&1 </dev/null & for i in $(seq 1 100); do [ -s %[3]s ] && exec cat %[3]s; sleep 0.05; done; echo "no pid file %[3]s" >&2; exit 1`,
		ShellQuote(wrap), strings.Join(q, " "), ShellQuote(pidf))
}

var pidRe = regexp.MustCompile(`pid=([0-9]+)`)

// ListenerPIDs maps each port to the pids holding a TCP listener on it,
// from one `ss -Hltnp` read as root (root sees every process). A failed
// read is an error, never "nobody listens".
func ListenerPIDs(ctx context.Context, node Runner, ports []string) (map[string][]string, error) {
	stdout, stderr, code, err := Call(ctx, node.RunRoot, "ss -Hltnp")
	if err != nil || code != 0 {
		return nil, fmt.Errorf("ss exit %d: %s", code, FirstLine(stderr))
	}
	out := map[string][]string{}
	want := map[string]bool{}
	for _, p := range ports {
		want[p] = true
	}
	for _, l := range strings.Split(stdout, "\n") {
		f := strings.Fields(l)
		if len(f) < 4 {
			continue
		}
		local := f[3]
		port := local[strings.LastIndex(local, ":")+1:]
		if !want[port] {
			continue
		}
		pids := pidRe.FindAllStringSubmatch(l, -1)
		if len(pids) == 0 {
			out[port] = append(out[port], "?") // held by a process ss could not name
		}
		for _, m := range pids {
			out[port] = append(out[port], m[1])
		}
	}
	return out, nil
}

// PortOf is the port of host:port.
func PortOf(addr string) string { return addr[strings.LastIndex(addr, ":")+1:] }

// Coded hands a step whose param Param equals Equals to the product's Go
// handler Handler (an exception of the description, with its why): a case
// of an action the step kind does not declare.
type Coded struct {
	Param   string `json:"param"`
	Equals  string `json:"equals"`
	Handler string `json:"handler"`
	Why     string `json:"why"`
}

// checkProcess checks a process action.
func (a *Action) checkProcess(defined func(map[string]bool) func(string) error, subjects, ints map[string]bool) error {
	p := a.Process
	if p == nil || len(a.Variants) != 0 || len(a.Steps) != 0 || a.Retry != nil || len(a.OnOKSet) != 0 || a.Device != nil || a.IO != nil || a.Each != "" || a.Input != nil {
		return errors.New("a process action has process only")
	}
	if a.RunnerDeadlineSeconds <= 0 || strings.TrimSpace(p.Cite) == "" || p.Name == "" {
		return errors.New("a process action needs its runner_deadline_seconds, and its process a name and a cite")
	}
	if !subjects[p.Subject] {
		return fmt.Errorf("process subject %q is not a subject or process param", p.Subject)
	}
	names := map[string]bool{"now_ns": true}
	switch p.Op {
	case "kill":
		if !ints[p.WithinParam] || len(p.Argv) != 0 || p.Ready != nil || len(p.Before)+len(p.Owned)+len(p.After) != 0 {
			return errors.New("kill: an int within_param, and nothing a start has")
		}
		return nil
	case "start":
		if len(p.Argv) == 0 {
			return errors.New("start: argv is required")
		}
		if err := checkWords(p.Argv, defined(names)); err != nil {
			return fmt.Errorf("argv: %w", err)
		}
	case "restart":
		if len(p.Argv) != 0 {
			return errors.New("restart relaunches the recorded command line: no argv")
		}
	default:
		return fmt.Errorf("process op %q", p.Op)
	}
	r := p.Ready
	if p.Log == "" || r == nil || !ints[r.WithinParam] || r.EveryMs <= 0 || len(r.Probes) == 0 || r.NotReady == "" || r.NotReadyByDeadline == "" {
		return errors.New("start/restart: log, and ready with an int within_param, every_ms, probes, not_ready and not_ready_by_deadline")
	}
	if err := defined(names)(p.Log); err != nil {
		return err
	}
	for k := range p.ResultLaunched {
		if k == "pid" || k == "pid_start" || k == "log" || k == "detail" {
			return fmt.Errorf("result_launched %q is the step's own field", k)
		}
	}
	check := func(at string, probes []Probe, ready bool) error {
		for i, pr := range probes {
			if err := pr.check(defined(names), subjects, ready); err != nil {
				return fmt.Errorf("%s probe %d (%s): %w", at, i, pr.Name, err)
			}
			for k := range pr.Capture {
				names["ready."+k] = true
			}
			if pr.Kind == "device" {
				names["device"] = true
			}
		}
		return nil
	}
	if err := check("before", p.Before, false); err != nil {
		return err
	}
	for _, v := range p.ResultLaunched {
		if err := defined(names)(v.Template); err != nil {
			return fmt.Errorf("result_launched: %w", err)
		}
	}
	names["launched.pid"], names["launched.start"], names["launched.log"] = true, true, true
	if err := check("owned", p.Owned, false); err != nil {
		return err
	}
	if err := check("ready", r.Probes, true); err != nil {
		return err
	}
	if err := defined(names)(r.NotReadyByDeadline); err != nil {
		return err
	}
	names["ready.within"] = true
	if err := defined(names)(r.NotReady); err != nil {
		return err
	}
	delete(names, "ready.within")
	for k, t := range p.OnReadySet {
		if i := strings.IndexByte(k, '.'); i <= 0 || !subjects[k[:i]] {
			return fmt.Errorf("on_ready_set %q: not <subject>.<field>", k)
		}
		if err := defined(names)(t); err != nil {
			return err
		}
	}
	for _, v := range p.ResultReady {
		if err := defined(names)(v.Template); err != nil {
			return fmt.Errorf("result_ready: %w", err)
		}
	}
	return check("after", p.After, false)
}

func (pr *Probe) check(defined func(string) error, subjects map[string]bool, ready bool) error {
	if strings.TrimSpace(pr.Cite) == "" {
		return errors.New("cite is required")
	}
	if pr.When != nil {
		if err := defined("{{" + pr.When.Field + "}}"); err != nil {
			return err
		}
	}
	addrs := func() error {
		if len(pr.Addresses) == 0 {
			return errors.New("addresses are required")
		}
		for _, t := range pr.Addresses {
			if err := defined(t); err != nil {
				return err
			}
		}
		return nil
	}
	switch pr.Kind {
	case "command":
		if len(pr.Command) == 0 {
			return errors.New("command is required")
		}
		if err := checkWords(pr.Command, defined); err != nil {
			return err
		}
		if pr.Decode == nil {
			if pr.Failure == "" || len(pr.OnOKSet) != 0 {
				return errors.New("a command without a decoding needs its failure, and sets nothing")
			}
			return nil
		}
		if ready || pr.Decode.Kind != "self_reported" || pr.Decode.Detail == "" {
			return errors.New("a decoded command is self_reported, with its detail, and not a ready probe")
		}
		for k := range pr.OnOKSet {
			if i := strings.IndexByte(k, '.'); i <= 0 || !subjects[k[:i]] {
				return fmt.Errorf("on_ok_set %q: not <subject>.<field>", k)
			}
		}
	case "ports_free":
		if ready {
			return errors.New("ports_free is a check before the launch")
		}
		return addrs()
	case "listeners_held":
		if !ready {
			return errors.New("listeners_held is a ready probe")
		}
		return addrs()
	case "json":
		if !ready || len(pr.Command) == 0 || len(pr.Holds) == 0 {
			return errors.New("json is a ready probe with a command and holds")
		}
		if _, err := structType(pr.Schema); err != nil {
			return err
		}
		if err := checkWords(pr.Command, defined); err != nil {
			return err
		}
		for _, h := range pr.Holds {
			n := 0
			for _, t := range []string{deref(h.Equals), h.Above, h.AtLeast} {
				if err := defined(t); err != nil {
					return err
				}
			}
			if h.Equals != nil {
				n++
			}
			if h.Above != "" {
				n++
			}
			if h.AtLeast != "" {
				n++
			}
			if h.Path == "" || n != 1 {
				return errors.New("a hold has a path and exactly one of equals, above, at_least")
			}
		}
	case "nvme_connect":
		if ready || pr.Address == "" || pr.Subsystem == "" {
			return errors.New("nvme_connect: address and subsystem, not a ready probe")
		}
		for _, t := range []string{pr.Address, pr.Subsystem, pr.Options} {
			if err := defined(t); err != nil {
				return err
			}
		}
	case "device":
		if ready || pr.Subsystem == "" || pr.Into == "" || pr.WithinSeconds < 0 {
			return errors.New("device: subsystem, into and within_seconds, not a ready probe")
		}
		return defined(pr.Subsystem)
	default:
		return fmt.Errorf("probe kind %q", pr.Kind)
	}
	return nil
}

// processStep runs a process action.
func (x *Executor) processStep(s *scope, actx *tr.ActionContext, act tr.Action, a *Action, expect string) (map[string]string, error) {
	p := a.Process
	dirty := false
	set := func(k, val string) error {
		i := strings.IndexByte(k, '.')
		if err := s.subjects[k[:i]].Set(k[i+1:], val); err != nil {
			return err
		}
		dirty = true
		return nil
	}
	defer func() {
		if dirty {
			for _, sub := range s.subjects {
				sub.Save()
			}
		}
	}()
	for _, k := range sortedKeys(a.OnInvokeSet) {
		val, err := s.expand(a.OnInvokeSet[k])
		if err == nil {
			err = set(k, val)
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", a.Action, err)
		}
	}
	vol, args, err := x.recordOf(s, a, expect)
	if err != nil {
		return nil, err
	}
	setp := func(f, val string) error { return set(p.Subject+"."+f, val) }
	out, err := x.bounded(s, actx, act, a, vol, args, func(ctx context.Context) (string, map[string]any) {
		switch p.Op {
		case "kill":
			return x.kill(s, ctx, p, setp)
		case "restart":
			return x.restart(s, ctx, p, setp, set)
		}
		// The argv is expanded after the Before probes: one of them may set a
		// field it names (the layout id a lookup resolved).
		out, res, _ := x.start(s, ctx, p, func() ([]string, error) { return s.argv(p.Argv) }, setp, set)
		return out, res
	})
	if err != nil {
		return nil, err
	}
	if sets, ok := a.OnOutcomeSet[out["value"]]; ok {
		for _, k := range sortedKeys(sets) {
			val, err := s.expand(sets[k])
			if err == nil {
				err = set(k, val)
			}
			if err != nil {
				return nil, fmt.Errorf("%s: on_outcome_set %s: %w", a.Action, k, err)
			}
		}
	}
	return out, nil
}

// field reads a field of the step's process subject; a subject the product
// could not resolve has none.
func (s *scope) field(p *ProcessStep, f string) string {
	sub, ok := s.subjects[p.Subject]
	if !ok {
		return ""
	}
	v, _ := sub.Field(s.ctx, s.node, f)
	return v
}

func (x *Executor) kill(s *scope, ctx context.Context, p *ProcessStep, set func(f, val string) error) (string, map[string]any) {
	name := s.vals[p.Subject]
	if why := s.unresolved[p.Subject]; why != "" {
		return tr.OutcomeRefused, map[string]any{"detail": why}
	}
	pid, start := s.field(p, "pid"), s.field(p, "pid_start")
	switch {
	case pid == "":
		return tr.OutcomeRefused, map[string]any{"detail": name + " is not running (no process recorded)"}
	case !digitsRe.MatchString(pid) || !digitsRe.MatchString(start):
		return tr.OutcomeRefused, map[string]any{"detail": "no identity recorded for " + name + ": nothing sent"}
	}
	stdout, stderr, code, err := Call(ctx, s.node.Run, KillCmd("/proc", pid, start))
	out := strings.TrimSpace(stdout)
	res := map[string]any{"pid": pid, "pid_start": start}
	switch {
	case err != nil:
		return tr.OutcomeUnknown, map[string]any{"detail": err.Error()}
	case code == 0 && out == "gone":
		res["detail"] = name + " is not running: nothing sent"
		return tr.OutcomeRefused, res
	case code == 3:
		res["detail"] = "identity unreadable: nothing sent"
		return tr.OutcomeRefused, res
	case code != 0 || (out != "sent" && !strings.HasPrefix(out, "sent ")):
		res["detail"] = fmt.Sprintf("kill exit %d: %s %s", code, FirstLine(out), FirstLine(stderr))
		return tr.OutcomeUnknown, res
	}
	// The signal went out: the process is the killed one from now on,
	// whatever the wait below finds.
	argv := ""
	if a, aerr := DecodeArgv(strings.TrimSpace(strings.TrimPrefix(out, "sent"))); aerr != nil {
		res["argv_error"] = aerr.Error()
	} else {
		b, _ := json.Marshal(a)
		argv = string(b)
	}
	if err := set("killed_pid", pid); err != nil {
		res["detail"] = "recording the kill: " + err.Error()
		return tr.OutcomeUnknown, res
	}
	if err := set("killed_argv", argv); err != nil {
		res["detail"] = "recording the kill: " + err.Error()
		return tr.OutcomeUnknown, res
	}
	bound := time.Duration(atoi(s.vals[p.WithinParam])) * time.Second
	if why := WaitGone(ctx, s.node, pid, start, bound); why != "" {
		res["detail"] = why
		return tr.OutcomeUnknown, res
	}
	return tr.OutcomeOK, res
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

func (x *Executor) restart(s *scope, ctx context.Context, p *ProcessStep, set func(f, val string) error, setAny func(k, val string) error) (string, map[string]any) {
	name := s.vals[p.Subject]
	if why := s.unresolved[p.Subject]; why != "" {
		return tr.OutcomeRefused, map[string]any{"detail": why}
	}
	pid, killed := s.field(p, "pid"), s.field(p, "killed_pid")
	var argv []string
	if raw := s.field(p, "killed_argv"); raw != "" {
		_ = json.Unmarshal([]byte(raw), &argv)
	}
	switch {
	case pid == "" || killed != pid:
		return tr.OutcomeRefused, map[string]any{"detail": name + " was not killed by the runner: nothing started"}
	case len(argv) == 0:
		return tr.OutcomeRefused, map[string]any{"detail": "the killed process's command line is unknown: nothing started"}
	}
	switch ProcState(ctx, s.node, pid, s.field(p, "pid_start")) {
	case ProcRunning:
		return tr.OutcomeRefused, map[string]any{"detail": name + " is still running: nothing started"}
	case ProcUnreadable:
		return tr.OutcomeRefused, map[string]any{"detail": "state of the killed process unreadable: nothing started"}
	}
	out, res, launched := x.start(s, ctx, p, func() ([]string, error) { return argv, nil }, set, setAny)
	if launched {
		// A new identity is recorded: the killed one is history.
		if err := set("killed_pid", ""); err == nil {
			_ = set("killed_argv", "")
		}
	}
	return out, res
}

// start launches argv and waits for readiness; launched is true once a new
// identity is recorded in the subject.
func (x *Executor) start(s *scope, ctx context.Context, p *ProcessStep, argvOf func() ([]string, error), setp func(f, val string) error, set func(k, val string) error) (string, map[string]any, bool) {
	for i := range p.Before {
		if out, detail, ok := x.probe(s, ctx, &p.Before[i], nil, set); !ok {
			return out, map[string]any{"detail": detail}, false
		}
	}
	argv, err := argvOf()
	if err != nil {
		return tr.OutcomeUnknown, map[string]any{"detail": err.Error()}, false
	}
	s.vals["now_ns"] = strconv.FormatInt(time.Now().UnixNano(), 10)
	logPath, err := s.expand(p.Log)
	if err != nil {
		return tr.OutcomeUnknown, map[string]any{"detail": err.Error()}, false
	}
	stdout, stderr, code, err := Call(ctx, s.node.Run, WrapperLaunch(argv, logPath))
	pid := strings.TrimSpace(stdout)
	if err != nil || code != 0 || !digitsRe.MatchString(pid) {
		return tr.OutcomeUnknown, map[string]any{"detail": "start: " + FirstLine(stderr)}, false
	}
	start, state := ProcStart(ctx, s.node, pid)
	res := map[string]any{"pid": pid, "pid_start": start, "log": logPath}
	if err := x.values(s, res, p.ResultLaunched); err != nil {
		res["detail"] = err.Error()
		return tr.OutcomeUnknown, res, false
	}
	if state != ProcRunning {
		res["detail"] = p.Name + " exited at once, or its identity could not be read"
		return tr.OutcomeUnknown, res, false
	}
	b, _ := json.Marshal(argv)
	for _, kv := range [][2]string{{"pid", pid}, {"pid_start", start}, {"launch_argv", string(b)}, {"exit_file", logPath + ".exit"}} {
		if err := setp(kv[0], kv[1]); err != nil {
			res["detail"] = "recording the process: " + err.Error()
			return tr.OutcomeUnknown, res, false
		}
	}
	s.vals["launched.pid"], s.vals["launched.start"], s.vals["launched.log"] = pid, start, logPath
	for i := range p.Owned {
		if _, detail, ok := x.probe(s, ctx, &p.Owned[i], nil, set); !ok {
			res["detail"] = detail
			return tr.OutcomeUnknown, res, true
		}
	}
	r := p.Ready
	bound := time.Duration(atoi(s.vals[r.WithinParam])) * time.Second
	s.vals["ready.within"] = bound.String()
	end := time.Now().Add(bound)
	for ready := false; !ready; {
		switch ProcState(ctx, s.node, pid, start) {
		case ProcGone:
			res["detail"] = p.Name + " exited before serving"
			return tr.OutcomeUnknown, res, true
		case ProcRunning:
			ready = true
			for i := range r.Probes {
				if _, _, ok := x.probe(s, ctx, &r.Probes[i], nil, set); !ok {
					ready = false
					break
				}
			}
		}
		if ready {
			break
		}
		if time.Now().After(end) {
			res["detail"], _ = s.expand(r.NotReady)
			return tr.OutcomeUnknown, res, true
		}
		if !Pause(ctx, time.Duration(r.EveryMs)*time.Millisecond) {
			res["detail"], _ = s.expand(r.NotReadyByDeadline)
			return tr.OutcomeUnknown, res, true
		}
	}
	for _, k := range sortedKeys(p.OnReadySet) {
		val, err := s.expand(p.OnReadySet[k])
		if err == nil {
			err = set(k, val)
		}
		if err != nil {
			res["detail"] = "on_ready_set " + k + ": " + err.Error()
			return tr.OutcomeUnknown, res, true
		}
	}
	if err := x.values(s, res, p.ResultReady); err != nil {
		res["detail"] = err.Error()
		return tr.OutcomeUnknown, res, true
	}
	for i := range p.After {
		if _, detail, ok := x.probe(s, ctx, &p.After[i], res, set); !ok {
			res["detail"] = detail
			return tr.OutcomeUnknown, res, true
		}
	}
	return tr.OutcomeOK, res, true
}

// values adds typed result values.
func (x *Executor) values(s *scope, res map[string]any, vals map[string]Value) error {
	for _, k := range sortedKeys2(vals) {
		v := vals[k]
		t, err := s.expand(v.Template)
		if err != nil {
			return err
		}
		switch v.Type {
		case "int":
			n, err := strconv.ParseUint(t, 10, 64)
			if err != nil {
				return fmt.Errorf("result %s %q is not an int", k, t)
			}
			res[k] = n
		case "bool":
			b, err := strconv.ParseBool(t)
			if err != nil {
				return fmt.Errorf("result %s %q is not a bool", k, t)
			}
			res[k] = b
		default:
			res[k] = t
		}
	}
	return nil
}

// probe runs one probe: ok, or the outcome and detail that end the step.
func (x *Executor) probe(s *scope, ctx context.Context, pr *Probe, res map[string]any, set func(k, val string) error) (string, string, bool) {
	if pr.When != nil {
		hold, err := s.holds(pr.When)
		if err != nil {
			return tr.OutcomeUnknown, err.Error(), false
		}
		if !hold {
			return "", "", true
		}
	}
	addrs := func() ([]string, error) {
		var out []string
		for _, t := range pr.Addresses {
			v, err := s.expand(t)
			if err != nil {
				return nil, err
			}
			out = append(out, PortOf(v))
		}
		return out, nil
	}
	switch pr.Kind {
	case "command":
		cmd, err := s.command(pr.Command)
		if err != nil {
			return tr.OutcomeUnknown, err.Error(), false
		}
		run := s.node.Run
		if pr.AsRoot {
			run = s.node.RunRoot
		}
		stdout, stderr, code, err := Call(ctx, run, cmd)
		if pr.Decode == nil {
			if err != nil || code != 0 {
				s.vals["exit"], s.vals["stderr_first_line"] = strconv.Itoa(code), FirstLine(stderr)
				msg, _ := s.expand(pr.Failure)
				return tr.OutcomeUnknown, msg, false
			}
			return "", "", true
		}
		if err != nil {
			return tr.OutcomeUnknown, err.Error(), false
		}
		ans, out := decodeSelf(*pr.Decode, s, stdout, code)
		for f, val := range ans {
			s.vals["answer."+f] = fmt.Sprint(val)
		}
		s.vals["stderr"] = stderr
		if out != tr.OutcomeOK {
			msg, _ := s.expand(pr.Decode.Detail)
			return out, msg, false
		}
		for _, k := range sortedKeys(pr.OnOKSet) {
			val, err := s.expand(pr.OnOKSet[k])
			if err == nil {
				err = set(k, val)
			}
			if err != nil {
				return tr.OutcomeUnknown, "on_ok_set " + k + ": " + err.Error(), false
			}
		}
		return "", "", true
	case "ports_free":
		ports, err := addrs()
		if err != nil {
			return tr.OutcomeUnknown, err.Error(), false
		}
		held, err := ListenerPIDs(ctx, s.node, ports)
		if err != nil {
			return tr.OutcomeUnknown, "endpoint check: " + err.Error(), false
		}
		for _, port := range ports {
			if len(held[port]) > 0 {
				return tr.OutcomeUnknown, fmt.Sprintf("endpoint :%s already listens (pids %v): %s not started", port, held[port], s.processName()), false
			}
		}
		return "", "", true
	case "listeners_held":
		ports, err := addrs()
		if err != nil {
			return tr.OutcomeUnknown, err.Error(), false
		}
		owners, err := ListenerPIDs(ctx, s.node, ports)
		if err != nil {
			return tr.OutcomeUnknown, "", false
		}
		for _, port := range ports {
			if !contains(owners[port], s.vals["launched.pid"]) {
				return tr.OutcomeUnknown, "", false
			}
		}
		return "", "", true
	case "json":
		cmd, err := s.command(pr.Command)
		if err != nil {
			return tr.OutcomeUnknown, err.Error(), false
		}
		stdout, _, code, err := Call(ctx, s.node.Run, cmd)
		if err != nil || code != 0 {
			return tr.OutcomeUnknown, "", false
		}
		data, ok := decodeSchema(pr.Schema, stdout)
		if !ok {
			return tr.OutcomeUnknown, "", false
		}
		for _, h := range pr.Holds {
			if !s.hold(h, data) {
				return tr.OutcomeUnknown, "", false
			}
		}
		for _, k := range sortedKeys(pr.Capture) {
			s.vals["ready."+k] = valueText(lookupPath(data, pr.Capture[k]))
		}
		return "", "", true
	case "nvme_connect":
		addr, err := s.expand(pr.Address)
		if err != nil {
			return tr.OutcomeUnknown, err.Error(), false
		}
		nqn, err := s.expand(pr.Subsystem)
		if err != nil {
			return tr.OutcomeUnknown, err.Error(), false
		}
		opts, err := s.expand(pr.Options)
		if err != nil {
			return tr.OutcomeUnknown, err.Error(), false
		}
		if pr.UnlessPresent {
			state, _, why := NVMeScan(ctx, s.node, nqn)
			if state == NVMeUnreadable {
				return tr.OutcomeUnknown, why, false
			}
			if state != NVMeAbsent {
				return "", "", true // the kernel reconnects the controller it kept by itself
			}
		}
		host, port, _ := strings.Cut(addr, ":")
		if why := NVMeConnect(ctx, s.node, host, port, nqn, opts); why != "" {
			return tr.OutcomeUnknown, why, false
		}
		return "", "", true
	case "device":
		nqn, err := s.expand(pr.Subsystem)
		if err != nil {
			return tr.OutcomeUnknown, err.Error(), false
		}
		dev, why := NVMeDevice(ctx, s.node, nqn, time.Duration(pr.WithinSeconds)*time.Second)
		if why != "" {
			return tr.OutcomeUnknown, why, false
		}
		s.vals["device"] = dev
		if res != nil {
			res[pr.Into] = dev
		}
		return "", "", true
	}
	return tr.OutcomeUnknown, "probe kind " + pr.Kind, false
}

func (s *scope) processName() string { return s.procName }

func valueText(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

// hold says whether one condition holds on decoded data.
func (s *scope) hold(h Hold, data map[string]any) bool {
	got := valueText(lookupPath(data, h.Path))
	if h.Equals != nil {
		want, err := s.expand(*h.Equals)
		return err == nil && got == want
	}
	g, err := strconv.ParseUint(got, 10, 64)
	if err != nil {
		return false
	}
	t := h.Above
	if t == "" {
		t = h.AtLeast
	}
	ws, err := s.expand(t)
	if err != nil {
		return false
	}
	w, err := strconv.ParseUint(ws, 10, 64)
	if err != nil {
		return false
	}
	if h.Above != "" {
		return g > w
	}
	return g >= w
}
