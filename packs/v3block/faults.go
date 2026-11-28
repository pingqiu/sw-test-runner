package v3block

// Process faults (runner spec S6, M2): kill a daemon the runner started, by
// the identity it recorded when it started it; start it again with the
// command line it ran with; and observe whether the run is healthy again.
//
// The processes are the ones this pack starts and tracks: the blockmaster
// (started through the run's ownership script, m0-own.sh spawn, and found in
// its ledger WORK/owned as "pid start name") and each volume's holder
// (started by v3_op_attach, identified by the volume's PID and PIDStart).
// Identity is (pid, start time), never a pid alone, and never a name or a
// pattern. A restarted process is recorded in the same ledger, so the run's
// teardown stops it like any other.
//
// kill and restart record invoke and complete like every typed operation
// (verb kill / restart, volume ""). Refused means the runner did nothing: an
// unknown process name, a process that is not running (kill) or still running
// (restart), or an identity it could not read. healthy is an observation.

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/actions"
	"github.com/pingqiu/sw-test-runner/declared"
)

const (
	// specOrderVar holds the volume names in v3_op_spec's order:
	// "blockvolume:<i>" is the holder of volume i (from 0).
	specOrderVar = "v3op_volumes"
	// masterVar holds the blockmaster's identity once a fault touched it.
	masterVar = "v3fault_blockmaster"
)

func init() {
	// No model action commits a process fault: like Guest.Read, these name
	// the environment, not a step of the product.
	opIDs["kill"] = "Env.Kill"
	opIDs["restart"] = "Env.Restart"
	// The store fault (actions.json v3_op_fault_apply_store and
	// v3_op_fault_restore_store): a pause of the store process and its
	// continue, the environment's as a kill is.
	opIDs["fault_apply"] = "Env.StorePause"
	opIDs["fault_restore"] = "Env.StoreContinue"
}

var digits = regexp.MustCompile(`^[0-9]+$`)

// faultProc is one daemon the runner may fault.
type faultProc struct {
	Name      string   `json:"name"`
	PID       string   `json:"pid,omitempty"`
	Start     string   `json:"start,omitempty"`
	KilledPID string   `json:"killed_pid,omitempty"` // the pid the runner killed, while it is still the recorded one
	Argv      []string `json:"argv,omitempty"`       // that process's command line, read before the kill
	vol       *opVolume
}

// ledgerEntry is one line of the ownership script's ledger.
type ledgerEntry struct{ pid, start, name string }

// ledger reads WORK/owned. An unreadable or malformed ledger is an error,
// never an empty one.
func ledger(ctx context.Context, node runner, work string) ([]ledgerEntry, string) {
	if work == "" {
		return nil, "no work directory configured: no ledger of started processes"
	}
	stdout, stderr, code, err := node.Run(ctx, "cat "+shellQuote(work+"/owned"))
	if err != nil || code != 0 {
		return nil, fmt.Sprintf("ledger unreadable (exit %d): %s", code, firstLine(stderr))
	}
	var out []ledgerEntry
	for _, l := range strings.Split(stdout, "\n") {
		f := strings.Fields(l)
		if len(f) == 0 {
			continue
		}
		if len(f) != 3 || !digits.MatchString(f[0]) || !digits.MatchString(f[1]) {
			return nil, "ledger line malformed: " + firstLine(l)
		}
		out = append(out, ledgerEntry{f[0], f[1], f[2]})
	}
	return out, ""
}

func isMasterEntry(name string) bool {
	return name == "blockmaster" || strings.HasPrefix(name, "blockmaster-restart")
}

// latest is the last ledger entry whose name matches.
func latest(entries []ledgerEntry, match func(string) bool) (ledgerEntry, bool) {
	for i := len(entries) - 1; i >= 0; i-- {
		if match(entries[i].name) {
			return entries[i], true
		}
	}
	return ledgerEntry{}, false
}

