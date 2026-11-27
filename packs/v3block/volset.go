package v3block

// Volume-set actions: typed operations over N volumes on one host, each
// through the operator surface (the sw-block CLI; where it has no command,
// the blockmaster call it makes, through v3ctl). One action covers the whole
// set with bounded parallelism, so a 200-volume scenario is a few steps.
//
// An action decides from exit codes and JSON answers, never from a log line.
// It fails when any volume's operation is not ok, naming the volumes and their
// outcome (refused or unknown). Latencies are host-monotonic and reported as
// <save_as>_p50_ms, _p99_ms, _max_ms, _n.
//
// The set lives in the variable "v3vs" (JSON) between actions.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/actions"
)

const volSetVar = "v3vs"

// vsHost is one simulated host of a volume: its server id and endpoints.
type setVsHost struct {
	Server string `json:"server"`
	Data   string `json:"data"`
	Ctrl   string `json:"ctrl"`
	Status string `json:"status"`
	NVMe   string `json:"nvme"`
}

type vsVolume struct {
	Name     string              `json:"name"`
	NQN      string              `json:"nqn"`
	LayoutID string              `json:"layout_id,omitempty"`
	Hosts    [2]setVsHost        `json:"hosts"` // [0] the first placement, [1] the move target
	Cur      int                 `json:"cur"`   // index of the host serving now
	PID      string              `json:"pid,omitempty"`
	Epoch    uint64              `json:"epoch,omitempty"`
	Expect   map[string][]string `json:"expect,omitempty"`  // lba -> the sha256 values the block may hold
	Deleted  map[string]bool     `json:"deleted,omitempty"` // snapshots deleted by an earlier step
	Moved    bool                `json:"moved,omitempty"`
}

// vsSet is the set and the configuration every action shares.
type vsSet struct {
	Bin       string     `json:"bin"`
	Master    string     `json:"master"`
	Filer     string     `json:"filer"`
	ChunkPath string     `json:"chunk_path"`
	Root      string     `json:"root"`
	SizeBytes uint64     `json:"size_bytes"`
	Blocks    uint64     `json:"blocks"`
	Parallel  int        `json:"parallel"`
	LockDirs  bool       `json:"lock_dirs_test_only"`
	Volumes   []vsVolume `json:"volumes"`
}

func loadSet(actx *tr.ActionContext) (*vsSet, error) {
	raw := actx.Vars[volSetVar]
	if raw == "" {
		return nil, errors.New("no volume set: run v3_vs_spec first")
	}
	var s vsSet
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return nil, fmt.Errorf("volume set: %w", err)
	}
	return &s, nil
}

func (s *vsSet) save(actx *tr.ActionContext) {
	b, _ := json.Marshal(s)
	actx.Vars[volSetVar] = string(b)
}

func (s *vsSet) cli() string        { return s.Bin + "/sw-block" }
func (s *vsSet) v3ctl() string      { return s.Bin + "/v3ctl" }
func (v *vsVolume) host() setVsHost { return v.Hosts[v.Cur] }

// vsSpecHosts lays out the endpoints of volume i (1-based): ten ports each
// from base, the first four for host 0 and the next four for host 1.
func vsSpecHosts(base, i int, name string) [2]setVsHost {
	p := func(k int) string { return fmt.Sprintf("127.0.0.1:%d", base+i*10+k) }
	return [2]setVsHost{
		{Server: "a-" + name, Data: p(0), Ctrl: p(1), Status: p(2), NVMe: p(3)},
		{Server: "b-" + name, Data: p(4), Ctrl: p(5), Status: p(6), NVMe: p(7)},
	}
}

// vsClusterSpec is the cluster spec: every host, and each volume placed on
// its first host WITHOUT size_bytes, so the master holds the placement and the
// volume is created later by a timed CreateVolume call.
func vsClusterSpec(s *vsSet) string {
	var nodes, vols strings.Builder
	for _, v := range s.Volumes {
		for _, h := range v.Hosts {
			fmt.Fprintf(&nodes, "  - server_id: %s\n    data_addr: %s\n    ctrl_addr: %s\n", h.Server, h.Data, h.Ctrl)
		}
		fmt.Fprintf(&vols, "  - id: %s\n    replication_factor: 1\n    placements:\n      - server_id: %s\n        replica_id: r1\n", v.Name, v.Hosts[0].Server)
	}
	return "nodes:\n" + nodes.String() + "volumes:\n" + vols.String()
}

// v3VsSpec builds the set and writes its cluster spec.
//
// Required: bin_dir (blockmaster, blockvolume, sw-block, v3ctl), master,
// filer (gRPC), chunk_path, root, count, spec_path.
// Optional: prefix (default "v"), base_port (default 20000), size_bytes
// (default 16 MiB), parallel (default 16), nqn_prefix, lock_dirs_test_only
// ("true": pass blockvolume's test-only lock directory flag, for a host
// without the product's systemd lock directory).
func v3VsSpec(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
	node, err := actions.GetNode(actx, act.Node)
	if err != nil {
		return nil, fmt.Errorf("v3_vs_spec: %w", err)
	}
	s := &vsSet{Bin: requireParam(act, actx, "bin_dir"), Master: requireParam(act, actx, "master"),
		Filer: requireParam(act, actx, "filer"), ChunkPath: requireParam(act, actx, "chunk_path"), Root: requireParam(act, actx, "root")}
	count := actions.ParseInt(requireParam(act, actx, "count"), 0)
	specPath := requireParam(act, actx, "spec_path")
	if s.Bin == "" || s.Master == "" || s.Filer == "" || s.ChunkPath == "" || s.Root == "" || specPath == "" || count < 1 || count > 999 {
		return nil, errors.New("v3_vs_spec: bin_dir, master, filer, chunk_path, root, spec_path and count (1..999) are required")
	}
	prefix := optionalParam(act, "prefix")
	if prefix == "" {
		prefix = "v"
	}
	if !regexp.MustCompile(`^[a-z][a-z0-9-]{0,20}$`).MatchString(prefix) {
		return nil, fmt.Errorf("v3_vs_spec: prefix %q", prefix)
	}
	base := actions.ParseInt(optionalParam(act, "base_port"), 20000)
	if base < 1024 || base+count*10+10 > 65535 {
		return nil, fmt.Errorf("v3_vs_spec: base_port %d leaves no room for %d volumes", base, count)
	}
	s.SizeBytes = uint64(actions.ParseInt(optionalParam(act, "size_bytes"), 16<<20))
	if s.SizeBytes%4096 != 0 || s.SizeBytes == 0 {
		return nil, errors.New("v3_vs_spec: size_bytes must be a positive multiple of 4096")
	}
	s.Blocks = s.SizeBytes / 4096
	s.Parallel = actions.ParseInt(optionalParam(act, "parallel"), 16)
	s.LockDirs = optionalParam(act, "lock_dirs_test_only") == "true"
	nqn := optionalParam(act, "nqn_prefix")
	if nqn == "" {
		sum := sha256.Sum256([]byte(s.Root))
		nqn = "nqn.2026-11.io.seaweedfs:vs-" + hex.EncodeToString(sum[:8]) + "-"
	}
	for i := 1; i <= count; i++ {
		name := fmt.Sprintf("%s%03d", prefix, i)
		s.Volumes = append(s.Volumes, vsVolume{Name: name, NQN: nqn + name, Hosts: vsSpecHosts(base, i, name)})
	}
	cmd := fmt.Sprintf("mkdir -p %s %s && cat > %s <<'V3VS_SPEC_EOF'\n%sV3VS_SPEC_EOF\n",
		shellQuote(s.Root), shellQuote(setDirOf(specPath)), shellQuote(specPath), vsClusterSpec(s))
	if _, stderr, code, err := node.Run(ctx, cmd); err != nil || code != 0 {
		return nil, fmt.Errorf("v3_vs_spec: write %s: code=%d %s %v", specPath, code, stderr, err)
	}
	s.save(actx)
	actx.Log("  v3_vs_spec: %d volumes, spec %s", count, specPath)
	return map[string]string{"value": strconv.Itoa(count)}, nil
}

