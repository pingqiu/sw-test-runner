package v3block

// Typed operations (runner spec S1): one action per operator operation, on
// ONE volume. Each calls the operator surface (sw-block; where the CLI has no
// command, the master call it makes, through v3ctl), maps the answer to ok /
// refused / unknown (outcome.go), and RECORDS invoke and complete in the run's
// history. An action decides nothing about the product: it returns an error
// only when it cannot record (no history, bad parameters). A refused or
// unknown operation is a history fact; the checkers judge it after the run.
//
// The `op` of each record is the Module.Action id of the model-to-code trace
// that commits the operation (opIDs); the scenario loops for many volumes.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/actions"
)

// opIDs: verb -> the Module.Action recorded as op.
var opIDs = map[string]string{
	"create":          "VolumeCreate.MarkReady",
	"attach":          "HolderInterface.Start",
	"write":           "HolderInterface.Write",
	"read":            "Guest.Read",  // a read changes no state; no model has an action for it
	"flush":           "Guest.Flush", // the guest's fsync of the device; no model has an action for it yet
	"snapshot":        "SnapshotRemoval.Commit",
	"snapshot_delete": "SnapshotRemoval.DeleteSnap",
	"restore":         "RestoreTarget.CreateHead",
	"release":         "RestoreTarget.OpRelease",
	"stop":            "HolderInterface.StopCommit",
	"move":            "MoveAdmission.IssueClean",
	"delete":          "MoveAdmission.DelMark",
}

// opEnv is the configuration every operation shares, from params or vars.
type opEnv struct {
	bin, master, filer, chunkPath, root, client string
	// The run's ownership (m0-own.sh): its script, work directory and token.
	ownScript, work, owner string
	// expect is the step's declared outcome ("refused"), recorded in its
	// invocation so the checkers know which refusal the scenario expected.
	// Per step, never per verb: any other refusal still fails the run.
	expect   string
	lockDirs bool
}

func (e opEnv) cli() string   { return e.bin + "/sw-block" }
func (e opEnv) v3ctl() string { return e.bin + "/v3ctl" }

// opVolume is the runner's own state of one volume between operations: where
// it may be served and by which process. It is not a judgment of anything.
type opVolume struct {
	Name     string            `json:"name"`
	NQN      string            `json:"nqn"`
	LayoutID string            `json:"layout_id,omitempty"`
	Hosts    []vsHost          `json:"hosts"`
	Cur      int               `json:"cur"`
	PID      string            `json:"pid,omitempty"`
	PIDStart string            `json:"pid_start,omitempty"` // with PID, the holder's identity
	Epoch    uint64            `json:"epoch,omitempty"`
	Written  map[string]bool   `json:"written,omitempty"` // lbas a write was attempted on
	Extra    map[string]string `json:"extra,omitempty"`
}

// vsHost is one simulated host of a volume: its server id and endpoints.
type vsHost struct {
	Server string `json:"server"`
	Data   string `json:"data"`
	Ctrl   string `json:"ctrl"`
	Status string `json:"status"`
	NVMe   string `json:"nvme"`
}

func volVar(name string) string { return "v3op_vol_" + name }

func loadVolume(actx *tr.ActionContext, name string) (*opVolume, error) {
	raw := actx.Vars[volVar(name)]
	if raw == "" {
		return nil, fmt.Errorf("volume %q is not in the spec (run v3_op_spec first)", name)
	}
	var v opVolume
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return nil, fmt.Errorf("volume %q state: %w", name, err)
	}
	return &v, nil
}

func saveVolume(actx *tr.ActionContext, v *opVolume) {
	b, _ := json.Marshal(v)
	actx.Vars[volVar(v.Name)] = string(b)
}

var varRef = regexp.MustCompile(`\{\{\s*(\w+)\s*\}\}`)

// opParam is a param, else a var; a var's value may name other vars
// ({{ work }}/bin in the scenario's env), which the engine leaves as they are
// in vars, so they are expanded here (bounded; an unknown name stays).
func opParam(act tr.Action, actx *tr.ActionContext, k string) string {
	v := requireParam(act, actx, k)
	for i := 0; i < 4 && strings.Contains(v, "{{"); i++ {
		v = varRef.ReplaceAllStringFunc(v, func(m string) string {
			if x, ok := actx.Vars[varRef.FindStringSubmatch(m)[1]]; ok {
				return x
			}
			return m
		})
	}
	return v
}

// opBegin reads the shared configuration and the volume, and refuses to run
// without a history to record into.
func opBegin(actx *tr.ActionContext, act tr.Action, name string) (runner, opEnv, *opVolume, error) {
	node, e, err := opConfig(actx, act, name)
	if err != nil {
		return nil, opEnv{}, nil, err
	}
	vol := requireParam(act, actx, "volume")
	if vol == "" {
		return nil, opEnv{}, nil, fmt.Errorf("%s: volume is required", name)
	}
	v, err := loadVolume(actx, vol)
	if err != nil {
		return nil, opEnv{}, nil, fmt.Errorf("%s: %w", name, err)
	}
	return node, e, v, nil
}