// resolveProc finds the named process. A reason instead means the runner
// knows no such process: the operation is refused and changes nothing.
func resolveProc(ctx context.Context, actx *tr.ActionContext, node runner, e opEnv, name string) (*faultProc, string) {
	if name == "blockmaster" {
		p := &faultProc{Name: name}
		if raw := actx.Vars[masterVar]; raw != "" {
			if err := json.Unmarshal([]byte(raw), p); err != nil {
				return nil, "blockmaster state: " + err.Error()
			}
			return p, ""
		}
		entries, why := ledger(ctx, node, e.work)
		if why != "" {
			return nil, why
		}
		m, ok := latest(entries, isMasterEntry)
		if !ok {
			return nil, "no blockmaster in the ledger"
		}
		p.PID, p.Start = m.pid, m.start
		return p, ""
	}
	rest, ok := strings.CutPrefix(name, "blockvolume:")
	i, err := strconv.Atoi(rest)
	names := strings.Fields(actx.Vars[specOrderVar])
	if !ok || err != nil || !digits.MatchString(rest) || i >= len(names) {
		return nil, fmt.Sprintf("unknown process %q (blockmaster, or blockvolume:<i> for i below %d)", name, len(names))
	}
	v, err := loadVolume(actx, names[i])
	if err != nil {
		return nil, err.Error()
	}
	p := &faultProc{Name: name, PID: v.PID, Start: v.PIDStart, KilledPID: v.Extra["killed_pid"], vol: v}
	if a := v.Extra["argv"]; a != "" {
		if err := json.Unmarshal([]byte(a), &p.Argv); err != nil {
			return nil, "holder command line: " + err.Error()
		}
	}
	return p, ""
}

// args are a fault's invoke args: the process as asked and, for a holder, the
// volume it resolved to. The record's Volume stays "": a fault touches every
// volume.
func (p *faultProc) args(process string) map[string]any {
	a := map[string]any{"process": process}
	if p != nil && p.vol != nil {
		a["volume"] = p.vol.Name
	}
	return a
}

// save writes the process's state back where resolveProc reads it.
func (p *faultProc) save(actx *tr.ActionContext) {
	if p.vol == nil {
		b, _ := json.Marshal(p)
		actx.Vars[masterVar] = string(b)
		return
	}
	v := p.vol
	v.PID, v.PIDStart = p.PID, p.Start
	if v.Extra == nil {
		v.Extra = map[string]string{}
	}
	delete(v.Extra, "killed_pid")
	delete(v.Extra, "argv")
	if p.KilledPID != "" {
		v.Extra["killed_pid"] = p.KilledPID
		b, _ := json.Marshal(p.Argv)
		v.Extra["argv"] = string(b)
	}
	saveVolume(actx, v)
}

// killCmd: the identity-checked SIGKILL (declared.KillCmd).
func killCmd(proc, pid, start string) string { return declared.KillCmd(proc, pid, start) }

func decodeArgv(b64 string) ([]string, error) { return declared.DecodeArgv(b64) }

// restartMasterOp starts the blockmaster the runner killed with the command
// line it ran with, through the ownership script's spawn (ready when it
// holds its port) and then a master call that answers; then the unit:
// holders that fenced while it was down. A holder's restart is the declared
// v3_op_restart_holder (v3OpRestart hands it there).
func (e opEnv) restartMasterOp(ctx context.Context, actx *tr.ActionContext, node runner, process string, bound time.Duration) (map[string]string, error) {
	p, why := resolveProc(ctx, actx, node, e, process)
	out, err := recordBounded(ctx, actx, e, "restart", "", p.args(process), func(ctx context.Context) (string, map[string]any) {
		switch {
		case why != "":
			return tr.OutcomeRefused, map[string]any{"detail": why}
		case p.vol != nil:
			return tr.OutcomeRefused, map[string]any{"detail": process + " is a holder: v3_op_restart_holder restarts it, nothing started"}
		case p.PID == "" || p.KilledPID != p.PID:
			return tr.OutcomeRefused, map[string]any{"detail": process + " was not killed by the runner: nothing started"}
		case len(p.Argv) == 0:
			return tr.OutcomeRefused, map[string]any{"detail": "the killed process's command line is unknown: nothing started"}
		}
		switch procState(ctx, node, p.PID, p.Start) {
		case procRunning:
			return tr.OutcomeRefused, map[string]any{"detail": process + " is still running: nothing started"}
		case procUnreadable:
			return tr.OutcomeRefused, map[string]any{"detail": "state of the killed process unreadable: nothing started"}
		}
		return e.restartMaster(ctx, actx, node, p, bound)
	})
	if err == nil && p != nil && p.vol == nil {
		err = e.supervise(ctx, actx, node)
	}
	return out, err
}