func setDirOf(p string) string {
	if i := strings.LastIndex(p, "/"); i > 0 {
		return p[:i]
	}
	return "/"
}

// outcome is one volume's result of one operation.
type outcome struct {
	Volume string
	Result string // ok | refused | unknown
	Detail string
	Dur    time.Duration
}

// forEach runs op on every selected volume with s.Parallel workers and
// collects the outcomes in volume order.
func (s *vsSet) forEach(ctx context.Context, sel []int, op func(ctx context.Context, v *vsVolume) outcome) []outcome {
	out := make([]outcome, len(sel))
	sem := make(chan struct{}, max(1, s.Parallel))
	var wg sync.WaitGroup
	for k, i := range sel {
		wg.Add(1)
		go func(k, i int) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				out[k] = outcome{Volume: s.Volumes[i].Name, Result: "unknown", Detail: ctx.Err().Error()}
				return
			}
			defer func() { <-sem }()
			start := time.Now()
			o := op(ctx, &s.Volumes[i])
			o.Volume = s.Volumes[i].Name
			if o.Dur == 0 {
				o.Dur = time.Since(start)
			}
			out[k] = o
		}(k, i)
	}
	wg.Wait()
	return out
}

// selectVolumes is every volume, or the first n when the param "first" is set.
func (s *vsSet) selectVolumes(act tr.Action) []int {
	n := len(s.Volumes)
	if f := actions.ParseInt(optionalParam(act, "first"), 0); f > 0 && f < n {
		n = f
	}
	sel := make([]int, n)
	for i := range sel {
		sel[i] = i
	}
	return sel
}

// report turns outcomes into the action's result: an error naming every
// volume that was not ok, and latency figures under save_as.
func report(actx *tr.ActionContext, act tr.Action, name string, outs []outcome) (map[string]string, error) {
	var bad []string
	durs := make([]time.Duration, 0, len(outs))
	for _, o := range outs {
		if o.Result != "ok" {
			bad = append(bad, fmt.Sprintf("%s %s: %s", o.Volume, o.Result, o.Detail))
			continue
		}
		durs = append(durs, o.Dur)
	}
	res := latency(durs)
	res["value"] = strconv.Itoa(len(outs) - len(bad))
	res["failed"] = strconv.Itoa(len(bad))
	if act.SaveAs != "" {
		for k, v := range res {
			if k != "value" {
				actx.Vars[act.SaveAs+"_"+k] = v
			}
		}
	}
	actx.Log("  %s: %d ok, %d not ok; p50=%sms p99=%sms max=%sms", name, len(outs)-len(bad), len(bad), res["p50_ms"], res["p99_ms"], res["max_ms"])
	if len(bad) > 0 {
		sort.Strings(bad)
		if len(bad) > 20 {
			bad = append(bad[:20], fmt.Sprintf("... and %d more", len(bad)-20))
		}
		return res, fmt.Errorf("%s: %d of %d volumes not ok: %s", name, len(bad), len(outs), strings.Join(bad, "; "))
	}
	return res, nil
}

// latency is p50, p99 (nearest rank) and max in milliseconds.
func latency(d []time.Duration) map[string]string {
	out := map[string]string{"n": strconv.Itoa(len(d))}
	if len(d) == 0 {
		return out
	}
	s := append([]time.Duration(nil), d...)
	sort.Slice(s, func(a, b int) bool { return s[a] < s[b] })
	rank := func(p float64) time.Duration { return s[int(math.Ceil(p*float64(len(s))))-1] }
	ms := func(x time.Duration) string { return strconv.FormatFloat(float64(x.Microseconds())/1000, 'f', 1, 64) }
	out["p50_ms"], out["p99_ms"], out["max_ms"] = ms(rank(0.50)), ms(rank(0.99)), ms(s[len(s)-1])
	return out
}

// run runs one command and returns stdout, stderr, the exit code and
// whether the command could not be run at all.
type setRunner interface {
	Run(ctx context.Context, cmd string) (string, string, int, error)
	RunRoot(ctx context.Context, cmd string) (string, string, int, error)
}

// v3ctlAnswer is v3ctl's JSON answer.
type setV3ctlAnswer struct {
	Outcome    string `json:"outcome"`
	Volume     string `json:"volume"`
	LayoutID   string `json:"layout_id"`
	StoreReady bool   `json:"store_ready"`
	CallNs     int64  `json:"call_ns"`
	Error      string `json:"error"`
}

// parseV3ctl maps v3ctl's exit code and answer to an outcome. The exit code
// and the answer must agree; anything else is unknown.
func setParseV3ctl(stdout string, code int, volume string) (setV3ctlAnswer, string) {
	var a setV3ctlAnswer
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &a); err != nil || a.Volume != volume {
		return a, "unknown"
	}
	want := map[string]int{"ok": 0, "refused": 1, "unknown": 3}[a.Outcome]
	if a.Outcome == "" || want != code || (a.Outcome == "ok" && len(a.LayoutID) != 32) {
		return a, "unknown"
	}
	return a, a.Outcome
}

// v3VsCreate creates every volume with one timed CreateVolume call each.
// The latency is the call alone, as v3ctl measures it.
func v3VsCreate(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
	node, s, err := vsBegin(actx, act, "v3_vs_create")
	if err != nil {
		return nil, err
	}
	outs := s.forEach(ctx, s.selectVolumes(act), func(ctx context.Context, v *vsVolume) outcome {
		cmd := fmt.Sprintf("%s create --master %s --volume %s --size %d", shellQuote(s.v3ctl()), shellQuote(s.Master), shellQuote(v.Name), s.SizeBytes)
		stdout, stderr, code, err := node.Run(ctx, cmd)
		if err != nil {
			return outcome{Result: "unknown", Detail: err.Error()}
		}
		a, r := setParseV3ctl(stdout, code, v.Name)
		if r != "ok" {
			return outcome{Result: r, Detail: setFirstLine(a.Error + " " + stderr)}
		}
		v.LayoutID = a.LayoutID
		return outcome{Result: "ok", Dur: time.Duration(a.CallNs)}
	})
	s.save(actx)
	return report(actx, act, "v3_vs_create", outs)
}

func vsBegin(actx *tr.ActionContext, act tr.Action, name string) (setRunner, *vsSet, error) {
	node, err := actions.GetNode(actx, act.Node)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", name, err)
	}
	s, err := loadSet(actx)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", name, err)
	}
	return node, s, nil
}

func setFirstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// volumeArgs is blockvolume's command line for v on host h, as the product's
// own real-process tests run it, with the layout id from the master.
func (s *vsSet) volumeArgs(v *vsVolume, h setVsHost) []string {
	args := []string{"--master", s.Master, "--server-id", h.Server, "--volume-id", v.Name, "--replica-id", "r1",
		"--layout-id", v.LayoutID, "--data-addr", h.Data, "--ctrl-addr", h.Ctrl, "--status-addr", h.Status,
		"--durable-root", s.Root + "/" + h.Server, "--durable-impl", "walstore-chunk",
		"--durable-blocks", strconv.FormatUint(s.Blocks, 10), "--durable-blocksize", "4096",
		"--chunk-filer", s.Filer, "--chunk-path", s.ChunkPath, "--chunk-control-endpoint", "--workspace-lease",
		"--nvme-listen", h.NVMe, "--nvme-subsysnqn", v.NQN, "--chunk-capture-interval", "0", "--chunk-snapshot-schedule", "off"}
	if s.LockDirs {
		args = append([]string{"--volume-lock-dir-test-only", s.Root + "/" + h.Server + ".locks"}, args...)
	}
	return args
}