// opConfig is opBegin without a volume: the node and the shared configuration,
// for the operations on a process rather than a volume (faults.go).
func opConfig(actx *tr.ActionContext, act tr.Action, name string) (runner, opEnv, error) {
	if actx.History == nil {
		return nil, opEnv{}, fmt.Errorf("%s: no history (typed operations run only with a run bundle)", name)
	}
	node, err := opNode(actx, act.Node)
	if err != nil {
		return nil, opEnv{}, fmt.Errorf("%s: %w", name, err)
	}
	e := opEnv{bin: opParam(act, actx, "bin_dir"), master: opParam(act, actx, "master"), filer: opParam(act, actx, "filer"),
		chunkPath: opParam(act, actx, "chunk_path"), root: opParam(act, actx, "root"), client: opParam(act, actx, "client"),
		lockDirs: opParam(act, actx, "lock_dirs_test_only") == "true", expect: optionalParam(act, "expect"),
		ownScript: opParam(act, actx, "own_script"), work: opParam(act, actx, "work"), owner: opParam(act, actx, "owner")}
	if e.expect != "" && e.expect != tr.OutcomeRefused {
		return nil, opEnv{}, fmt.Errorf("%s: expect %q; a step may declare only %q", name, e.expect, tr.OutcomeRefused)
	}
	for _, f := range []string{e.bin, e.master, e.filer, e.chunkPath, e.root, e.client} {
		if strings.Contains(f, "{{") {
			return nil, opEnv{}, fmt.Errorf("%s: unresolved configuration %q", name, f)
		}
	}
	if e.client == "" {
		e.client = "c1"
	}
	if e.bin == "" || e.master == "" || e.filer == "" || e.chunkPath == "" || e.root == "" {
		return nil, opEnv{}, fmt.Errorf("%s: bin_dir, master, filer, chunk_path and root are required (params or vars)", name)
	}
	return node, e, nil
}

// record runs one operation between its invoke and complete records.
func record(actx *tr.ActionContext, e opEnv, verb, vol string, args map[string]any, do func() (string, map[string]any)) (map[string]string, error) {
	if e.expect != "" {
		if args == nil {
			args = map[string]any{}
		}
		args["expect"] = e.expect
	}
	seq, err := actx.History.Invoke(tr.HistoryRecord{Client: e.client, Op: opIDs[verb], Verb: verb, Volume: vol, Args: args})
	if err != nil {
		return nil, err
	}
	outcome, result := do()
	if err := actx.History.Complete(seq, outcome, result); err != nil {
		return nil, err
	}
	actx.Log("  v3_op_%s(%s): %s", verb, vol, outcome)
	return map[string]string{"value": outcome}, nil
}

func fail(detail string) (string, map[string]any) {
	return tr.OutcomeUnknown, map[string]any{"detail": detail}
}

func fromChunk(op opPath, stdout string, code int, result map[string]any) (string, map[string]any) {
	r, d := chunkOutcome(op, stdout, code)
	if result == nil {
		result = map[string]any{}
	}
	if d != "" {
		result["detail"] = d
	}
	return r, result
}

// v3HistoryEnd closes the run's history now, writing its end record, so a
// later phase of the same run (the checkers) judges a history that says it is
// whole. Every record after it is refused; the CLI's own close at the end of
// the run is then a no-op.
func v3HistoryEnd(_ context.Context, actx *tr.ActionContext, _ tr.Action) (map[string]string, error) {
	if actx.History == nil {
		return nil, errors.New("v3_history_end: no history (typed operations run only with a run bundle)")
	}
	open := actx.History.Open()
	if err := actx.History.Close(); err != nil {
		return nil, fmt.Errorf("v3_history_end: %w", err)
	}
	actx.Log("  v3_history_end: %s closed, %d invocations open", actx.History.Path(), open)
	return map[string]string{"value": strconv.Itoa(open)}, nil
}