func (e opEnv) restartMaster(ctx context.Context, actx *tr.ActionContext, node runner, p *faultProc, bound time.Duration) (string, map[string]any) {
	if e.ownScript == "" || e.work == "" || e.owner == "" {
		return tr.OutcomeRefused, map[string]any{"detail": "no ownership script, work directory or owner configured: nothing started"}
	}
	before, why := ledger(ctx, node, e.work)
	if why != "" {
		return tr.OutcomeRefused, map[string]any{"detail": why + ": nothing started"}
	}
	n := 0
	for _, x := range before {
		if isMasterEntry(x.name) {
			n++
		}
	}
	name := fmt.Sprintf("blockmaster-restart%d", n) // its own log: the killed one's stays
	_, stderr, code, err := node.Run(ctx, fmt.Sprintf("bash %s spawn %s %s %s %s -- %s", shellQuote(e.ownScript), shellQuote(e.work), shellQuote(e.owner),
		name, portOf(e.master), strings.Join(quoteAll(p.Argv), " ")))
	res := map[string]any{"name": name}
	// The identity spawn recorded, whatever its exit: it records before it waits.
	after, lwhy := ledger(ctx, node, e.work)
	started, found := latest(after, func(s string) bool { return s == name })
	if found {
		p.PID, p.Start, p.KilledPID, p.Argv = started.pid, started.start, "", nil
		p.save(actx)
		res["pid"], res["pid_start"] = started.pid, started.start
	}
	if err != nil || code != 0 {
		res["detail"] = fmt.Sprintf("spawn exit %d: %s", code, firstLine(stderr))
		if lwhy == "" && !found {
			return tr.OutcomeRefused, res // spawn refused before starting anything
		}
		return tr.OutcomeUnknown, res
	}
	if !found {
		res["detail"] = "spawn succeeded but the ledger names no " + name + " " + lwhy
		return tr.OutcomeUnknown, res
	}
	vol := firstOf(strings.Fields(actx.Vars[specOrderVar]))
	end := time.Now().Add(bound)
	for {
		if !alive(ctx, node, p.PID, p.Start) {
			res["detail"] = "the restarted blockmaster is not running"
			return tr.OutcomeUnknown, res
		}
		if masterAnswers(ctx, node, e, vol) {
			return tr.OutcomeOK, res
		}
		if time.Now().After(end) {
			res["detail"] = fmt.Sprintf("the master does not answer within %s", bound)
			return tr.OutcomeUnknown, res
		}
		if !pause(ctx, 500*time.Millisecond) {
			res["detail"] = "the master does not answer by the runner deadline"
			return tr.OutcomeUnknown, res
		}
	}
}

func firstOf(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[0]
}