// vsStatus is the part of blockvolume's /status the actions read.
type setVsStatus struct {
	VolumeID             string
	Epoch                uint64
	Healthy              bool
	AuthorityRole        string
	FrontendPrimaryReady bool
}

func (st setVsStatus) serving(vol string) bool {
	return st.VolumeID == vol && st.Healthy && st.AuthorityRole == "primary" && st.FrontendPrimaryReady && st.Epoch > 0
}

func setReadStatus(ctx context.Context, node setRunner, h setVsHost, vol string) (setVsStatus, bool) {
	stdout, _, code, err := node.Run(ctx, fmt.Sprintf("curl -s -m 2 %s", shellQuote("http://"+h.Status+"/status?volume="+vol)))
	var st setVsStatus
	if err != nil || code != 0 || json.Unmarshal([]byte(stdout), &st) != nil {
		return st, false
	}
	return st, true
}

// waitServing polls the volume's status until it serves above minEpoch, the
// process exits, or the bound passes.
func waitServing(ctx context.Context, node setRunner, v *vsVolume, minEpoch uint64, bound time.Duration) (uint64, string) {
	end := time.Now().Add(bound)
	for time.Now().Before(end) {
		if st, ok := setReadStatus(ctx, node, v.host(), v.Name); ok && st.serving(v.Name) && st.Epoch > minEpoch {
			return st.Epoch, ""
		}
		if v.PID != "" {
			if out, _, _, _ := node.Run(ctx, "kill -0 "+v.PID+" 2>/dev/null && echo alive"); strings.TrimSpace(out) != "alive" {
				return 0, "refused: blockvolume exited before serving"
			}
		}
		select {
		case <-ctx.Done():
			return 0, "unknown: " + ctx.Err().Error()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return 0, fmt.Sprintf("unknown: not serving above epoch %d within %s", minEpoch, bound)
}

// attach starts blockvolume for v on its current host, waits until it
// serves above minEpoch, and connects the kernel NVMe/TCP host to it.
func (s *vsSet) attach(ctx context.Context, node setRunner, v *vsVolume, minEpoch uint64, bound time.Duration) outcome {
	h := v.host()
	if v.LayoutID == "" {
		return outcome{Result: "refused", Detail: "no layout id (create first)"}
	}
	logPath := fmt.Sprintf("%s/%s.%d.log", s.Root, h.Server, time.Now().UnixNano())
	mk := fmt.Sprintf("mkdir -p %s", shellQuote(s.Root+"/"+h.Server))
	if s.LockDirs {
		mk += " " + shellQuote(s.Root+"/"+h.Server+".locks")
	}
	if _, _, code, err := node.Run(ctx, mk); err != nil || code != 0 {
		return outcome{Result: "unknown", Detail: "mkdir"}
	}
	// Start in the background and print the child's own pid: no ps grep.
	cmd := fmt.Sprintf("setsid %s %s >%s 2>&1 </dev/null & echo $!", shellQuote(s.Bin+"/blockvolume"), strings.Join(quoteAll(s.volumeArgs(v, h)), " "), shellQuote(logPath))
	stdout, stderr, code, err := node.Run(ctx, cmd)
	pid := strings.TrimSpace(stdout)
	if err != nil || code != 0 || !regexp.MustCompile(`^[0-9]+$`).MatchString(pid) {
		return outcome{Result: "unknown", Detail: "start: " + setFirstLine(stderr)}
	}
	v.PID = pid
	epoch, why := waitServing(ctx, node, v, minEpoch, bound)
	if why != "" {
		r, d, _ := strings.Cut(why, ": ")
		return outcome{Result: r, Detail: d + " (log " + logPath + ")"}
	}
	v.Epoch = epoch
	host, port, _ := strings.Cut(h.NVMe, ":")
	if why := setNvmeConnect(ctx, node, host, port, v.NQN); why != "" {
		return outcome{Result: "unknown", Detail: why}
	}
	if _, why := setDeviceOf(ctx, node, v.NQN, 20*time.Second); why != "" {
		return outcome{Result: "unknown", Detail: why}
	}
	return outcome{Result: "ok"}
}

// nvmeConnect connects the kernel NVMe/TCP host to nqn at host:port through
// the kernel's fabrics interface, the call nvme-cli makes (no tool needed).
func setNvmeConnect(ctx context.Context, node setRunner, host, port, nqn string) string {
	if !regexp.MustCompile(`^[0-9.]+$`).MatchString(host) || !regexp.MustCompile(`^[0-9]+$`).MatchString(port) || !regexp.MustCompile(`^[A-Za-z0-9.:_-]+$`).MatchString(nqn) {
		return "nvme connect: malformed address or nqn"
	}
	opts := fmt.Sprintf("transport=tcp,traddr=%s,trsvcid=%s,nqn=%s", host, port, nqn)
	if _, stderr, code, err := node.RunRoot(ctx, fmt.Sprintf("printf %%s %s > /dev/nvme-fabrics", shellQuote(opts))); err != nil || code != 0 {
		return "nvme connect: " + setFirstLine(stderr)
	}
	return ""
}

// nvmeDisconnect deletes every kernel controller of subsystem nqn through
// sysfs (what `nvme disconnect -n` does) and confirms none remains.
func setNvmeDisconnect(ctx context.Context, node setRunner, nqn string) string {
	cmd := fmt.Sprintf(`for d in /sys/class/nvme/nvme*; do [ "$(cat "$d/subsysnqn" 2>/dev/null)" = %s ] && echo 1 > "$d/delete_controller"; done; true`, shellQuote(nqn))
	if _, stderr, code, err := node.RunRoot(ctx, cmd); err != nil || code != 0 {
		return "nvme disconnect: " + setFirstLine(stderr)
	}
	if _, why := setDeviceOf(ctx, node, nqn, 500*time.Millisecond); why == "" {
		return "nvme disconnect: a controller of " + nqn + " remains"
	}
	return ""
}

// deviceOf finds the block device of the connected subsystem nqn in sysfs.
func setDeviceOf(ctx context.Context, node setRunner, nqn string, bound time.Duration) (string, string) {
	cmd := fmt.Sprintf(`for d in /sys/class/nvme/nvme*; do [ "$(cat "$d/subsysnqn" 2>/dev/null)" = %s ] || continue; for n in "$d"/nvme*n*; do [ -e "$n" ] && basename "$n"; done; done | sed -E 's/c[0-9]+n/n/' | sort -u`, shellQuote(nqn))
	end := time.Now().Add(bound)
	for time.Now().Before(end) {
		stdout, _, _, _ := node.Run(ctx, cmd)
		if devs := strings.Fields(stdout); len(devs) == 1 {
			return "/dev/" + devs[0], ""
		} else if len(devs) > 1 {
			return "", fmt.Sprintf("subsystem %s has %d namespaces", nqn, len(devs))
		}
		time.Sleep(250 * time.Millisecond)
	}
	return "", "no block device for " + nqn
}

// v3VsAttach resolves each volume's layout id at the master when it is not
// known yet, then starts and connects it. Optional: ready_seconds (60).
func v3VsAttach(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
	node, s, err := vsBegin(actx, act, "v3_vs_attach")
	if err != nil {
		return nil, err
	}
	bound := time.Duration(actions.ParseInt(optionalParam(act, "ready_seconds"), 60)) * time.Second
	outs := s.forEach(ctx, s.selectVolumes(act), func(ctx context.Context, v *vsVolume) outcome {
		if v.LayoutID == "" {
			stdout, _, code, err := node.Run(ctx, fmt.Sprintf("%s resolve --master %s --volume %s", shellQuote(s.v3ctl()), shellQuote(s.Master), shellQuote(v.Name)))
			if err != nil {
				return outcome{Result: "unknown", Detail: err.Error()}
			}
			a, r := setParseV3ctl(stdout, code, v.Name)
			if r != "ok" {
				return outcome{Result: r, Detail: "resolve: " + a.Error}
			}
			v.LayoutID = a.LayoutID
		}
		return s.attach(ctx, node, v, 0, bound)
	})
	s.save(actx)
	return report(actx, act, "v3_vs_attach", outs)
}

// blockSHA is the content of the 4096-byte block a write puts at lba: a
// repeated tag naming the volume, the round and the block, so a block read
// from the wrong volume, round or offset cannot match.
func setBlockTag(vol, round string, lba uint64) string {
	return fmt.Sprintf("%s|%s|%d|", vol, round, lba)
}

// lbas are the blocks a round writes: spread over the volume so they land in
// different chunks (first, middle, last block).
func lbas(blocks uint64) []uint64 {
	if blocks < 3 {
		return []uint64{0}
	}
	return []uint64{0, blocks / 2, blocks - 1}
}

// zeroBlockSHA is the sha256 of a 4096-byte zero block: a new volume's content.
const zeroBlockSHA = "ad7facb2586fc6e966c004d7d1d16b024f5805ff7cb47c7a85dabd8b48892ca7"

// blockSHA is the sha256 of the block a write of tag puts down, computed here
// so a write whose outcome is unknown still names the value it attempted.
func setBlockSHA(tag string) string {
	b := []byte(strings.Repeat(tag, 4096/len(tag)+1))[:4096]
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// v3VsWrite writes one round of blocks to every volume through its kernel
// block device (O_DIRECT + fsync: exit 0 means acknowledged and flushed). An
// acknowledged write makes its value the only one the block may hold. A write
// that fails is unknown: the block may then hold any value it could hold
// before, or the attempted one, and nothing else (the spec's shadow rule).
// Required: round.
func v3VsWrite(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
	node, s, err := vsBegin(actx, act, "v3_vs_write")
	if err != nil {
		return nil, err
	}
	round := requireParam(act, actx, "round")
	if !regexp.MustCompile(`^[A-Za-z0-9_-]{1,16}$`).MatchString(round) {
		return nil, errors.New("v3_vs_write: round (1-16 of [A-Za-z0-9_-]) is required")
	}
	outs := s.forEach(ctx, s.selectVolumes(act), func(ctx context.Context, v *vsVolume) outcome {
		dev, why := setDeviceOf(ctx, node, v.NQN, 5*time.Second)
		if why != "" {
			return outcome{Result: "unknown", Detail: why}
		}
		if v.Expect == nil {
			v.Expect = map[string][]string{}
		}
		for _, lba := range lbas(s.Blocks) {
			tag := setBlockTag(v.Name, round, lba)
			gen := fmt.Sprintf(`python3 -c 'import sys;t=sys.argv[1].encode();sys.stdout.buffer.write((t*(4096//len(t)+1))[:4096])' %s`, shellQuote(tag))
			cmd := fmt.Sprintf("set -o pipefail; %s | tee >(sha256sum | cut -c1-64 >&2) | dd of=%s bs=4096 seek=%d count=1 oflag=direct conv=fsync,notrunc iflag=fullblock status=none", gen, dev, lba)
			_, stderr, code, err := node.RunRoot(ctx, "bash -c "+shellQuote(cmd))
			key := strconv.FormatUint(lba, 10)
			want := setBlockSHA(tag)
			if err != nil || code != 0 {
				v.Expect[key] = unknownWrite(v.Expect[key], want)
				return outcome{Result: "unknown", Detail: fmt.Sprintf("write lba %d: code %d %s", lba, code, setFirstLine(stderr))}
			}
			if sum := strings.TrimSpace(stderr); sum != want {
				v.Expect[key] = unknownWrite(v.Expect[key], want)
				return outcome{Result: "unknown", Detail: fmt.Sprintf("write lba %d: the generator wrote %s, not %s", lba, setFirstLine(sum), want)}
			}
			v.Expect[key] = []string{want}
		}
		return outcome{Result: "ok"}
	})
	s.save(actx)
	return report(actx, act, "v3_vs_write", outs)
}

// v3VsVerify reads every expected block of every volume and compares its
// sha256 with the last acknowledged write. A different content is a product
// result (refused here means "the product returned other data").
func v3VsVerify(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
	node, s, err := vsBegin(actx, act, "v3_vs_verify")
	if err != nil {
		return nil, err
	}
	outs := s.forEach(ctx, s.selectVolumes(act), func(ctx context.Context, v *vsVolume) outcome {
		if len(v.Expect) == 0 {
			return outcome{Result: "refused", Detail: "nothing written to verify"}
		}
		dev, why := setDeviceOf(ctx, node, v.NQN, 10*time.Second)
		if why != "" {
			return outcome{Result: "unknown", Detail: why}
		}
		keys := make([]string, 0, len(v.Expect))
		for k := range v.Expect {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			stdout, stderr, code, err := node.RunRoot(ctx, fmt.Sprintf("dd if=%s bs=4096 skip=%s count=1 iflag=direct status=none | sha256sum | cut -c1-64", dev, k))
			if err != nil || code != 0 {
				return outcome{Result: "unknown", Detail: fmt.Sprintf("read lba %s: %s", k, setFirstLine(stderr))}
			}
			if got := strings.TrimSpace(stdout); !setContains(v.Expect[k], got) {
				return outcome{Result: "refused", Detail: fmt.Sprintf("DATA lba %s sha256 %s, want one of %v", k, got, v.Expect[k])}
			}
		}
		return outcome{Result: "ok"}
	})
	return report(actx, act, "v3_vs_verify", outs)
}

// unknownWrite is what a block may hold after a write of want whose outcome
// is unknown: whatever it could hold before (zero for a block never written)
// or want.
func unknownWrite(before []string, want string) []string {
	if len(before) == 0 {
		before = []string{zeroBlockSHA}
	}
	if setContains(before, want) {
		return before
	}
	return append(append([]string(nil), before...), want)
}

func setContains(set []string, v string) bool {
	for _, x := range set {
		if x == v {
			return true
		}
	}
	return false
}

// setChunkOutcome maps an `sw-block ops chunk` exit and its JSON error body
// for the volume-set actions. The outcome table (product/outcomes.json) does
// not name them, so only its rows for every action ("*") apply: exit 0 ok; a
// code those rows refuse, refused; every other answer unknown.
func setChunkOutcome(stdout string, code int) (string, string) {
	return chunkOutcome(opPath{}, stdout, code)
}

// v3VsSnapshot takes a named snapshot of every volume. Required: name.
func v3VsSnapshot(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
	node, s, err := vsBegin(actx, act, "v3_vs_snapshot")
	if err != nil {
		return nil, err
	}
	name := requireParam(act, actx, "name")
	if name == "" {
		return nil, errors.New("v3_vs_snapshot: name is required")
	}
	outs := s.forEach(ctx, s.selectVolumes(act), func(ctx context.Context, v *vsVolume) outcome {
		cmd := fmt.Sprintf("%s ops chunk snapshot --volume %s --status-addr %s --name %s --request-id %s", shellQuote(s.cli()),
			shellQuote(v.Name), shellQuote(v.host().Status), shellQuote(name), shellQuote(v.Name+"-"+name))
		stdout, _, code, err := node.Run(ctx, cmd)
		if err != nil {
			return outcome{Result: "unknown", Detail: err.Error()}
		}
		r, d := setChunkOutcome(stdout, code)
		return outcome{Result: r, Detail: d}
	})
	return report(actx, act, "v3_vs_snapshot", outs)
}

// v3VsSnapshotDelete deletes the named snapshot of every volume. Required: name.
func v3VsSnapshotDelete(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
	node, s, err := vsBegin(actx, act, "v3_vs_snapshot_delete")
	if err != nil {
		return nil, err
	}
	name := requireParam(act, actx, "name")
	if name == "" {
		return nil, errors.New("v3_vs_snapshot_delete: name is required")
	}
	outs := s.forEach(ctx, s.selectVolumes(act), func(ctx context.Context, v *vsVolume) outcome {
		if v.Deleted[name] {
			return outcome{Result: "ok", Detail: "deleted by an earlier step"}
		}
		r, d := s.snapshotDelete(ctx, node, v, name, true)
		return outcome{Result: r, Detail: d}
	})
	s.save(actx)
	return report(actx, act, "v3_vs_snapshot_delete", outs)
}

// snapshotDelete deletes one snapshot: an operator write of HEAD
// (MoveAdmission.OpWrite). While a holder serves the volume it goes through
// that holder (POST /chunk/snapshot/delete), which runs the cleaning right
// after it; the product's documentation says a delete through `sw-block ops
// chunk` (store direct) is cleaned only at the next periodic pass, hourly by
// default. Only the move probe permits store-direct deletion while stopped;
// holderOnly never falls back if the serving holder becomes unavailable.
func (s *vsSet) snapshotDelete(ctx context.Context, node setRunner, v *vsVolume, name string, holderOnly bool) (string, string) {
	var stdout string
	var code int
	var err error
	if holderOnly || setAlive(ctx, node, v.PID) {
		body, _ := json.Marshal(map[string]string{"volumeID": v.Name, "name": name})
		// The body, then the HTTP status alone on the last line.
		var out string
		out, _, code, err = node.Run(ctx, fmt.Sprintf("curl -s -m 60 -X POST -H 'Content-Type: application/json' --data %s -w '\\n%%{http_code}' %s",
			shellQuote(string(body)), shellQuote("http://"+v.host().Status+"/chunk/snapshot/delete")))
		if err != nil || code != 0 {
			return "unknown", fmt.Sprintf("holder not answering (curl exit %d)", code)
		}
		i := strings.LastIndex(strings.TrimRight(out, "\n"), "\n")
		stdout, code = out[:max(i, 0)], 1
		if strings.TrimSpace(out[i+1:]) == "200" {
			code = 0
		}
	} else {
		cmd := fmt.Sprintf("%s ops chunk snapshot-delete --volume %s --name %s --master %s --chunk-filer %s --chunk-path %s", shellQuote(s.cli()),
			shellQuote(v.Name), shellQuote(name), shellQuote(s.Master), shellQuote(s.Filer), shellQuote(s.ChunkPath))
		stdout, _, code, err = node.Run(ctx, cmd)
		if err != nil {
			return "unknown", err.Error()
		}
	}
	r, d := setChunkOutcome(stdout, code)
	if r == "ok" {
		if v.Deleted == nil {
			v.Deleted = map[string]bool{}
		}
		v.Deleted[name] = true
	}
	return r, d
}

// layoutVolumeDir is the volume's directory in the filer: <name>-<id8>.
func layoutVolumeDir(name, layoutID string) string {
	if len(layoutID) < 8 {
		return name
	}
	return name + "-" + layoutID[:8]
}

// v3VsObjects reads each volume's objects in the filer (files under its
// directory's pool/, seg/ and root/, through the filer's HTTP listing; the
// HTTP port is the gRPC port minus 10000, SeaweedFS's convention) and, from
// `sw-block ops chunk usage`, its snapshots and the chunks its current image
// names. Counts are recorded as <save_as>_<volume>_<dir>.
//
// mode=record records only (the listing before writes). mode=live waits
// (bound_seconds, 300) until every volume has no snapshot and exactly its live
// set in pool/: the files there equal the image's own chunks (the product's
// cleaning runs after each snapshot delete). More files at the bound is
// refused (OBJECTS LEFT); a failed listing or usage read is unknown.
func v3VsObjects(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
	node, s, err := vsBegin(actx, act, "v3_vs_objects")
	if err != nil {
		return nil, err
	}
	mode := optionalParam(act, "mode")
	if mode != "record" && mode != "live" {
		return nil, errors.New("v3_vs_objects: mode is record or live")
	}
	host, gport, ok := strings.Cut(s.Filer, ":")
	gp, perr := strconv.Atoi(gport)
	if !ok || perr != nil || gp <= 10000 {
		return nil, fmt.Errorf("v3_vs_objects: filer %q has no SeaweedFS gRPC port", s.Filer)
	}
	base := fmt.Sprintf("http://%s:%d%s", host, gp-10000, strings.TrimRight(s.ChunkPath, "/"))
	bound := time.Duration(actions.ParseInt(optionalParam(act, "bound_seconds"), 300)) * time.Second
	var mu sync.Mutex
	outs := s.forEach(ctx, s.selectVolumes(act), func(ctx context.Context, v *vsVolume) outcome {
		dir := base + "/" + layoutVolumeDir(v.Name, v.LayoutID)
		end := time.Now().Add(bound)
		for {
			counts := map[string]int{}
			for _, sub := range []string{"pool", "seg", "root"} {
				n, why := countFiles(ctx, node, dir+"/"+sub)
				if why != "" {
					return outcome{Result: "unknown", Detail: why}
				}
				counts[sub] = n
			}
			if act.SaveAs != "" {
				mu.Lock()
				for sub, n := range counts {
					actx.Vars[fmt.Sprintf("%s_%s_%s", act.SaveAs, v.Name, sub)] = strconv.Itoa(n)
				}
				mu.Unlock()
			}
			summary := fmt.Sprintf("pool %d seg %d root %d", counts["pool"], counts["seg"], counts["root"])
			if mode == "record" {
				return outcome{Result: "ok", Detail: summary}
			}
			u, why := chunkUsage(ctx, node, s, v.Name)
			if why != "" {
				return outcome{Result: "unknown", Detail: why}
			}
			if u.Snapshots == 0 && counts["pool"] == u.OwnChunks {
				return outcome{Result: "ok", Detail: summary}
			}
			if time.Now().After(end) {
				return outcome{Result: "refused", Detail: fmt.Sprintf("OBJECTS LEFT after %s: %s; usage snapshots %d own_chunks %d", bound, summary, u.Snapshots, u.OwnChunks)}
			}
			select {
			case <-ctx.Done():
				return outcome{Result: "unknown", Detail: ctx.Err().Error()}
			case <-time.After(2 * time.Second):
			}
		}
	})
	return report(actx, act, "v3_vs_objects "+mode, outs)
}

// vsUsage is the answer of `sw-block ops chunk usage`.
type vsUsage struct {
	Snapshots     int `json:"snapshots"`
	Dependents    int `json:"dependents"`
	OwnChunks     int `json:"own_chunks"`
	ForeignChunks int `json:"foreign_chunks"`
}

func chunkUsage(ctx context.Context, node setRunner, s *vsSet, vol string) (vsUsage, string) {
	var u vsUsage
	stdout, _, code, err := node.Run(ctx, fmt.Sprintf("%s ops chunk usage --volume %s --master %s --chunk-filer %s --chunk-path %s", shellQuote(s.cli()),
		shellQuote(vol), shellQuote(s.Master), shellQuote(s.Filer), shellQuote(s.ChunkPath)))
	if err != nil {
		return u, "usage: " + err.Error()
	}
	if r, d := setChunkOutcome(stdout, code); r != "ok" {
		return u, "usage " + r + ": " + d
	}
	if json.Unmarshal([]byte(strings.TrimSpace(stdout)), &u) != nil {
		return u, "usage answer is not JSON"
	}
	return u, ""
}

// countFiles counts every file below dir through the filer's JSON listing.
func countFiles(ctx context.Context, node setRunner, dir string) (int, string) {
	py := `import json,sys,urllib.request,urllib.parse,urllib.error
def ls(d):
    n,last=0,""
    while True:
        q=urllib.parse.urlencode({"limit":1000,"lastFileName":last})
        try:
            r=json.load(urllib.request.urlopen(urllib.request.Request(d+"/?"+q,headers={"Accept":"application/json"}),timeout=10))
        except urllib.error.HTTPError as e:
            if e.code==404 and not last: return 0
            raise
        es=r.get("Entries") or []
        for e in es:
            p=e["FullPath"]
            n+=ls(d.split("/",3)[0]+"//"+d.split("/",3)[2]+urllib.parse.quote(p)) if e.get("Mode",0)&0o20000000000 else 1
        if not r.get("ShouldDisplayLoadMore") or not es: return n
        last=es[-1]["FullPath"].rsplit("/",1)[1]
print(ls(sys.argv[1]))`
	stdout, stderr, code, err := node.Run(ctx, fmt.Sprintf("python3 -I -c %s %s", shellQuote(py), shellQuote(dir)))
	n, perr := strconv.Atoi(strings.TrimSpace(stdout))
	if err != nil || code != 0 || perr != nil {
		return 0, "listing " + dir + ": " + setFirstLine(stderr)
	}
	return n, ""
}

// v3VsRestart stops one volume's blockvolume by its recorded pid (signal,
// default TERM; KILL for a crash), then starts it again on the same host and
// waits until it serves. Optional: index (1-based, default 1), signal,
// ready_seconds (60).
func v3VsRestart(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
	node, s, err := vsBegin(actx, act, "v3_vs_restart")
	if err != nil {
		return nil, err
	}
	i := actions.ParseInt(optionalParam(act, "index"), 1) - 1
	sig := optionalParam(act, "signal")
	if sig == "" {
		sig = "TERM"
	}
	if i < 0 || i >= len(s.Volumes) || (sig != "TERM" && sig != "KILL") {
		return nil, errors.New("v3_vs_restart: index in the set, signal TERM or KILL")
	}
	v := &s.Volumes[i]
	bound := time.Duration(actions.ParseInt(optionalParam(act, "ready_seconds"), 60)) * time.Second
	start := time.Now()
	o := func() outcome {
		if why := setNvmeDisconnect(ctx, node, v.NQN); why != "" {
			return outcome{Result: "unknown", Detail: why}
		}
		if why := stopPID(ctx, node, v.PID, sig, 30*time.Second); why != "" {
			return outcome{Result: "unknown", Detail: why}
		}
		return s.attach(ctx, node, v, 0, bound)
	}()
	o.Volume, o.Dur = v.Name, time.Since(start)
	s.save(actx)
	return report(actx, act, "v3_vs_restart", []outcome{o})
}

// stopPID signals pid and waits until it is gone.
func stopPID(ctx context.Context, node setRunner, pid, sig string, bound time.Duration) string {
	if !regexp.MustCompile(`^[0-9]+$`).MatchString(pid) {
		return "no recorded pid"
	}
	_, _, _, _ = node.Run(ctx, fmt.Sprintf("kill -%s %s 2>/dev/null", sig, pid))
	return setWaitGone(ctx, node, pid, bound)
}

// alive reports whether the recorded pid still runs.
func setAlive(ctx context.Context, node setRunner, pid string) bool {
	if !regexp.MustCompile(`^[0-9]+$`).MatchString(pid) {
		return false
	}
	out, _, _, _ := node.Run(ctx, "kill -0 "+pid+" 2>/dev/null && echo alive")
	return strings.TrimSpace(out) == "alive"
}

func setWaitGone(ctx context.Context, node setRunner, pid string, bound time.Duration) string {
	end := time.Now().Add(bound)
	for time.Now().Before(end) {
		if out, _, _, _ := node.Run(ctx, "kill -0 "+pid+" 2>/dev/null && echo alive"); strings.TrimSpace(out) != "alive" {
			return ""
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Sprintf("pid %s still running after %s", pid, bound)
}

// v3VsServing checks that every volume serves on its current host at an
// epoch at least the recorded one, within bound_seconds (60): no volume stuck.
func v3VsServing(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
	node, s, err := vsBegin(actx, act, "v3_vs_serving")
	if err != nil {
		return nil, err
	}
	bound := time.Duration(actions.ParseInt(optionalParam(act, "bound_seconds"), 60)) * time.Second
	outs := s.forEach(ctx, s.selectVolumes(act), func(ctx context.Context, v *vsVolume) outcome {
		e, why := waitServing(ctx, node, v, max(v.Epoch, 1)-1, bound)
		if why != "" {
			r, d, _ := strings.Cut(why, ": ")
			return outcome{Result: r, Detail: "STUCK " + d}
		}
		v.Epoch = e
		return outcome{Result: "ok"}
	})
	s.save(actx)
	return report(actx, act, "v3_vs_serving", outs)
}

// Planned move answers, as the CLI prints them.
var (
	reRevoke = regexp.MustCompile(`^revocation requested: volume (\S+) holder r1@(\d+) expiry_ns `)
	reMoved  = regexp.MustCompile(`^clean move issued: volume (\S+) request (\S+): r1@(\d+) -> r1@(\d+) on (\S+) \(sealed HEAD gen (\d+), remote LSN (\d+)\)$`)
)

// parseDetach reads the sealed generation of a plain detach: the JSON answer
// on stdout and exactly "sealed HEAD generation: G" on stderr, both naming
// the same generation, for this volume at this epoch.
func setParseDetach(stdout, stderr, vol string, epoch uint64) (uint64, error) {
	var a struct {
		VolumeID  string `json:"volumeID"`
		HeadGen   uint64 `json:"headGen"`
		HeadEpoch uint64 `json:"headEpoch"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &a); err != nil {
		return 0, fmt.Errorf("detach answer is not JSON: %q", setFirstLine(stdout))
	}
	if a.VolumeID != vol || a.HeadGen == 0 || a.HeadEpoch != epoch {
		return 0, fmt.Errorf("detach answer %+v; want %s at epoch %d with a generation", a, vol, epoch)
	}
	if strings.TrimSpace(stderr) != fmt.Sprintf("sealed HEAD generation: %d", a.HeadGen) {
		return 0, fmt.Errorf("detach stderr %q does not state generation %d", setFirstLine(stderr), a.HeadGen)
	}
	return a.HeadGen, nil
}

// showsAck reports whether `ops workspace show` reports the recorded ACK of
// r1 at epoch for generation gen, the generation the source sealed. The ACK
// is the source's own report of its seal, so a later operator write of HEAD
// does not change it.
//
// Valid for this scripted hand-over only: binding steps by a HEAD generation
// holds while nothing else writes HEAD between them, which the product itself
// does not rely on (the master admits a clean move by epoch, holder and
// position). Do not reuse showsAck or parseMoved in a generator.
func showsAck(out string, epoch, gen uint64) bool {
	want := fmt.Sprintf("recorded ACK: r1@%d HEAD generation %d", epoch, gen)
	for _, l := range strings.Split(out, "\n") {
		if strings.TrimRight(l, "\r") == want {
			return true
		}
	}
	return false
}

// parseMoved reads the new epoch from a clean move's first line, bound to the
// volume, its request, the old epoch and the target. The generation it names
// is HEAD's at the move: the sealed one, or a later one when an operator
// write (a snapshot delete, a cleaning) moved HEAD after the stop. So it must
// be at least the sealed generation, and the answer must also equal what
// `ops chunk show` reads of HEAD just before the move (headGen); see showsAck
// for the limits of binding by generation.
func parseMoved(out, vol, request string, epoch, sealedGen, headGen uint64, target string) (uint64, error) {
	first, _, _ := strings.Cut(out, "\n")
	m := reMoved.FindStringSubmatch(strings.TrimRight(first, "\r"))
	if m == nil || m[1] != vol || m[2] != request || m[3] != strconv.FormatUint(epoch, 10) || m[5] != target {
		return 0, fmt.Errorf("clean move answer %q does not confirm %s %s r1@%d -> %s", setFirstLine(out), vol, request, epoch, target)
	}
	g, _ := strconv.ParseUint(m[6], 10, 64)
	if g < sealedGen || g != headGen {
		return 0, fmt.Errorf("clean move names HEAD gen %d; want HEAD's gen %d read before the move, at least the sealed %d", g, headGen, sealedGen)
	}
	e2, _ := strconv.ParseUint(m[4], 10, 64)
	if e2 <= epoch {
		return 0, fmt.Errorf("clean move to epoch %d, not above %d", e2, epoch)
	}
	return e2, nil
}

// headGen reads HEAD's generation with `ops chunk show` (store direct).
func (s *vsSet) headGen(ctx context.Context, node setRunner, vol string) (uint64, string) {
	stdout, _, code, err := node.Run(ctx, fmt.Sprintf("%s ops chunk show --volume %s --master %s --chunk-filer %s --chunk-path %s", shellQuote(s.cli()),
		shellQuote(vol), shellQuote(s.Master), shellQuote(s.Filer), shellQuote(s.ChunkPath)))
	if err != nil {
		return 0, err.Error()
	}
	if r, d := setChunkOutcome(stdout, code); r != "ok" {
		return 0, "show " + r + ": " + d
	}
	var body struct {
		Head *struct {
			Gen uint64 `json:"Gen"`
		} `json:"head"`
	}
	if json.Unmarshal([]byte(strings.TrimSpace(stdout)), &body) != nil || body.Head == nil || body.Head.Gen == 0 {
		return 0, "show answer has no HEAD generation"
	}
	return body.Head.Gen, ""
}

// v3VsMove's `sw-block ops workspace` exits map as the move action's in the
// outcome table (wsOutcome): its revoke and move --clean both come after its
// detach --plain sealed HEAD, so the move as a whole changed state.

// v3VsMove moves the first n volumes (param first, default 10) to their
// second host with the planned hand-over: disconnect the guest; detach
// --plain; revoke with the sealed generation; wait for the source to exit by
// itself; show until the recorded ACK; move --clean; start on the target and
// connect. The freeze is from the detach to the target serving.
//
// Optional: op_write_snapshot names a snapshot that the FIRST moved volume
// deletes after its ACK and before its move (an operator write of HEAD
// between the stop and the clean move).
func v3VsMove(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
	node, s, err := vsBegin(actx, act, "v3_vs_move")
	if err != nil {
		return nil, err
	}
	if optionalParam(act, "first") == "" {
		act.Params["first"] = "10"
	}
	opWrite := optionalParam(act, "op_write_snapshot")
	bound := time.Duration(actions.ParseInt(optionalParam(act, "ready_seconds"), 90)) * time.Second
	sel := s.selectVolumes(act)
	outs := s.forEach(ctx, sel, func(ctx context.Context, v *vsVolume) outcome {
		if v.Moved || v.Cur != 0 {
			return outcome{Result: "refused", Detail: "already moved"}
		}
		if why := setNvmeDisconnect(ctx, node, v.NQN); why != "" {
			return outcome{Result: "unknown", Detail: why}
		}
		start := time.Now()
		src, dst := v.Hosts[0], v.Hosts[1]
		stdout, stderr, code, err := node.Run(ctx, fmt.Sprintf("%s ops chunk detach --plain --volume %s --status-addr %s --timeout 120s", shellQuote(s.cli()), shellQuote(v.Name), shellQuote(src.Status)))
		if err != nil {
			return outcome{Result: "unknown", Detail: err.Error()}
		}
		if r, d := setChunkOutcome(stdout, code); r != "ok" {
			return outcome{Result: r, Detail: "detach: " + d}
		}
		gen, perr := setParseDetach(stdout, stderr, v.Name, v.Epoch)
		if perr != nil {
			return outcome{Result: "unknown", Detail: perr.Error()}
		}
		ws := func(args ...string) (string, string, int, error) {
			return node.Run(ctx, shellQuote(s.cli())+" ops workspace "+strings.Join(quoteAll(append(args, "--master", s.Master, "--volume", v.Name)), " "))
		}
		stdout, stderr, code, err = ws("revoke", "--replica", "r1", "--epoch", strconv.FormatUint(v.Epoch, 10), "--sealed-gen", strconv.FormatUint(gen, 10))
		if err != nil || wsOutcome(wsMove, wsRevoke, code) != "ok" || !reRevoke.MatchString(stdout) {
			return outcome{Result: wsOutcomeOr(err, wsRevoke, code), Detail: "revoke: " + setFirstLine(stdout+" "+stderr)}
		}
		if why := setWaitGone(ctx, node, v.PID, 40*time.Second); why != "" {
			return outcome{Result: "unknown", Detail: "the source did not exit by itself after the revoke: " + why}
		}
		acked := false
		for end := time.Now().Add(30 * time.Second); time.Now().Before(end) && !acked; time.Sleep(500 * time.Millisecond) {
			out, _, c, e := ws("show")
			acked = e == nil && c == 0 && showsAck(out, v.Epoch, gen)
		}
		if !acked {
			return outcome{Result: "unknown", Detail: fmt.Sprintf("no recorded ACK of r1@%d gen %d", v.Epoch, gen)}
		}
		// An operator write of HEAD between the stop and the move: the clean
		// move must still be admitted (MoveAdmission.WitCleanAfterOpWrite).
		if opWrite != "" && v.Name == s.Volumes[sel[0]].Name {
			if r, d := s.snapshotDelete(ctx, node, v, opWrite, false); r != "ok" {
				return outcome{Result: r, Detail: "snapshot delete between the stop and the move: " + d}
			}
		}
		hg, why := s.headGen(ctx, node, v.Name)
		if why != "" {
			return outcome{Result: "unknown", Detail: why}
		}
		if opWrite != "" && v.Name == s.Volumes[sel[0]].Name && hg <= gen {
			return outcome{Result: "unknown", Detail: fmt.Sprintf("the snapshot delete did not move HEAD past the sealed gen %d (HEAD gen %d)", gen, hg)}
		}
		req := v.Name + "-move-1"
		stdout, stderr, code, err = ws("move", "--clean", "--replica", "r1", "--epoch", strconv.FormatUint(v.Epoch, 10),
			"--target-host", dst.Server, "--target-data", dst.Data, "--target-ctrl", dst.Ctrl, "--request-id", req)
		if err != nil || wsOutcome(wsMove, wsMoveClean, code) != "ok" {
			return outcome{Result: wsOutcomeOr(err, wsMoveClean, code), Detail: "move: " + setFirstLine(stdout+" "+stderr)}
		}
		e2, perr := parseMoved(stdout, v.Name, req, v.Epoch, gen, hg, dst.Server)
		if perr != nil {
			return outcome{Result: "unknown", Detail: perr.Error()}
		}
		v.Cur, v.Moved = 1, true
		if o := s.attach(ctx, node, v, e2-1, bound); o.Result != "ok" {
			return outcome{Result: o.Result, Detail: "target: " + o.Detail}
		}
		// The freeze ends when the target serves; the connect after it is the guest's.
		return outcome{Result: "ok", Dur: time.Since(start)}
	})
	s.save(actx)
	return report(actx, act, "v3_vs_move", outs)
}

func wsOutcomeOr(err error, path string, code int) string {
	if err != nil {
		return "unknown"
	}
	return wsOutcome(wsMove, path, code)
}

// v3VsStop disconnects every volume and stops its blockvolume by its
// recorded pid. Always phases call it; it settles all volumes and fails if any
// connection or process remains.
func v3VsStop(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
	if actx.Vars[volSetVar] == "" {
		actx.Log("  v3_vs_stop: no volume set was built; nothing to stop")
		return map[string]string{"value": "0"}, nil
	}
	node, s, err := vsBegin(actx, act, "v3_vs_stop")
	if err != nil {
		return nil, err
	}
	outs := s.forEach(ctx, s.selectVolumes(act), func(ctx context.Context, v *vsVolume) outcome {
		_ = setNvmeDisconnect(ctx, node, v.NQN)
		if v.PID != "" {
			if why := stopPID(ctx, node, v.PID, "TERM", 15*time.Second); why != "" {
				if why = stopPID(ctx, node, v.PID, "KILL", 10*time.Second); why != "" {
					return outcome{Result: "unknown", Detail: why}
				}
			}
		}
		if _, why := setDeviceOf(ctx, node, v.NQN, time.Second); why == "" {
			return outcome{Result: "unknown", Detail: "NVMe connection remains"}
		}
		return outcome{Result: "ok"}
	})
	// Independently enumerate the whole run, including daemons whose launch
	// reply was lost before their PID reached the state. Residue is never PASS.
	sum := sha256.Sum256([]byte(s.Root))
	prefix := "nqn.2026-11.io.seaweedfs:vs-" + hex.EncodeToString(sum[:8]) + "-"
	stdout, stderr, code, sweepErr := node.RunRoot(ctx, fmt.Sprintf("python3 -I -c %s %s %s", shellQuote(hostAbsenceSweep), shellQuote(s.Bin+"/blockvolume"), shellQuote(prefix)))
	if sweepErr != nil || code != 0 {
		outs = append(outs, outcome{Volume: "run-sweep", Result: "unknown", Detail: fmt.Sprintf("residue or absence unreadable: %s %s %v", setFirstLine(stdout), setFirstLine(stderr), sweepErr)})
	}
	return report(actx, act, "v3_vs_stop", outs)
}

// This sweep is independent of recorded PIDs and per-volume connection state.
const hostAbsenceSweep = `import os,sys,glob,json
binary,prefix=sys.argv[1:];pids=[];nqns=[]
for p in os.listdir("/proc"):
    if not p.isdigit(): continue
    try:
        if open("/proc/"+p+"/cmdline","rb").read().split(b"\0")[0].decode()==binary: pids.append(p)
    except FileNotFoundError: pass
for path in glob.glob("/sys/class/nvme/nvme*/subsysnqn"):
    try:
        nqn=open(path).read().strip()
        if nqn.startswith(prefix): nqns.append(nqn)
    except FileNotFoundError: pass
print(json.dumps(dict(pids=pids,nqns=nqns)))
sys.exit(1 if pids or nqns else 0)
`

// hostSampler samples the host CPU and this run's blockvolume RSS/fds each
// second. Matching the executable follows replacement PIDs after restarts/moves.
const hostSampler = `import sys,time,os,json
binary,out,stop=sys.argv[1:]
def cpu():
    f=[int(x) for x in open("/proc/stat").readline().split()[1:]]
    return sum(f),f[3]+f[4]
def processes():
    rss=fds=0
    for p in os.listdir("/proc"):
        if not p.isdigit(): continue
        try:
            if open("/proc/"+p+"/cmdline","rb").read().split(b"\0")[0].decode()!=binary: continue
            for line in open("/proc/"+p+"/status"):
                if line.startswith("VmRSS:"): rss+=int(line.split()[1])
            fds+=len(os.listdir("/proc/"+p+"/fd"))
        except (OSError,UnicodeError): pass
    return rss/1024,fds
start=time.monotonic(); last=start; next_tick=start+1
peak=dict(samples=0,rss_mib=0,open_fds=0,cpu_busy_pct=0,max_gap_s=0,started_unix=time.time(),elapsed_s=0,complete=False)
t0,i0=cpu()
open(out+".ready","x").close()
end=start+7200
while True:
    time.sleep(max(0,next_tick-time.monotonic()))
    t1,i1=cpu();rss,fds=processes()
    now=time.monotonic(); next_tick+=1
    peak["max_gap_s"]=max(peak["max_gap_s"],now-last);last=now
    peak["elapsed_s"]=now-start
    peak["stopped_unix"]=time.time()
    peak["complete"]=os.path.exists(stop)
    peak["samples"]+=1
    peak["rss_mib"]=max(peak["rss_mib"],rss)
    peak["open_fds"]=max(peak["open_fds"],fds)
    peak["cpu_busy_pct"]=max(peak["cpu_busy_pct"],100*(1-(i1-i0)/max(1,t1-t0)))
    with open(out+".tmp","w") as f: json.dump(peak,f)
    os.replace(out+".tmp",out)
    t0,i0=t1,i1
    if peak["complete"] or now>=end: break
open(out+".done","x").close()
`

type hostPeak struct {
	Samples  int     `json:"samples"`
	RSS      float64 `json:"rss_mib"`
	FDs      int     `json:"open_fds"`
	CPU      float64 `json:"cpu_busy_pct"`
	MaxGap   float64 `json:"max_gap_s"`
	Elapsed  float64 `json:"elapsed_s"`
	Complete bool    `json:"complete"`
}

func (p hostPeak) valid() bool {
	return p.Samples >= 1 && p.Complete && p.MaxGap > 0 && p.MaxGap <= 2 && p.Elapsed > 0 && float64(p.Samples) >= math.Floor(p.Elapsed)-1 && p.RSS >= 0 && p.FDs >= 0 && p.CPU >= 0 && p.CPU <= 100
}

// v3VsHost brackets every IO phase with start/stop. The stop reports maxima,
// not an observation presented as a peak. Cleanup also stops a partial run.
func v3VsHost(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
	node, s, err := vsBegin(actx, act, "v3_vs_host")
	if err != nil {
		return nil, err
	}
	path := s.Root + "/host-samples.json"
	stop := path + ".stop"
	mode := optionalParam(act, "mode")
	var cmd string
	switch mode {
	case "start":
		cmd = fmt.Sprintf("mkdir -p %s; test ! -e %s || exit 2\nprintf %%s %s > %s\nsetsid python3 -I %s %s %s %s > %s 2>&1 < /dev/null &\n", shellQuote(s.Root), shellQuote(path+".py"), shellQuote(hostSampler), shellQuote(path+".py"), shellQuote(path+".py"), shellQuote(s.Bin+"/blockvolume"), shellQuote(path), shellQuote(stop), shellQuote(path+".log"))
		cmd += fmt.Sprintf("for i in $(seq 1 50); do [ -e %s ] && exit 0; sleep 0.1; done; echo 'sampler did not start' >&2; exit 1", shellQuote(path+".ready"))
	case "stop", "cleanup":
		cmd = fmt.Sprintf("[ -e %s ] || exit 0\ntouch %s\nfor i in $(seq 1 50); do [ -e %s ] && { cat %s; exit 0; }; sleep 0.1; done\necho 'sampler did not stop' >&2; exit 1", shellQuote(path+".py"), shellQuote(stop), shellQuote(path+".done"), shellQuote(path))
	default:
		return nil, fmt.Errorf("v3_vs_host: mode must be start, stop or cleanup")
	}
	stdout, stderr, code, err := node.RunRoot(ctx, cmd)
	if err != nil || code != 0 {
		return nil, fmt.Errorf("v3_vs_host %s: exit %d %s %v", mode, code, setFirstLine(stderr), err)
	}
	if mode == "start" || mode == "cleanup" {
		return map[string]string{"value": mode}, nil
	}
	var peak hostPeak
	if err := json.Unmarshal([]byte(stdout), &peak); err != nil || !peak.valid() {
		return nil, fmt.Errorf("v3_vs_host: missing/invalid samples: %s", setFirstLine(stdout))
	}
	res := map[string]string{"value": strconv.Itoa(peak.Samples), "samples": strconv.Itoa(peak.Samples), "max_gap_s": fmt.Sprintf("%.3f", peak.MaxGap), "rss_mib": fmt.Sprintf("%.1f", peak.RSS), "open_fds": strconv.Itoa(peak.FDs), "cpu_busy_pct": fmt.Sprintf("%.1f", peak.CPU)}
	if act.SaveAs != "" {
		for k, v := range res {
			if k != "value" {
				actx.Vars[act.SaveAs+"_"+k] = v
			}
		}
	}
	actx.Log("  v3_vs_host maxima: samples=%s max_gap=%ss RSS=%s MiB fds=%s CPU=%s%%", res["samples"], res["max_gap_s"], res["rss_mib"], res["open_fds"], res["cpu_busy_pct"])
	return res, nil
}
