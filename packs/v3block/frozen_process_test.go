package v3block

// Frozen copies of the process Go handlers that product/actions.json's
// process actions replaced (v3_op_attach, v3_op_kill, v3_op_restart), as
// they were before the move (feat/v3block-m2 at 4fb5f3d), with frozen copies
// of every primitive the move changed (the process identity reads, the
// listener read, the wrapper launch, the kill shell, the NVMe connect), so
// the old behaviour cannot drift with the new code. The helpers the move
// left as they were (resolveProc, faultProc.save, setLaunch, readStatus,
// recordOwned, lifecycle, restartMaster, supervise) are called live. The
// equivalence tests (process_equiv_test.go) run each against its declared
// action. Test code only: nothing registers them.

import (
	"context"
	"encoding/base64"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/actions"
)

func listenerPIDsFrozen(ctx context.Context, node runner, ports []string) (map[string][]string, error) {
	stdout, stderr, code, err := node.RunRoot(ctx, "ss -Hltnp")
	if err != nil || code != 0 {
		return nil, fmt.Errorf("ss exit %d: %s", code, firstLine(stderr))
	}
	out := map[string][]string{}
	want := map[string]bool{}
	for _, p := range ports {
		want[p] = true
	}
	pidRe := regexp.MustCompile(`pid=([0-9]+)`)
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
			out[port] = append(out[port], "?")
		}
		for _, m := range pids {
			out[port] = append(out[port], m[1])
		}
	}
	return out, nil
}

func procStartCmdFrozen(proc, pid string) string {
	return fmt.Sprintf(`if [ -e %[1]s/%[2]s ]; then s=$(cat %[1]s/%[2]s/stat) || exit 3; s=${s##*) }; set -- $s; [ -n "${20}" ] || exit 3; echo "start ${20}"; else echo absent; fi`, proc, pid)
}

func procStartFrozen(ctx context.Context, node runner, pid string) (string, string) {
	if !regexp.MustCompile(`^[0-9]+$`).MatchString(pid) {
		return "", "gone"
	}
	stdout, _, code, err := node.Run(ctx, procStartCmdFrozen("/proc", pid))
	out := strings.TrimSpace(stdout)
	switch {
	case err != nil || code != 0:
		return "", "unreadable"
	case out == "absent":
		return "", "gone"
	case regexp.MustCompile(`^start [0-9]+$`).MatchString(out):
		return out[6:], "running"
	}
	return "", "unreadable"
}

func procStateFrozen(ctx context.Context, node runner, pid, start string) string {
	if start == "" {
		return "unreadable"
	}
	got, state := procStartFrozen(ctx, node, pid)
	if state == "running" && got != start {
		return "gone"
	}
	return state
}

func waitGoneFrozen(ctx context.Context, node runner, pid, start string, bound time.Duration) string {
	end := time.Now().Add(bound)
	last := ""
	for {
		switch last = procStateFrozen(ctx, node, pid, start); last {
		case "gone":
			return ""
		}
		if time.Now().After(end) {
			if last == "unreadable" {
				return fmt.Sprintf("pid %s: its state could not be read within %s", pid, bound)
			}
			return fmt.Sprintf("pid %s still running after %s", pid, bound)
		}
		if !pause(ctx, 250*time.Millisecond) {
			return fmt.Sprintf("pid %s: not gone by the runner deadline", pid)
		}
	}
}

func holderLaunchCmdFrozen(argv []string, logPath string) string {
	pidf, exitf := logPath+".pid", logPath+".exit"
	wrap := fmt.Sprintf(`"$@" >%[1]s 2>&1 </dev/null & p=$!; echo "$p" >%[2]s.tmp && mv %[2]s.tmp %[2]s; wait "$p"; s=$?; echo "$s $(date +%%s%%N)" >%[3]s.tmp && mv %[3]s.tmp %[3]s`,
		shellQuote(logPath), shellQuote(pidf), shellQuote(exitf))
	return fmt.Sprintf(`setsid sh -c %s sh %s >/dev/null 2>&1 </dev/null & for i in $(seq 1 100); do [ -s %[3]s ] && exec cat %[3]s; sleep 0.05; done; echo "no pid file %[3]s" >&2; exit 1`,
		shellQuote(wrap), strings.Join(quoteAll(argv), " "), shellQuote(pidf))
}