// masterAnswers: the master answers a resolve of vol (the call v3_op_attach
// makes) with an answer naming vol, ok or refused. No answer, or one that
// does not confirm the request, is not an answer.
func masterAnswers(ctx context.Context, node runner, e opEnv, vol string) bool {
	if vol == "" {
		return false
	}
	stdout, _, code, err := node.Run(ctx, fmt.Sprintf("%s resolve --master %s --volume %s", shellQuote(e.v3ctl()), shellQuote(e.master), shellQuote(vol)))
	if err != nil {
		return false
	}
	_, r := parseV3ctl(stdout, code, vol)
	return r == tr.OutcomeOK || r == tr.OutcomeRefused
}

// healthCheck is one look. It first places every UNSETTLED volume by
// observation (settle; each placement a "settled" record), then demands:
// the blockmaster runs and answers; every HELD volume serves from a live
// holder (down names processes, not_serving volumes); no holder serves a
// NOT HELD volume (served_not_held); and no volume is in the third state
// (third_state: an UNSETTLED volume observation cannot place).
func (e opEnv) healthCheck(ctx context.Context, actx *tr.ActionContext, node runner) (map[string]any, bool, error) {
	down, notServing, holders := []string{}, []string{}, map[string]any{}
	thirdState, pending, servedNotHeld := []map[string]any{}, []map[string]any{}, []string{}
	names := strings.Fields(actx.Vars[specOrderVar])
	if p, why := resolveProc(ctx, actx, node, e, "blockmaster"); why != "" || !alive(ctx, node, p.PID, p.Start) {
		down = append(down, "blockmaster")
	}
	answers := masterAnswers(ctx, node, e, firstOf(names))
	for i, n := range names {
		v, err := loadVolume(actx, n)
		if err != nil {
			return nil, false, err
		}
		if v.Extra["intent"] == intentUnsettled {
			state, ev, why := e.settle(ctx, node, v)
			if state == "" {
				// Neither consistent state: pending until the recovery
				// deadline, the third state after it.
				left, err := settleDeadline(v)
				if err != nil {
					return nil, false, err
				}
				if left > 0 {
					pending = append(pending, map[string]any{"volume": n, "why": why, "deadline_in_s": left.Seconds()})
				} else {
					thirdState = append(thirdState, map[string]any{"volume": n, "why": why})
				}
			} else {
				setIntent(v, state)
				if state == intentNotHeld {
					v.PID, v.PIDStart = "", ""
				}
				saveVolume(actx, v)
				if err := actx.History.Observe(tr.HistoryObserve, tr.HistoryRecord{Client: "runner", Verb: "settled", Result: map[string]any{"volume": n, "state": state, "evidence": ev}}); err != nil {
					return nil, false, err
				}
			}
		}
		switch v.Extra["intent"] {
		case intentHeld:
			if v.PID != "" && !alive(ctx, node, v.PID, v.PIDStart) {
				down = append(down, fmt.Sprintf("blockvolume:%d", i))
			}
			if st, ok := readStatus(ctx, node, v.Hosts[v.Cur].Status, n); !ok || !st.serving(n) {
				notServing = append(notServing, n)
			}
		case intentNotHeld:
			for _, h := range v.Hosts {
				if st, ok := readStatus(ctx, node, h.Status, n); ok && st.serving(n) {
					servedNotHeld = append(servedNotHeld, n)
					break
				}
			}
		}
		if v.PID != "" {
			holders[n] = liveHolder(ctx, node, v.Hosts[v.Cur].Status)
		}
	}
	healthy := len(down) == 0 && answers && len(notServing) == 0 && len(thirdState) == 0 && len(pending) == 0 && len(servedNotHeld) == 0
	return map[string]any{"healthy": healthy, "down": down, "not_serving": notServing, "master_answers": answers, "holders": holders,
		"third_state": thirdState, "unsettled_pending": pending, "served_not_held": servedNotHeld}, healthy, nil
}