// v3OpSpec writes the cluster spec for the named volumes: each gets two hosts
// (a-<v> first, b-<v> a move target) and a placement on its first host, with
// no size, so the volume is created later by a timed CreateVolume call (or a
// restore). Params: volumes (space separated), spec_path, base_port (20000),
// nqn_prefix. It records nothing: it is the run's setup.
func v3OpSpec(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
	node, err := actions.GetNode(actx, act.Node)
	if err != nil {
		return nil, fmt.Errorf("v3_op_spec: %w", err)
	}
	names := strings.Fields(requireParam(act, actx, "volumes"))
	path := requireParam(act, actx, "spec_path")
	if len(names) == 0 || path == "" {
		return nil, errors.New("v3_op_spec: volumes and spec_path are required")
	}
	base := actions.ParseInt(optionalParam(act, "base_port"), 20000)
	nqn := optionalParam(act, "nqn_prefix")
	if nqn == "" {
		nqn = "nqn.2026-11.io.seaweedfs:op-"
	}
	vs, err := specVolumes(names, base, nqn)
	if err != nil {
		return nil, fmt.Errorf("v3_op_spec: %w", err)
	}
	var nodes, vols strings.Builder
	for _, v := range vs {
		name := v.Name
		for _, h := range v.Hosts {
			fmt.Fprintf(&nodes, "  - server_id: %s\n    data_addr: %s\n    ctrl_addr: %s\n", h.Server, h.Data, h.Ctrl)
		}
		fmt.Fprintf(&vols, "  - id: %s\n    replication_factor: 1\n    placements:\n      - server_id: %s\n        replica_id: r1\n", name, v.Hosts[0].Server)
		saveVolume(actx, v)
	}
	// The spec's order: blockvolume:<i> of the process faults is volume i here.
	actx.Vars[specOrderVar] = strings.Join(names, " ")
	cmd := fmt.Sprintf("mkdir -p %s && cat > %s <<'V3OP_SPEC_EOF'\nnodes:\n%svolumes:\n%sV3OP_SPEC_EOF\n", shellQuote(dirOf(path)), shellQuote(path), nodes.String(), vols.String())
	if _, stderr, code, err := node.Run(ctx, cmd); err != nil || code != 0 {
		return nil, fmt.Errorf("v3_op_spec: write %s: %s %v", path, firstLine(stderr), err)
	}
	return map[string]string{"value": strconv.Itoa(len(names))}, nil
}