func killCmdFrozen(proc, pid, start string) string {
	return fmt.Sprintf(`if [ -e %[1]s/%[2]s ]; then s=$(cat %[1]s/%[2]s/stat) || exit 3; s=${s##*) }; set -- $s; [ -n "${20}" ] || exit 3; [ "${20}" = %[3]s ] || { echo gone; exit 0; }; a=$(base64 -w0 < %[1]s/%[2]s/cmdline) || exit 3; kill -KILL %[2]s || exit 4; echo "sent $a"; else echo gone; fi`, proc, pid, start)
}

func decodeArgvFrozen(b64 string) ([]string, error) {
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

func nvmeConnectFrozen(ctx context.Context, node runner, host, port, nqn, extra string) string {
	if !regexp.MustCompile(`^[0-9.]+$`).MatchString(host) || !regexp.MustCompile(`^[0-9]+$`).MatchString(port) || !regexp.MustCompile(`^[A-Za-z0-9.:_-]+$`).MatchString(nqn) {
		return "nvme connect: malformed address or nqn"
	}
	if !regexp.MustCompile(`^(,[a-z_]+=-?[0-9]+)*$`).MatchString(extra) {
		return "nvme connect: malformed options " + extra
	}
	opts := fmt.Sprintf("transport=tcp,traddr=%s,trsvcid=%s,nqn=%s%s", host, port, nqn, extra)
	if _, stderr, code, err := node.RunRoot(ctx, fmt.Sprintf("printf %%s %s > /dev/nvme-fabrics", shellQuote(opts))); err != nil || code != 0 {
		return "nvme connect: " + firstLine(stderr)
	}
	return ""
}

func (e opEnv) volumeArgsFrozen(v *opVolume, h vsHost, blocks uint64) []string {
	args := []string{"--master", e.master, "--server-id", h.Server, "--volume-id", v.Name, "--replica-id", "r1",
		"--layout-id", v.LayoutID, "--data-addr", h.Data, "--ctrl-addr", h.Ctrl, "--status-addr", h.Status,
		"--durable-root", e.root + "/" + h.Server, "--durable-impl", "walstore-chunk",
		"--durable-blocks", strconv.FormatUint(blocks, 10), "--durable-blocksize", "4096",
		"--chunk-filer", e.filer, "--chunk-path", e.chunkPath, "--chunk-control-endpoint", "--workspace-lease",
		"--nvme-listen", h.NVMe, "--nvme-subsysnqn", v.NQN, "--chunk-capture-interval", "0", "--chunk-snapshot-schedule", "off"}
	if e.lockDirs {
		args = append([]string{"--volume-lock-dir-test-only", e.root + "/" + h.Server + ".locks"}, args...)
	}
	return args
}

func v3OpAttachFrozen(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
	node, e, v, err := opBegin(actx, act, "v3_op_attach")
	if err != nil {
		return nil, err
	}
	if h := optionalParam(act, "host"); h != "" {
		i, perr := strconv.Atoi(h)
		if perr != nil || i < 0 || i >= len(v.Hosts) {
			return nil, fmt.Errorf("v3_op_attach: host %q", h)
		}
		v.Cur = i
	}
	blocks := uint64(actions.ParseInt(optionalParam(act, "blocks"), 4096))
	bound := time.Duration(actions.ParseInt(optionalParam(act, "ready_seconds"), 60)) * time.Second
	h := v.Hosts[v.Cur]
	defer saveVolume(actx, v)
	return lifecycle(v, intentHeld)(recordBounded(ctx, actx, e, "attach", v.Name, map[string]any{"server": h.Server, "min_epoch": v.Epoch, "guest_connect": e.connect.Label}, func(ctx context.Context) (string, map[string]any) {
		if v.LayoutID == "" {
			stdout, _, code, err := node.Run(ctx, fmt.Sprintf("%s resolve --master %s --volume %s", shellQuote(e.v3ctl()), shellQuote(e.master), shellQuote(v.Name)))
			if err != nil {
				return fail(err.Error())
			}
			a, r := parseV3ctl(stdout, code, v.Name)
			if r != tr.OutcomeOK {
				return r, map[string]any{"detail": "resolve: " + a.Error}
			}
			v.LayoutID = a.LayoutID
		}
		return e.launchHolderFrozen(ctx, node, v, h, append([]string{e.bin + "/blockvolume"}, e.volumeArgsFrozen(v, h, blocks)...), bound, false)
	}))
}

func (e opEnv) launchHolderFrozen(ctx context.Context, node runner, v *opVolume, h vsHost, argv []string, bound time.Duration, restart bool) (string, map[string]any) {
	ports := []string{portOf(h.Data), portOf(h.Ctrl), portOf(h.Status), portOf(h.NVMe)}
	held, err := listenerPIDsFrozen(ctx, node, ports)
	if err != nil {
		return fail("endpoint check: " + err.Error())
	}
	for _, p := range ports {
		if len(held[p]) > 0 {
			return fail(fmt.Sprintf("endpoint :%s already listens (pids %v): blockvolume not started", p, held[p]))
		}
	}
	mk := "mkdir -p " + shellQuote(e.root+"/"+h.Server)
	if e.lockDirs {
		mk += " " + shellQuote(e.root+"/"+h.Server+".locks")
	}
	if _, _, code, err := node.Run(ctx, mk); err != nil || code != 0 {
		return fail("mkdir")
	}
	logPath := fmt.Sprintf("%s/%s.%d.log", e.root, h.Server, time.Now().UnixNano())
	stdout, stderr, code, err := node.Run(ctx, holderLaunchCmdFrozen(argv, logPath))
	pid := strings.TrimSpace(stdout)
	if err != nil || code != 0 || !regexp.MustCompile(`^[0-9]+$`).MatchString(pid) {
		return fail("start: " + firstLine(stderr))
	}
	start, state := procStartFrozen(ctx, node, pid)
	res := map[string]any{"pid": pid, "pid_start": start, "log": logPath, "layout_id": v.LayoutID, "guest_connect": e.connect.Label}
	if state != "running" {
		res["detail"] = "blockvolume exited at once, or its identity could not be read"
		return tr.OutcomeUnknown, res
	}
	v.PID, v.PIDStart = pid, start
	setLaunch(v, argv, logPath)
	if why := e.recordOwned(ctx, node, "blockvolume-"+h.Server, pid); why != "" {
		res["detail"] = why
		return tr.OutcomeUnknown, res
	}
	end := time.Now().Add(bound)
	for {
		switch procStateFrozen(ctx, node, pid, start) {
		case "gone":
			res["detail"] = "blockvolume exited before serving"
			return tr.OutcomeUnknown, res
		case "running":
			if st, ok := readStatus(ctx, node, h.Status, v.Name); ok && st.serving(v.Name) && (st.Epoch > v.Epoch || restart && st.Epoch == v.Epoch) {
				owners, err := listenerPIDsFrozen(ctx, node, []string{portOf(h.Status), portOf(h.NVMe)})
				if err == nil && contains(owners[portOf(h.Status)], pid) && contains(owners[portOf(h.NVMe)], pid) {
					v.Epoch, res["epoch"] = st.Epoch, st.Epoch
					connect := true
					if restart {
						state, _, why := nvmeScanFrozen(ctx, node, v.NQN)
						if state == "unreadable" {
							res["detail"] = why
							return tr.OutcomeUnknown, res
						}
						connect = state == "absent"
					}
					host, port, _ := strings.Cut(h.NVMe, ":")
					if connect {
						if why := nvmeConnectFrozen(ctx, node, host, port, v.NQN, e.connect.Options); why != "" {
							res["detail"] = why
							return tr.OutcomeUnknown, res
						}
					}
					dev, why := deviceOfFrozen(ctx, node, v.NQN, 20*time.Second)
					if why != "" {
						res["detail"] = why
						return tr.OutcomeUnknown, res
					}
					res["device"] = dev
					return tr.OutcomeOK, res
				}
			}
		}
		if time.Now().After(end) {
			res["detail"] = fmt.Sprintf("not serving above epoch %d from its own listeners within %s", v.Epoch, bound)
			return tr.OutcomeUnknown, res
		}
		if !pause(ctx, 250*time.Millisecond) {
			res["detail"] = fmt.Sprintf("not serving above epoch %d from its own listeners by the runner deadline", v.Epoch)
			return tr.OutcomeUnknown, res
		}
	}
}

func (e opEnv) killFrozen(ctx context.Context, actx *tr.ActionContext, node runner, process string, bound time.Duration) (map[string]string, error) {
	p, why := resolveProc(ctx, actx, node, e, process)
	args := p.args(process)
	args["signal"] = "KILL"
	if p != nil && digits.MatchString(p.PID) {
		n, _ := strconv.Atoi(p.PID)
		args["pid"] = n
	}
	return recordBounded(ctx, actx, e, "kill", "", args, func(ctx context.Context) (string, map[string]any) {
		switch {
		case why != "":
			return tr.OutcomeRefused, map[string]any{"detail": why}
		case p.PID == "":
			return tr.OutcomeRefused, map[string]any{"detail": process + " is not running (no process recorded)"}
		case !digits.MatchString(p.PID) || !digits.MatchString(p.Start):
			return tr.OutcomeRefused, map[string]any{"detail": "no identity recorded for " + process + ": nothing sent"}
		}
		stdout, stderr, code, err := node.Run(ctx, killCmdFrozen("/proc", p.PID, p.Start))
		out := strings.TrimSpace(stdout)
		res := map[string]any{"pid": p.PID, "pid_start": p.Start}
		switch {
		case err != nil:
			return fail(err.Error())
		case code == 0 && out == "gone":
			res["detail"] = process + " is not running: nothing sent"
			return tr.OutcomeRefused, res
		case code == 3:
			res["detail"] = "identity unreadable: nothing sent"
			return tr.OutcomeRefused, res
		case code != 0 || (out != "sent" && !strings.HasPrefix(out, "sent ")):
			res["detail"] = fmt.Sprintf("kill exit %d: %s %s", code, firstLine(out), firstLine(stderr))
			return tr.OutcomeUnknown, res
		}
		argv, aerr := decodeArgvFrozen(strings.TrimSpace(strings.TrimPrefix(out, "sent")))
		p.KilledPID, p.Argv = p.PID, argv
		if aerr != nil {
			p.Argv = nil
			res["argv_error"] = aerr.Error()
		}
		p.save(actx)
		if p.vol != nil {
			p.vol.Extra["killed_at"] = strconv.FormatInt(nowFn().UnixNano(), 10)
			defer saveVolume(actx, p.vol)
		}
		if why := waitGoneFrozen(ctx, node, p.PID, p.Start, bound); why != "" {
			res["detail"] = why
			return tr.OutcomeUnknown, res
		}
		return tr.OutcomeOK, res
	})
}

func (e opEnv) restartFrozen(ctx context.Context, actx *tr.ActionContext, node runner, process string, bound time.Duration) (map[string]string, error) {
	p, why := resolveProc(ctx, actx, node, e, process)
	out, err := recordBounded(ctx, actx, e, "restart", "", p.args(process), func(ctx context.Context) (string, map[string]any) {
		switch {
		case why != "":
			return tr.OutcomeRefused, map[string]any{"detail": why}
		case p.PID == "" || p.KilledPID != p.PID:
			return tr.OutcomeRefused, map[string]any{"detail": process + " was not killed by the runner: nothing started"}
		case len(p.Argv) == 0:
			return tr.OutcomeRefused, map[string]any{"detail": "the killed process's command line is unknown: nothing started"}
		}
		switch procStateFrozen(ctx, node, p.PID, p.Start) {
		case "running":
			return tr.OutcomeRefused, map[string]any{"detail": process + " is still running: nothing started"}
		case "unreadable":
			return tr.OutcomeRefused, map[string]any{"detail": "state of the killed process unreadable: nothing started"}
		}
		if p.vol != nil {
			v := p.vol
			out, res := e.launchHolderFrozen(ctx, node, v, v.Hosts[v.Cur], p.Argv, bound, true)
			if v.PID != p.PID {
				p.PID, p.Start, p.KilledPID, p.Argv = v.PID, v.PIDStart, "", nil
			}
			p.save(actx)
			return out, res
		}
		return e.restartMaster(ctx, actx, node, p, bound)
	})
	if err == nil && p != nil && p.vol == nil {
		err = e.supervise(ctx, actx, node)
	}
	return out, err
}

func v3OpKillFrozen(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
	node, e, err := opConfig(actx, act, "v3_op_kill")
	if err != nil {
		return nil, err
	}
	return e.killFrozen(ctx, actx, node, optionalParam(act, "process"), time.Duration(actions.ParseInt(optionalParam(act, "kill_seconds"), 20))*time.Second)
}

func v3OpRestartFrozen(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
	node, e, err := opConfig(actx, act, "v3_op_restart")
	if err != nil {
		return nil, err
	}
	return e.restartFrozen(ctx, actx, node, optionalParam(act, "process"), time.Duration(actions.ParseInt(optionalParam(act, "ready_seconds"), 60))*time.Second)
}