// liveHolder is the process that holds the volume's status listener now, read
// from the host (the status answer names no pid), not from the runner's
// record: {"pid": n, "start": "<start time>"}, or {"error": why} when no
// single process can be named. A holder restarted behind the runner's back
// shows its own pid and start time here.
func liveHolder(ctx context.Context, node runner, statusAddr string) map[string]any {
	port := portOf(statusAddr)
	owners, err := listenerPIDs(ctx, node, []string{port})
	if err != nil {
		return map[string]any{"error": "listeners: " + err.Error()}
	}
	var pids []string // one process may list its listener twice (IPv4 and IPv6)
	for _, p := range owners[port] {
		if !contains(pids, p) {
			pids = append(pids, p)
		}
	}
	if len(pids) != 1 || !digits.MatchString(pids[0]) {
		return map[string]any{"error": fmt.Sprintf("status port :%s held by %v", port, pids)}
	}
	start, state := procStart(ctx, node, pids[0])
	if state != procRunning {
		return map[string]any{"error": fmt.Sprintf("pid %s: %s", pids[0], state)}
	}
	n, _ := strconv.Atoi(pids[0])
	return map[string]any{"pid": n, "start": start}
}

// healthy writes one healthy observation: the first healthy look within wait,
// or the last look when wait runs out (wait 0: one look).
func (e opEnv) healthy(ctx context.Context, actx *tr.ActionContext, node runner, wait time.Duration) (map[string]string, error) {
	start := time.Now()
	var res map[string]any
	var ok bool
	var err error
	for attempts := 1; ; attempts++ {
		if err = e.supervise(ctx, actx, node); err != nil {
			return nil, err
		}
		if res, ok, err = e.healthCheck(ctx, actx, node); err != nil {
			return nil, err
		}
		res["attempts"] = attempts
		if ok || time.Since(start) >= wait || !pause(ctx, 500*time.Millisecond) {
			break
		}
	}
	res["elapsed_ms"] = time.Since(start).Milliseconds()
	if err := actx.History.Observe(tr.HistoryObserve, tr.HistoryRecord{Client: "runner", Verb: "healthy", Result: res}); err != nil {
		return nil, err
	}
	actx.Log("  v3_op_healthy: %v", res)
	return map[string]string{"value": tr.OutcomeOK, "healthy": strconv.FormatBool(ok)}, nil
}

// The holder's unit. In a deployment the product's systemd unit restarts a
// holder that exits by itself; here the runner plays that unit, with the
// policy read from the product's shipped unit file (unit.go). Every holder is
// launched through a wrapper that keeps the holder's own pid and start time
// as its identity (it is the wrapper's child, not the wrapper) and writes the
// holder's exit status to a per-launch file; the supervision step reads that
// file, never the holder's log.
// holderLaunchCmd starts argv under the unit wrapper and prints the holder's
// pid. The wrapper (its own session) starts the holder with its output in
// logPath, writes its pid to logPath.pid, waits for it and writes
// "<wait status> <unix ns>" to logPath.exit (each file written whole, by a
// rename). A wait status above 128 is a signal (128+n), as the shell reports it.
// (declared.WrapperLaunch, the launcher of every declared process.)
func holderLaunchCmd(argv []string, logPath string) string {
	return declared.WrapperLaunch(argv, logPath)
}

// setLaunch records the running holder's command line and exit-status file.
func setLaunch(v *opVolume, argv []string, logPath string) {
	if v.Extra == nil {
		v.Extra = map[string]string{}
	}
	b, _ := json.Marshal(argv)
	v.Extra["launch_argv"], v.Extra["exit_file"] = string(b), logPath+".exit"
}

