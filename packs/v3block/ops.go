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
	"read":            "Guest.Read", // a read changes no state; no model has an action for it
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
	node, err := actions.GetNode(actx, act.Node)
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

func fromChunk(stdout string, code int, result map[string]any) (string, map[string]any) {
	r, d := chunkOutcome(stdout, code)
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

// v3OpCreate: one timed CreateVolume call (VolumeCreate.MarkReady). Param:
// size_bytes (default 16 MiB).
func v3OpCreate(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
	node, e, v, err := opBegin(actx, act, "v3_op_create")
	if err != nil {
		return nil, err
	}
	size := actions.ParseInt(optionalParam(act, "size_bytes"), 16<<20)
	defer saveVolume(actx, v)
	return record(actx, e, "create", v.Name, map[string]any{"size_bytes": size}, func() (string, map[string]any) {
		stdout, stderr, code, err := node.Run(ctx, fmt.Sprintf("%s create --master %s --volume %s --size %d", shellQuote(e.v3ctl()), shellQuote(e.master), shellQuote(v.Name), size))
		if err != nil {
			return fail(err.Error())
		}
		a, r := parseV3ctl(stdout, code, v.Name)
		res := map[string]any{"call_ns": a.CallNs}
		if r != tr.OutcomeOK {
			res["detail"] = firstLine(a.Error + " " + stderr)
			return r, res
		}
		v.LayoutID, res["layout_id"] = a.LayoutID, a.LayoutID
		return r, res
	})
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
	stdout, stderr, code, err := node.Run(ctx, fmt.Sprintf("setsid %s >%s 2>&1 </dev/null & echo $!", strings.Join(quoteAll(argv), " "), shellQuote(logPath)))
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
	if why := e.recordOwned(ctx, node, "blockvolume-"+h.Server, pid); why != "" {
		res["detail"] = why
		return tr.OutcomeUnknown, res
	}
	end := time.Now().Add(bound)
	for {
		switch procState(ctx, node, pid, start) {
		case procGone:
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

// v3OpWrite writes one block per lba through the kernel block device (O_DIRECT
// + fsync: exit 0 is acknowledged and flushed), each its own operation
// (HolderInterface.Write). The record carries the sha256 of the attempted
// block; the outcome is ok or unknown. Params: lbas, round.
func v3OpWrite(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
	node, e, v, err := opBegin(actx, act, "v3_op_write")
	if err != nil {
		return nil, err
	}
	round := requireParam(act, actx, "round")
	if !regexp.MustCompile(`^[A-Za-z0-9_-]{1,16}$`).MatchString(round) {
		return nil, errors.New("v3_op_write: round (1-16 of [A-Za-z0-9_-]) is required")
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
		last, err = record(actx, e, "write", v.Name, map[string]any{"lba": lba, "sha256": want, "round": round}, func() (string, map[string]any) {
			dev, why := deviceOf(ctx, node, v.NQN, 5*time.Second)
			if why != "" {
				return fail(why)
			}
			_, stderr, code, err := node.RunRoot(ctx, fmt.Sprintf("dd if=%s of=%s bs=4096 seek=%d count=1 oflag=direct conv=fsync,notrunc status=none", shellQuote(blk), dev, lba))
			if err != nil || code != 0 {
				return fail(fmt.Sprintf("dd exit %d: %s", code, firstLine(stderr)))
			}
			return tr.OutcomeOK, nil
		})
		if err != nil {
			return nil, err
		}
	}
	return last, nil
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

// v3OpSnapshot takes a named snapshot through the holder (SnapshotRemoval.Commit).
// Param: name.
func v3OpSnapshot(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
	node, e, v, err := opBegin(actx, act, "v3_op_snapshot")
	if err != nil {
		return nil, err
	}
	name := requireParam(act, actx, "name")
	if name == "" {
		return nil, errors.New("v3_op_snapshot: name is required")
	}
	return record(actx, e, "snapshot", v.Name, map[string]any{"name": name}, func() (string, map[string]any) {
		stdout, _, code, err := node.Run(ctx, fmt.Sprintf("%s ops chunk snapshot --volume %s --status-addr %s --name %s --request-id %s", shellQuote(e.cli()),
			shellQuote(v.Name), shellQuote(v.Hosts[v.Cur].Status), shellQuote(name), shellQuote(v.Name+"-"+name)))
		if err != nil {
			return fail(err.Error())
		}
		return fromChunk(stdout, code, nil)
	})
}

// v3OpSnapshotDelete deletes a named snapshot (SnapshotRemoval.DeleteSnap):
// through the holder while it serves (the product cleans right after such a
// delete), store direct with `sw-block ops chunk snapshot-delete` when no
// holder runs (cleaned at the next periodic pass, the product's guide says).
// Param: name.
func v3OpSnapshotDelete(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
	node, e, v, err := opBegin(actx, act, "v3_op_snapshot_delete")
	if err != nil {
		return nil, err
	}
	name := requireParam(act, actx, "name")
	if name == "" {
		return nil, errors.New("v3_op_snapshot_delete: name is required")
	}
	via := "store"
	if alive(ctx, node, v.PID, v.PIDStart) {
		via = "holder"
	}
	return record(actx, e, "snapshot_delete", v.Name, map[string]any{"name": name, "via": via}, func() (string, map[string]any) {
		if via == "holder" {
			body, _ := json.Marshal(map[string]string{"volumeID": v.Name, "name": name})
			out, _, code, err := node.Run(ctx, fmt.Sprintf("curl -s -m 60 -X POST -H 'Content-Type: application/json' --data %s -w '\\n%%{http_code}' %s",
				shellQuote(string(body)), shellQuote("http://"+v.Hosts[v.Cur].Status+"/chunk/snapshot/delete")))
			if err != nil || code != 0 {
				return fail(fmt.Sprintf("holder not answering (curl exit %d)", code))
			}
			i := strings.LastIndex(strings.TrimRight(out, "\n"), "\n")
			status := 1
			if strings.TrimSpace(out[i+1:]) == "200" {
				status = 0
			}
			return fromChunk(out[:max(i, 0)], status, nil)
		}
		stdout, _, code, err := node.Run(ctx, fmt.Sprintf("%s ops chunk snapshot-delete --volume %s --name %s --master %s --chunk-filer %s --chunk-path %s", shellQuote(e.cli()),
			shellQuote(v.Name), shellQuote(name), shellQuote(e.master), shellQuote(e.filer), shellQuote(e.chunkPath)))
		if err != nil {
			return fail(err.Error())
		}
		return fromChunk(stdout, code, nil)
	})
}

// v3OpRestore creates the target volume from a snapshot of this volume
// through the master (RestoreTarget.CreateHead). Params: snapshot, target_volume ("target" is the parser's iSCSI field).
func v3OpRestore(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
	node, e, v, err := opBegin(actx, act, "v3_op_restore")
	if err != nil {
		return nil, err
	}
	snap, target := requireParam(act, actx, "snapshot"), requireParam(act, actx, "target_volume")
	if snap == "" || target == "" {
		return nil, errors.New("v3_op_restore: snapshot and target_volume are required")
	}
	t, err := loadVolume(actx, target)
	if err != nil {
		return nil, fmt.Errorf("v3_op_restore: %w", err)
	}
	defer saveVolume(actx, t)
	return record(actx, e, "restore", target, map[string]any{"source": v.Name, "snapshot": snap}, func() (string, map[string]any) {
		stdout, _, code, err := node.Run(ctx, fmt.Sprintf("%s ops chunk restore --source %s --snapshot %s --target %s --master %s --request-id %s", shellQuote(e.cli()),
			shellQuote(v.Name), shellQuote(snap), shellQuote(target), shellQuote(e.master), shellQuote("restore-"+target)))
		if err != nil {
			return fail(err.Error())
		}
		return fromChunk(stdout, code, nil)
	})
}

// v3OpRelease removes a deleted restore target from this volume's snapshot
// dependents (RestoreTarget.OpRelease). Param: target_volume.
func v3OpRelease(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
	node, e, v, err := opBegin(actx, act, "v3_op_release")
	if err != nil {
		return nil, err
	}
	target := requireParam(act, actx, "target_volume")
	if target == "" {
		return nil, errors.New("v3_op_release: target_volume is required")
	}
	return record(actx, e, "release", v.Name, map[string]any{"target": target}, func() (string, map[string]any) {
		stdout, _, code, err := node.Run(ctx, fmt.Sprintf("%s ops chunk release --source %s --target %s --master %s --chunk-filer %s --chunk-path %s", shellQuote(e.cli()),
			shellQuote(v.Name), shellQuote(target), shellQuote(e.master), shellQuote(e.filer), shellQuote(e.chunkPath)))
		if err != nil {
			return fail(err.Error())
		}
		return fromChunk(stdout, code, nil)
	})
}

// handOver runs the planned stop of the holder: disconnect the guest, detach
// --plain, revoke with the sealed generation, wait for the holder to exit by
// itself, and wait for the master's recorded ACK of (replica, epoch). It
// returns the result fields and "" or the reason the stop is not complete.
func (e opEnv) handOver(ctx context.Context, node runner, v *opVolume) (map[string]any, string) {
	res := map[string]any{"epoch": v.Epoch}
	if why := nvmeDisconnect(ctx, node, v.NQN); why != "" {
		return res, why
	}
	stdout, stderr, code, err := node.Run(ctx, fmt.Sprintf("%s ops chunk detach --plain --volume %s --status-addr %s --timeout 120s", shellQuote(e.cli()), shellQuote(v.Name), shellQuote(v.Hosts[v.Cur].Status)))
	if err != nil {
		return res, err.Error()
	}
	if r, d := chunkOutcome(stdout, code); r != tr.OutcomeOK {
		res["detach"] = r
		return res, "detach: " + d
	}
	gen, perr := parseDetach(stdout, stderr, v.Name, v.Epoch)
	if perr != nil {
		return res, perr.Error()
	}
	res["sealed_gen"] = gen
	out, errOut, code, err := e.workspace(ctx, node, v.Name, "revoke", "--replica", "r1", "--epoch", strconv.FormatUint(v.Epoch, 10), "--sealed-gen", strconv.FormatUint(gen, 10))
	if err != nil || wsOutcome(code) != tr.OutcomeOK {
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
		res, why := e.handOver(ctx, node, v)
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
		res, why := e.handOver(ctx, node, v)
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
		if r := wsOutcome(code); r != tr.OutcomeOK {
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

// v3OpDelete deletes the volume through the master's delete authority
// (MoveAdmission.DelMark, `ops chunk volume-delete`). After a stop the master
// admits the delete only once the holder's lease horizon plus the clock-skew
// margin has passed, and answers not_admitted until then; the CLI's own
// procedure is to rerun with the SAME request id once the cause is cleared.
// So one operation reruns it while the answer is not_admitted, within
// admit_seconds (default 120), and records every attempt. Its outcome is the
// last answer's.
func v3OpDelete(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
	node, e, v, err := opBegin(actx, act, "v3_op_delete")
	if err != nil {
		return nil, err
	}
	bound := time.Duration(actions.ParseInt(optionalParam(act, "admit_seconds"), 120)) * time.Second
	req := "delete-" + v.Name
	return record(actx, e, "delete", v.Name, map[string]any{"layout_id": v.LayoutID, "request": req}, func() (string, map[string]any) {
		end := time.Now().Add(bound)
		var attempts []string
		for {
			stdout, _, code, err := node.Run(ctx, fmt.Sprintf("%s ops chunk volume-delete --volume %s --master %s --chunk-filer %s --chunk-path %s --request-id %s", shellQuote(e.cli()),
				shellQuote(v.Name), shellQuote(e.master), shellQuote(e.filer), shellQuote(e.chunkPath), shellQuote(req)))
			if err != nil {
				return tr.OutcomeUnknown, map[string]any{"detail": err.Error(), "attempts": attempts}
			}
			r, res := fromChunk(stdout, code, nil)
			d, _ := res["detail"].(string)
			attempts = append(attempts, fmt.Sprintf("%s %s", r, firstLine(d)))
			res["attempts"] = attempts
			if r == tr.OutcomeOK || !strings.HasPrefix(d, "not_admitted:") || time.Now().After(end) {
				return r, res
			}
			select {
			case <-ctx.Done():
				return r, res
			case <-time.After(3 * time.Second):
			}
		}
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
	if err == nil && code == 0 {
		var body struct {
			Head *struct {
				Gen    uint64
				Epoch  uint64
				Holder string
				State  string
			} `json:"head"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(stdout)), &body) == nil && body.Head != nil {
			res["head"] = map[string]any{"gen": body.Head.Gen, "epoch": body.Head.Epoch, "holder": body.Head.Holder, "state": body.Head.State}
		} else {
			res["head"] = "unreadable"
		}
	} else {
		var eb struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal([]byte(strings.TrimSpace(stdout)), &eb)
		res["show_error"] = map[string]any{"exit": code, "code": eb.Code}
	}
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