// specVolumes lays out the named volumes: volume i (from 0) gets two hosts on
// ports base+(i+1)*10+0..7 (data, ctrl, status, NVMe; first host, then the
// move target) and the NQN nqn+name. So n volumes use base+10..base+10n+7.
func specVolumes(names []string, base int, nqn string) ([]*opVolume, error) {
	var vs []*opVolume
	for i, name := range names {
		if !regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`).MatchString(name) {
			return nil, fmt.Errorf("volume name %q", name)
		}
		p := func(k int) string { return fmt.Sprintf("127.0.0.1:%d", base+(i+1)*10+k) }
		vs = append(vs, &opVolume{Name: name, NQN: nqn + name, Hosts: []vsHost{
			{Server: "a-" + name, Data: p(0), Ctrl: p(1), Status: p(2), NVMe: p(3)},
			{Server: "b-" + name, Data: p(4), Ctrl: p(5), Status: p(6), NVMe: p(7)},
		}})
	}
	return vs, nil
}

func dirOf(p string) string {
	if i := strings.LastIndex(p, "/"); i > 0 {
		return p[:i]
	}
	return "/"
}

// blockvolume's command line for v on host h, as the product's own
// real-process tests run it, with the layout id from the master.
func (e opEnv) volumeArgs(v *opVolume, h vsHost, blocks uint64) []string {
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

// v3OpAttach starts the holder on the volume's host (param host: 0 first, 1
// the move target; default the current one), waits until it serves above the
// recorded epoch, and connects the kernel NVMe/TCP host (HolderInterface.Start).
// The layout id comes from the master when the runner does not know it.
// Params: blocks (default 4096), ready_seconds (60).
func v3OpAttach(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
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
	return record(actx, e, "attach", v.Name, map[string]any{"server": h.Server, "min_epoch": v.Epoch}, func() (string, map[string]any) {
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
		return e.startHolder(ctx, node, v, h, blocks, bound)
	})
}

func portOf(addr string) string { return addr[strings.LastIndex(addr, ":")+1:] }

// startHolder starts blockvolume for v on host h and credits readiness only
// to that process:
//   - every endpoint of h must be free before the launch (a listener already
//     there, another run's or a leftover, refuses the start: nothing launched);
//   - the child is identified by its pid and start time and recorded as owned;
//   - it is ready only while that child runs, its status serves above the
//     recorded epoch, AND the status and NVMe listeners are held by it;
//   - only then is the kernel host connected, to this run's NQN.
func (e opEnv) startHolder(ctx context.Context, node runner, v *opVolume, h vsHost, blocks uint64, bound time.Duration) (string, map[string]any) {
	return e.launchHolder(ctx, node, v, h, append([]string{e.bin + "/blockvolume"}, e.volumeArgs(v, h, blocks)...), bound, false)
}

// launchHolder is startHolder with the command line given. A restart
// (faults.go: the command line of a killed holder, as it ran) is ready at the
// recorded epoch or above, and connects the kernel host only when no
// controller of the volume's NQN is left: the kernel reconnects the one a
// killed holder left behind by itself.
func (e opEnv) launchHolder(ctx context.Context, node runner, v *opVolume, h vsHost, argv []string, bound time.Duration, restart bool) (string, map[string]any) {
	setIntent(v, intentHeld) // invoked: held, whatever the outcome (faults.go)
	ports := []string{portOf(h.Data), portOf(h.Ctrl), portOf(h.Status), portOf(h.NVMe)}
	held, err := listenerPIDs(ctx, node, ports)
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
	// Through the unit wrapper (faults.go): the holder keeps its own pid, and
	// its exit status lands in a per-launch file.
	stdout, stderr, code, err := node.Run(ctx, holderLaunchCmd(argv, logPath))
	pid := strings.TrimSpace(stdout)
	if err != nil || code != 0 || !regexp.MustCompile(`^[0-9]+$`).MatchString(pid) {
		return fail("start: " + firstLine(stderr))
	}
	start, state := procStart(ctx, node, pid)
	res := map[string]any{"pid": pid, "pid_start": start, "log": logPath, "layout_id": v.LayoutID}
	if state != procRunning {
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
		switch procState(ctx, node, pid, start) {
		case procGone:
			// Not refused, whatever made it exit. The product's start
			// refusals (moved, deleted, superseded, geometry_mismatch) exit
			// 78, but 78 is "a no-restart outcome; the final log line names
			// the reason" (the product's standalone-deployment guide), not
			// "nothing changed": it is also head_fenced, a fence after the
			// start (cmd/blockvolume/process_exit.go:166-176), and the reason
			// is only in that log line (process_exit.go:118-124). Its exit
			// status (the wrapper's file) is the unit's business: the
			// supervision step in faults.go reads it.
			res["detail"] = "blockvolume exited before serving"
			return tr.OutcomeUnknown, res
		case procRunning:
			if st, ok := readStatus(ctx, node, h.Status, v.Name); ok && st.serving(v.Name) && (st.Epoch > v.Epoch || restart && st.Epoch == v.Epoch) {
				owners, err := listenerPIDs(ctx, node, []string{portOf(h.Status), portOf(h.NVMe)})
				if err == nil && contains(owners[portOf(h.Status)], pid) && contains(owners[portOf(h.NVMe)], pid) {
					v.Epoch, res["epoch"] = st.Epoch, st.Epoch
					connect := true
					if restart {
						state, _, why := nvmeScan(ctx, node, v.NQN)
						if state == nvmeUnreadable {
							res["detail"] = why
							return tr.OutcomeUnknown, res
						}
						connect = state == nvmeAbsent
					}
					host, port, _ := strings.Cut(h.NVMe, ":")
					if connect {
						if why := nvmeConnect(ctx, node, host, port, v.NQN); why != "" {
							res["detail"] = why
							return tr.OutcomeUnknown, res
						}
					}
					dev, why := deviceOf(ctx, node, v.NQN, 20*time.Second)
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
		time.Sleep(250 * time.Millisecond)
	}
}

// recordOwned records a started process with the run's ownership script
// (m0-own.sh record), so the teardown may stop it by its identity. Without an
// ownership script configured nothing is recorded.
func (e opEnv) recordOwned(ctx context.Context, node runner, name, pid string) string {
	if e.ownScript == "" {
		return ""
	}
	_, stderr, code, err := node.Run(ctx, fmt.Sprintf("bash %s record %s %s %s %s", shellQuote(e.ownScript), shellQuote(e.work), shellQuote(e.owner), shellQuote(name), pid))
	if err != nil || code != 0 {
		return "recording ownership: " + firstLine(stderr)
	}
	return ""
}

func contains(set []string, v string) bool {
	for _, x := range set {
		if x == v {
			return true
		}
	}
	return false
}

// blockTag is the content of one written block: a repeated tag naming the
// volume, the round and the block, so a block from another volume, round or
// offset cannot match.
func blockTag(vol, round string, lba uint64) string { return fmt.Sprintf("%s|%s|%d|", vol, round, lba) }

// parseLBAs reads "0 2048 4095" or "written" (every lba a write was attempted
// on, in order).
func parseLBAs(v *opVolume, s string) ([]uint64, error) {
	if s == "written" {
		var out []uint64
		for k := range v.Written {
			n, _ := strconv.ParseUint(k, 10, 64)
			out = append(out, n)
		}
		sort.Slice(out, func(a, b int) bool { return out[a] < out[b] })
		return out, nil
	}
	var out []uint64
	for _, f := range strings.Fields(s) {
		n, err := strconv.ParseUint(f, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("lba %q", f)
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil, errors.New("lbas are required")
	}
	return out, nil
}

// writeCmd is the device write of one block: O_DIRECT, and with flush also
// conv=fsync (the kernel sends the device a cache flush before dd exits), so
// exit 0 is acknowledged and flushed. Without flush exit 0 is acknowledged
// only: the block reached the device, which may still hold it volatile.
func writeCmd(blk, dev string, lba uint64, flush bool) string {
	conv := "notrunc"
	if flush {
		conv = "fsync,notrunc"
	}
	return fmt.Sprintf("dd if=%s of=%s bs=4096 seek=%d count=1 oflag=direct conv=%s status=none", shellQuote(blk), dev, lba, conv)
}

// writeArgs is a write's invocation: a write issued without the fsync says
// so ("flush": false); a flushed write's record is today's, without the key.
func writeArgs(lba uint64, sha, round string, flush bool) map[string]any {
	a := map[string]any{"lba": lba, "sha256": sha, "round": round}
	if !flush {
		a["flush"] = false
	}
	return a
}

// writeBlock writes the generated block blk at lba of v's device: ok on dd's
// exit 0, unknown otherwise.
func writeBlock(ctx context.Context, node runner, v *opVolume, blk string, lba uint64, flush bool) (string, map[string]any) {
	dev, why := deviceOf(ctx, node, v.NQN, 5*time.Second)
	if why != "" {
		return fail(why)
	}
	_, stderr, code, err := node.RunRoot(ctx, writeCmd(blk, dev, lba, flush))
	if err != nil || code != 0 {
		return fail(fmt.Sprintf("dd exit %d: %s", code, firstLine(stderr)))
	}
	return tr.OutcomeOK, nil
}

// v3OpWrite writes one block per lba through the kernel block device (O_DIRECT
// + fsync: exit 0 is acknowledged and flushed), each its own operation
// (HolderInterface.Write). The record carries the sha256 of the attempted
// block; the outcome is ok or unknown. Params: lbas, round, flush ("true",
// the default; "false" issues the write without the fsync and records
// "flush": false, so it is durable only after a later acknowledged flush).
func v3OpWrite(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
	node, e, v, err := opBegin(actx, act, "v3_op_write")
	if err != nil {
		return nil, err
	}
	round := requireParam(act, actx, "round")
	if !regexp.MustCompile(`^[A-Za-z0-9_-]{1,16}$`).MatchString(round) {
		return nil, errors.New("v3_op_write: round (1-16 of [A-Za-z0-9_-]) is required")
	}
	flush := true
	switch f := optionalParam(act, "flush"); f {
	case "", "true":
	case "false":
		flush = false
	default:
		return nil, fmt.Errorf("v3_op_write: flush %q; want true or false", f)
	}
	lbas, err := parseLBAs(v, requireParam(act, actx, "lbas"))
	if err != nil {
		return nil, fmt.Errorf("v3_op_write: %w", err)
	}
	if v.Written == nil {
		v.Written = map[string]bool{}
	}
	defer saveVolume(actx, v)
	var last map[string]string
	for _, lba := range lbas {
		tag := blockTag(v.Name, round, lba)
		want := blockSHA(tag)
		// The block is generated and checked BEFORE the invocation: a block
		// whose hash is not the one recorded would put a content on the device
		// that the history names nowhere, and the checker would blame the
		// product. A mismatch is the runner's failure: the run fails, nothing
		// is written.
		blk := fmt.Sprintf("%s/.block-%s-%d", e.root, v.Name, lba)
		gen := fmt.Sprintf(`mkdir -p %s && python3 -c 'import sys;t=sys.argv[1].encode();open(sys.argv[2],"wb").write((t*(4096//len(t)+1))[:4096])' %s %s && sha256sum < %s | cut -c1-64`,
			shellQuote(e.root), shellQuote(tag), shellQuote(blk), shellQuote(blk))
		stdout, stderr, code, err := node.Run(ctx, gen)
		if err != nil || code != 0 || strings.TrimSpace(stdout) != want {
			return nil, fmt.Errorf("v3_op_write: the block generator made %q (exit %d, %s), not %s: runner failure, nothing written", firstLine(stdout), code, firstLine(stderr), want)
		}
		v.Written[strconv.FormatUint(lba, 10)] = true
		last, err = record(actx, e, "write", v.Name, writeArgs(lba, want, round, flush), func() (string, map[string]any) {
			return writeBlock(ctx, node, v, blk, lba, flush)
		})
		if err != nil {
			return nil, err
		}
	}
	return last, nil
}

// flushCmd is fsync(2) of the device: on a block device the kernel sends the
// device a cache flush and returns only once it is acknowledged. python3 is
// on the host already (the block generator); exit 0 only after fsync returns.
func flushCmd(dev string) string {
	return fmt.Sprintf(`python3 -I -c 'import os,sys;fd=os.open(sys.argv[1],os.O_WRONLY);os.fsync(fd);os.close(fd)' %s`, dev)
}

// flushDevice fsyncs v's device: ok on exit 0, unknown otherwise (a failed
// or unanswered fsync may or may not have flushed; the product has no
// refusal for it).
func flushDevice(ctx context.Context, node runner, v *opVolume) (string, map[string]any) {
	dev, why := deviceOf(ctx, node, v.NQN, 5*time.Second)
	if why != "" {
		return fail(why)
	}
	_, stderr, code, err := node.RunRoot(ctx, flushCmd(dev))
	if err != nil || code != 0 {
		return fail(fmt.Sprintf("fsync exit %d: %s", code, firstLine(stderr)))
	}
	return tr.OutcomeOK, map[string]any{"device": dev}
}

// v3OpFlush flushes the volume through the kernel block device (fsync of the
// device), its own operation: an acknowledged flush makes every earlier write
// of the volume durable. No params beyond the volume.
func v3OpFlush(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
	node, e, v, err := opBegin(actx, act, "v3_op_flush")
	if err != nil {
		return nil, err
	}
	return record(actx, e, "flush", v.Name, map[string]any{}, func() (string, map[string]any) {
		return flushDevice(ctx, node, v)
	})
}

// v3OpRead reads one block per lba through the kernel block device and
// records the sha256 it read; it compares nothing. Params: lbas ("written":
// every lba a write was attempted on).
func v3OpRead(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
	node, e, v, err := opBegin(actx, act, "v3_op_read")
	if err != nil {
		return nil, err
	}
	lbas, err := parseLBAs(v, requireParam(act, actx, "lbas"))
	if err != nil {
		return nil, fmt.Errorf("v3_op_read: %w", err)
	}
	if len(lbas) == 0 {
		// Nothing to read records no operation and has no outcome: a runner
		// error, never a silent empty answer.
		return nil, fmt.Errorf("v3_op_read: no lba to read on %s (lbas %q)", v.Name, requireParam(act, actx, "lbas"))
	}
	var last map[string]string
	for _, lba := range lbas {
		last, err = record(actx, e, "read", v.Name, map[string]any{"lba": lba}, func() (string, map[string]any) {
			dev, why := deviceOf(ctx, node, v.NQN, 5*time.Second)
			if why != "" {
				return fail(why)
			}
			stdout, stderr, code, err := node.RunRoot(ctx, fmt.Sprintf("bash -c %s", shellQuote(fmt.Sprintf("set -o pipefail; dd if=%s bs=4096 skip=%d count=1 iflag=direct status=none | sha256sum | cut -c1-64", dev, lba))))
			sum := strings.TrimSpace(stdout)
			if err != nil || code != 0 || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(sum) {
				return fail(fmt.Sprintf("read exit %d: %s", code, firstLine(stderr)))
			}
			return tr.OutcomeOK, map[string]any{"sha256": sum}
		})
		if err != nil {
			return nil, err
		}
	}
	return last, nil
}

// handOver runs the planned stop of the holder: disconnect the guest, detach
// --plain, revoke with the sealed generation, wait for the holder to exit by
// itself, and wait for the master's recorded ACK of (replica, epoch). It
// returns the result fields and "" or the reason the stop is not complete.
func (e opEnv) handOver(ctx context.Context, node runner, v *opVolume, op string) (map[string]any, string) {
	setIntent(v, intentReleased) // invoked: released, whatever the outcome (faults.go)
	res := map[string]any{"epoch": v.Epoch}
	if why := nvmeDisconnect(ctx, node, v.NQN); why != "" {
		return res, why
	}
	stdout, stderr, code, err := node.Run(ctx, fmt.Sprintf("%s ops chunk detach --plain --volume %s --status-addr %s --timeout 120s", shellQuote(e.cli()), shellQuote(v.Name), shellQuote(v.Hosts[v.Cur].Status)))
	if err != nil {
		return res, err.Error()
	}
	if r, d := chunkOutcome(chunkDetach, stdout, code); r != tr.OutcomeOK {
		res["detach"] = r
		return res, "detach: " + d
	}
	gen, perr := parseDetach(stdout, stderr, v.Name, v.Epoch)
	if perr != nil {
		return res, perr.Error()
	}
	res["sealed_gen"] = gen
	out, errOut, code, err := e.workspace(ctx, node, v.Name, "revoke", "--replica", "r1", "--epoch", strconv.FormatUint(v.Epoch, 10), "--sealed-gen", strconv.FormatUint(gen, 10))
	if err != nil || wsOutcome(op, wsRevoke, code) != tr.OutcomeOK {
		res["revoke_exit"] = code
		return res, "revoke: " + firstLine(out+" "+errOut)
	}
	if why := waitGone(ctx, node, v.PID, v.PIDStart, 40*time.Second); why != "" {
		return res, "the holder did not exit by itself after the revoke: " + why
	}
	v.PID, v.PIDStart = "", ""
	ack := regexp.MustCompile(fmt.Sprintf(`(?m)^recorded ACK: r1@%d HEAD generation (\d+)\r?$`, v.Epoch))
	for end := time.Now().Add(30 * time.Second); time.Now().Before(end); time.Sleep(500 * time.Millisecond) {
		out, _, c, e2 := e.workspace(ctx, node, v.Name, "show")
		if m := ack.FindStringSubmatch(out); e2 == nil && c == 0 && m != nil {
			res["ack_gen"] = m[1] // recorded, not compared
			return res, ""
		}
	}
	return res, fmt.Sprintf("no recorded ACK of r1@%d", v.Epoch)
}

func (e opEnv) workspace(ctx context.Context, node runner, vol string, args ...string) (string, string, int, error) {
	return node.Run(ctx, shellQuote(e.cli())+" ops workspace "+strings.Join(quoteAll(append(args, "--master", e.master, "--volume", vol)), " "))
}

// v3OpStop stops the holder with the planned hand-over and no move
// (HolderInterface.StopCommit): the volume is then held by no one, as a
// volume delete requires.
func v3OpStop(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
	node, e, v, err := opBegin(actx, act, "v3_op_stop")
	if err != nil {
		return nil, err
	}
	defer saveVolume(actx, v)
	return record(actx, e, "stop", v.Name, map[string]any{"server": v.Hosts[v.Cur].Server, "epoch": v.Epoch}, func() (string, map[string]any) {
		res, why := e.handOver(ctx, node, v, wsStop)
		if why != "" {
			res["detail"] = why
			return tr.OutcomeUnknown, res
		}
		return tr.OutcomeOK, res
	})
}

var reMovedTo = regexp.MustCompile(`^clean move issued: volume (\S+) request (\S+): r1@(\d+) -> r1@(\d+) on (\S+) \(sealed HEAD gen (\d+), remote LSN (\d+)\)\r?$`)

// v3OpMove is the planned move to the volume's second host
// (MoveAdmission.IssueClean): the planned stop, then `ops workspace move
// --clean`. Its outcome is the master's exit code; the answer's new epoch,
// generation and LSN are recorded. Starting the volume on the target is the
// next operation (v3_op_attach host=1).
func v3OpMove(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
	node, e, v, err := opBegin(actx, act, "v3_op_move")
	if err != nil {
		return nil, err
	}
	to := 1 - v.Cur
	dst := v.Hosts[to]
	req := v.Name + "-move-" + strconv.FormatUint(v.Epoch, 10)
	defer saveVolume(actx, v)
	return record(actx, e, "move", v.Name, map[string]any{"from": v.Hosts[v.Cur].Server, "to": dst.Server, "epoch": v.Epoch, "request": req}, func() (string, map[string]any) {
		res, why := e.handOver(ctx, node, v, wsMove)
		if why != "" {
			res["detail"] = "stop: " + why
			return tr.OutcomeUnknown, res
		}
		out, errOut, code, err := e.workspace(ctx, node, v.Name, "move", "--clean", "--replica", "r1", "--epoch", strconv.FormatUint(v.Epoch, 10),
			"--target-host", dst.Server, "--target-data", dst.Data, "--target-ctrl", dst.Ctrl, "--request-id", req)
		if err != nil {
			return fail(err.Error())
		}
		res["exit"] = code
		// Never refused: the hand-over above already sealed and revoked
		// (product/outcomes.json). The exit is recorded for the reader.
		if r := wsOutcome(wsMove, wsMoveClean, code); r != tr.OutcomeOK {
			res["detail"] = firstLine(out + " " + errOut)
			return r, res
		}
		first, _, _ := strings.Cut(out, "\n")
		m := reMovedTo.FindStringSubmatch(first)
		if m == nil || m[1] != v.Name || m[2] != req || m[3] != strconv.FormatUint(v.Epoch, 10) || m[5] != dst.Server {
			res["detail"] = "the move answer does not name this move: " + firstLine(out)
			return tr.OutcomeUnknown, res
		}
		res["new_epoch"], res["head_gen"], res["remote_lsn"] = m[4], m[6], m[7]
		v.Cur = to
		return tr.OutcomeOK, res
	})
}

// v3OpObserve records what the product shows of the volume: HEAD from `ops
// chunk show` (or its error code), the authority line from `ops workspace
// show`, and every file under the volume's directory in the filer. An
// observation is a fact for the checkers, never a verdict.
func v3OpObserve(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
	node, e, v, err := opBegin(actx, act, "v3_op_observe")
	if err != nil {
		return nil, err
	}
	res := map[string]any{}
	stdout, _, code, err := node.Run(ctx, fmt.Sprintf("%s ops chunk show --volume %s --master %s --chunk-filer %s --chunk-path %s", shellQuote(e.cli()),
		shellQuote(v.Name), shellQuote(e.master), shellQuote(e.filer), shellQuote(e.chunkPath)))
	observeChunkShow(res, stdout, code, err)
	if out, _, c, e2 := e.workspace(ctx, node, v.Name, "show"); e2 == nil {
		res["workspace"] = map[string]any{"exit": c, "text": firstLines(out, 12)}
	}
	// The volume's directories in the filer, whatever its layout: a name that
	// never existed is observed as having none.
	if dirs, why := listDirs(ctx, node, e.filer, e.chunkPath, v.Name+"-"); why != "" {
		res["dirs_error"] = why
	} else {
		res["dirs"] = dirs
	}
	if v.LayoutID != "" {
		files, why := listFiles(ctx, node, e.filer, e.chunkPath, v.Name+"-"+v.LayoutID[:8])
		if why != "" {
			res["listing_error"] = why
		} else {
			res["files"] = files
		}
	}
	return map[string]string{"value": "observed"}, actx.History.Observe(tr.HistoryObserve, tr.HistoryRecord{Client: e.client, Volume: v.Name, Verb: "observe", Result: res})
}

// observeChunkShow records the answer of `ops chunk show`: HEAD's generation,
// epoch, holder and state as "head", and the names of HEAD's snapshots map
// (sorted; empty when HEAD holds none) as "snapshots". The command prints
// the product's decoded HEAD, whose snapshots map is the volume's snapshot
// list. Without a readable HEAD there is no list: "snapshots_error" says why.
func observeChunkShow(res map[string]any, stdout string, code int, err error) {
	if err != nil || code != 0 {
		var eb struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal([]byte(strings.TrimSpace(stdout)), &eb)
		res["show_error"] = map[string]any{"exit": code, "code": eb.Code}
		if err != nil {
			res["snapshots_error"] = fmt.Sprintf("chunk show: %v", err)
		} else {
			res["snapshots_error"] = fmt.Sprintf("chunk show: exit %d code %q", code, eb.Code)
		}
		return
	}
	var body struct {
		Head *struct {
			Gen       uint64
			Epoch     uint64
			Holder    string
			State     string
			Snapshots map[string]json.RawMessage
		} `json:"head"`
	}
	if json.Unmarshal([]byte(strings.TrimSpace(stdout)), &body) != nil || body.Head == nil {
		res["head"] = "unreadable"
		res["snapshots_error"] = "chunk show: HEAD unreadable"
		return
	}
	res["head"] = map[string]any{"gen": body.Head.Gen, "epoch": body.Head.Epoch, "holder": body.Head.Holder, "state": body.Head.State}
	names := make([]string, 0, len(body.Head.Snapshots))
	for n := range body.Head.Snapshots {
		names = append(names, n)
	}
	sort.Strings(names)
	res["snapshots"] = names
}

func firstLines(s string, n int) []string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return lines
}

// listDirs lists the names directly under the chunk path that start with
// prefix (one level, through the filer's HTTP listing). A missing chunk path
// is an empty list.
func listDirs(ctx context.Context, node runner, filer, chunkPath, prefix string) ([]string, string) {
	host, gport, ok := strings.Cut(filer, ":")
	gp, err := strconv.Atoi(gport)
	if !ok || err != nil || gp <= 10000 {
		return nil, "filer has no SeaweedFS gRPC port"
	}
	base := fmt.Sprintf("http://%s:%d%s", host, gp-10000, strings.TrimRight(chunkPath, "/"))
	py := `import json,sys,urllib.request,urllib.parse,urllib.error
d,pre=sys.argv[1],sys.argv[2]; out=[]; last=""
while True:
    q=urllib.parse.urlencode({"limit":1000,"lastFileName":last,"namePattern":pre+"*"})
    try:
        r=json.load(urllib.request.urlopen(urllib.request.Request(d+"/?"+q,headers={"Accept":"application/json"}),timeout=10))
    except urllib.error.HTTPError as e:
        if e.code==404 and not last: break
        raise
    es=r.get("Entries") or []
    out+=[e["FullPath"].rsplit("/",1)[1] for e in es]
    if not r.get("ShouldDisplayLoadMore") or not es: break
    last=es[-1]["FullPath"].rsplit("/",1)[1]
print(json.dumps(sorted(n for n in out if n.startswith(pre))))`
	stdout, stderr, code, err := node.Run(ctx, fmt.Sprintf("python3 -I -c %s %s %s", shellQuote(py), shellQuote(base), shellQuote(prefix)))
	var dirs []string
	if err != nil || code != 0 || json.Unmarshal([]byte(strings.TrimSpace(stdout)), &dirs) != nil {
		return nil, "listing: " + firstLine(stderr)
	}
	return dirs, ""
}

// listFiles lists every file below the volume's directory through the
// filer's HTTP listing (its port is the gRPC port minus 10000, SeaweedFS's
// convention), as paths relative to that directory. A missing directory is
// an empty list.
func listFiles(ctx context.Context, node runner, filer, chunkPath, dir string) ([]string, string) {
	host, gport, ok := strings.Cut(filer, ":")
	gp, err := strconv.Atoi(gport)
	if !ok || err != nil || gp <= 10000 {
		return nil, "filer has no SeaweedFS gRPC port"
	}
	base := fmt.Sprintf("http://%s:%d%s/%s", host, gp-10000, strings.TrimRight(chunkPath, "/"), dir)
	py := `import json,sys,urllib.request,urllib.parse,urllib.error
root=sys.argv[1]; out=[]
def ls(d,rel):
    last=""
    while True:
        q=urllib.parse.urlencode({"limit":1000,"lastFileName":last})
        try:
            r=json.load(urllib.request.urlopen(urllib.request.Request(d+"/?"+q,headers={"Accept":"application/json"}),timeout=10))
        except urllib.error.HTTPError as e:
            if e.code==404 and not last: return
            raise
        es=r.get("Entries") or []
        for e in es:
            name=e["FullPath"].rsplit("/",1)[1]
            if e.get("Mode",0)&0o20000000000: ls(d+"/"+urllib.parse.quote(name),rel+name+"/")
            else: out.append(rel+name)
        if not r.get("ShouldDisplayLoadMore") or not es: return
        last=es[-1]["FullPath"].rsplit("/",1)[1]
ls(root,"")
print(json.dumps(sorted(out)))`
	stdout, stderr, code, err := node.Run(ctx, fmt.Sprintf("python3 -I -c %s %s", shellQuote(py), shellQuote(base)))
	var files []string
	if err != nil || code != 0 || json.Unmarshal([]byte(strings.TrimSpace(stdout)), &files) != nil {
		return nil, "listing: " + firstLine(stderr)
	}
	return files, ""
}