// readExit reads a holder's exit file: its wait status and how long ago it
// exited, by the host's clock. why is set when there is no status (yet).
func readExit(ctx context.Context, node runner, file string) (status int, ago time.Duration, why string) {
	stdout, stderr, code, err := node.Run(ctx, fmt.Sprintf(`if [ -e %[1]s ]; then cat %[1]s || exit 3; else echo none; fi; date +%%s%%N`, shellQuote(file)))
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if err != nil || code != 0 || len(lines) != 2 {
		return 0, 0, fmt.Sprintf("exit file unreadable (exit %d): %s", code, firstLine(stderr))
	}
	if lines[0] == "none" {
		return 0, 0, "no exit status written"
	}
	f := strings.Fields(lines[0])
	now, err1 := strconv.ParseInt(strings.TrimSpace(lines[1]), 10, 64)
	if len(f) != 2 || err1 != nil {
		return 0, 0, "exit file malformed: " + firstLine(lines[0])
	}
	s, err2 := strconv.Atoi(f[0])
	at, err3 := strconv.ParseInt(f[1], 10, 64)
	if err2 != nil || err3 != nil {
		return 0, 0, "exit file malformed: " + firstLine(lines[0])
	}
	return s, time.Duration(now - at), ""
}

// A volume's intended state has three values, set by the answer of the last
// holder-lifecycle operation on it: HELD when an attach (also the one on a
// move's target) answered ok; NOT HELD when a stop or a move (its hand-over)
// answered ok; UNSETTLED when such an operation answered unknown (an unknown
// stop that did not take effect leaves a serving holder). A refusal changes
// nothing. An UNSETTLED volume is placed by observation at a healthy look
// (settle) and the placement is a "settled" record in the history. A
// restart, by the schedule or the unit, is not a lifecycle operation.
const (
	intentHeld      = "held"
	intentNotHeld   = "not_held"
	intentUnsettled = "unsettled"
)

func setIntent(v *opVolume, intent string) {
	if v.Extra == nil {
		v.Extra = map[string]string{}
	}
	v.Extra["intent"] = intent
}

// lifecycle records a lifecycle operation's answer as the volume's intended
// state: ok sets target, unknown leaves it UNSETTLED, a refusal changes
// nothing. Used as lifecycle(v, target)(record(...)).
func lifecycle(v *opVolume, target string) func(map[string]string, error) (map[string]string, error) {
	return func(out map[string]string, err error) (map[string]string, error) {
		if err == nil {
			switch out["value"] {
			case tr.OutcomeOK:
				setIntent(v, target)
			case tr.OutcomeUnknown:
				setIntent(v, intentUnsettled)
				v.Extra["unsettled_at"] = strconv.FormatInt(nowFn().UnixNano(), 10) // its completion
			}
		}
		return out, err
	}
}

// held: the volume is to be served, so its holder is the unit's to restart
// and healthy demands that it serve.
func held(v *opVolume) bool { return v.Extra["intent"] == intentHeld }

// nowFn is the runner's clock for recovery deadlines (a test sets it).
var nowFn = time.Now

// recoveryTerms are the product's recovery terms (product/checks.json
// recovery, each with its cite): regrant and start_wait.
func recoveryTerms() (regrant, startWait time.Duration, err error) {
	var c struct {
		Recovery []struct {
			Term    string  `json:"term"`
			Seconds float64 `json:"seconds"`
			Cite    string  `json:"cite"`
		} `json:"recovery"`
	}
	if err := json.Unmarshal(checksJSON, &c); err != nil {
		return 0, 0, err
	}
	for _, r := range c.Recovery {
		d := time.Duration(r.Seconds * float64(time.Second))
		if r.Cite == "" || d <= 0 {
			continue
		}
		switch r.Term {
		case "regrant":
			regrant = d
		case "start_wait":
			startWait = d
		}
	}
	if regrant == 0 || startWait == 0 {
		return 0, 0, fmt.Errorf("product/checks.json states no cited recovery regrant and start_wait")
	}
	return regrant, startWait, nil
}

// settleDeadline is how long an UNSETTLED volume may stay in neither
// consistent state: until its lifecycle operation's completion + regrant +
// start_wait, or, when a kill of its holder completed before that,
// max(kill completion + regrant, operation completion) + start_wait (the
// checker's C6 formula). Negative or zero: past the deadline.
func settleDeadline(v *opVolume) (time.Duration, error) {
	regrant, startWait, err := recoveryTerms()
	if err != nil {
		return 0, err
	}
	end, err := strconv.ParseInt(v.Extra["unsettled_at"], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("volume %s is unsettled with no completion time", v.Name)
	}
	base := time.Unix(0, end).Add(regrant)
	if k, kerr := strconv.ParseInt(v.Extra["killed_at"], 10, 64); kerr == nil && k <= end {
		base = time.Unix(0, k).Add(regrant)
		if e := time.Unix(0, end); e.After(base) {
			base = e
		}
	}
	return base.Add(startWait).Sub(nowFn()), nil
}

// wsLeaseState reads `ops workspace show`: the lease state ("granted",
// "revoking", "expired", "none") and the recorded ACK ("none" or the ack).
func wsLeaseState(out string) (lease, ack string) {
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		if s, ok := strings.CutPrefix(l, "lease state: "); ok {
			lease = s
		}
		if s, ok := strings.CutPrefix(l, "recorded ACK: "); ok {
			ack = s
		}
	}
	return lease, ack
}

// settle places an UNSETTLED volume by observation: its recorded holder
// still runs and serves it from its own status listener (the operation did
// not take effect): HELD; the holder is gone and the master's workspace
// shows the lease revoked, acknowledged or lapsed: NOT HELD. Anything else
// is the third state, and why says which.
func (e opEnv) settle(ctx context.Context, node runner, v *opVolume) (state string, evidence map[string]any, why string) {
	h := v.Hosts[v.Cur]
	if v.PID != "" {
		switch procState(ctx, node, v.PID, v.PIDStart) {
		case procRunning:
			st, ok := readStatus(ctx, node, h.Status, v.Name)
			live := liveHolder(ctx, node, h.Status)
			if ok && st.serving(v.Name) && fmt.Sprint(live["pid"]) == v.PID && live["start"] == v.PIDStart {
				return intentHeld, map[string]any{"pid": v.PID, "start": v.PIDStart, "epoch": st.Epoch, "serving": true}, ""
			}
			return "", nil, fmt.Sprintf("holder pid %s alive but not serving from its own status listener", v.PID)
		case procUnreadable:
			return "", nil, fmt.Sprintf("holder pid %s: state unreadable", v.PID)
		}
	}
	out, _, code, err := e.workspace(ctx, node, v.Name, "show")
	if err != nil || code != 0 {
		return "", nil, fmt.Sprintf("holder gone; workspace show exit %d", code)
	}
	lease, ack := wsLeaseState(out)
	ev := map[string]any{"holder_gone": v.PID, "lease": lease, "ack": ack}
	switch {
	case lease == "":
		return "", nil, "holder gone; workspace show names no lease state"
	case lease == "revoking" || lease == "expired" || lease == "none" || (ack != "" && ack != "none"):
		return intentNotHeld, ev, ""
	}
	return "", nil, fmt.Sprintf("holder gone; workspace lease still %s, no ACK", lease)
}

// supervise is the unit: every held volume whose holder (not one the runner
// killed) exited by itself with a restartable status is launched again with
// its recorded command line, RestartSec after the exit. Each relaunch is an
// observe record "unit_restart". A holder whose status is not written yet is
// looked at again at the next step.
func (e opEnv) supervise(ctx context.Context, actx *tr.ActionContext, node runner) error {
	pol, err := loadUnitPolicy(ctx, actx, node, e)
	if err != nil {
		return err
	}
	for _, n := range strings.Fields(actx.Vars[specOrderVar]) {
		v, err := loadVolume(actx, n)
		if err != nil {
			return err
		}
		if !held(v) || v.PID == "" || v.Extra["exit_file"] == "" || v.Extra["killed_pid"] == v.PID {
			continue
		}
		if procState(ctx, node, v.PID, v.PIDStart) != procGone {
			continue
		}
		status, ago, why := readExit(ctx, node, v.Extra["exit_file"])
		if why != "" || !pol.restartable(status) {
			continue
		}
		var argv []string
		if json.Unmarshal([]byte(v.Extra["launch_argv"]), &argv) != nil || len(argv) == 0 {
			continue
		}
		if wait := pol.restartSec() - ago; wait > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
		}
		oldPID, oldStart := v.PID, v.PIDStart
		out, res := e.launchHolder(ctx, node, v, v.Hosts[v.Cur], argv, 0, true)
		saveVolume(actx, v)
		r := map[string]any{"volume": n, "exit_status": status, "exited_ms_before": ago.Milliseconds(), "restart_sec": pol.RestartSec,
			"old_pid": oldPID, "old_start": oldStart, "launch": out}
		if v.PID != oldPID {
			r["new_pid"], r["new_start"] = v.PID, v.PIDStart
		}
		if d, ok := res["detail"]; ok {
			r["detail"] = d
		}
		if err := actx.History.Observe(tr.HistoryObserve, tr.HistoryRecord{Client: "runner", Verb: "unit_restart", Result: r}); err != nil {
			return err
		}
		actx.Log("  unit_restart(%s): exit %d, pid %s -> %s (%s)", n, status, oldPID, v.PID, out)
	}
	return nil
}

// v3OpRestart starts a killed daemon again (product/actions.json lists it
// as an exception): the blockmaster here; every other process through the
// declared v3_op_restart_holder, unchanged. Params: process, ready_seconds
// (60).
func v3OpRestart(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
	if optionalParam(act, "process") != "blockmaster" {
		if act.Action == "" {
			act.Action = "v3_op_restart"
		}
		return blockActions.Handler("v3_op_restart_holder")(ctx, actx, act)
	}
	node, e, err := opConfig(actx, act, "v3_op_restart")
	if err != nil {
		return nil, err
	}
	return e.restartMasterOp(ctx, actx, node, optionalParam(act, "process"), time.Duration(actions.ParseInt(optionalParam(act, "ready_seconds"), 60))*time.Second)
}

// singleLook writes one healthy observation of exactly one look, with no
// wait, marked result.look "single" (a short master outage's looks).
func (e opEnv) singleLook(ctx context.Context, actx *tr.ActionContext, node runner) (map[string]string, error) {
	start := time.Now()
	if err := e.supervise(ctx, actx, node); err != nil {
		return nil, err
	}
	res, ok, err := e.healthCheck(ctx, actx, node)
	if err != nil {
		return nil, err
	}
	res["look"], res["attempts"], res["elapsed_ms"] = "single", 1, time.Since(start).Milliseconds()
	if err := actx.History.Observe(tr.HistoryObserve, tr.HistoryRecord{Client: "runner", Verb: "healthy", Result: res}); err != nil {
		return nil, err
	}
	actx.Log("  v3_op_healthy look=single: %v", res)
	down, _ := res["down"].([]string)
	return map[string]string{"value": tr.OutcomeOK, "healthy": strconv.FormatBool(ok), "down": strings.Join(down, " ")}, nil
}

// v3OpHealthy writes the healthy observation. Params: wait_seconds (0), look
// ("single": one look, no wait, recorded as such).
func v3OpHealthy(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
	look := optionalParam(act, "look")
	if look != "" && look != "single" {
		return nil, fmt.Errorf("v3_op_healthy: look %q (single, or absent)", look)
	}
	node, e, err := opConfig(actx, act, "v3_op_healthy")
	if err != nil {
		return nil, err
	}
	// A healthy point: look at every abandoned child once.
	if err := declared.Check(ctx, node, actx.History, false); err != nil {
		return nil, err
	}
	if look == "single" {
		return e.singleLook(ctx, actx, node)
	}
	return e.healthy(ctx, actx, node, time.Duration(actions.ParseInt(optionalParam(act, "wait_seconds"), 0))*time.Second)
}
