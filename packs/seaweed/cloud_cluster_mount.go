package seaweed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pingqiu/sw-test-runner/inventory"
)

// The mount soak on the ten-node cloud layout: each of the three clients runs its own worker, with its own filer path
// and run root, against the shared filer and every volume server. Each client is an independent oracle; no client
// reads another's files, so this is not a cross-client file-sharing test.

// mountClients is the frozen roster; no client is added or dropped at run time.
var mountClients = []string{"client1", "client2", "client3"}

// Per-client bounds after the worker: the listing and the stream together, then the assessor.
var mountFetchBound, mountAssessBound = 4 * time.Minute, time.Duration(mountSoakAssessBudget) * time.Second

// mountStreamGrace is how long a cancelled stream may take to report its end.
var mountStreamGrace = 10 * time.Second

// mountAssess runs the embedded assessor over one admitted run root, on the controller; a var so a caller test can
// stand in for it.
var mountAssess = func(ctx context.Context, script, dir string) ([]byte, error) {
	return exec.CommandContext(ctx, "python3", "-I", script, "--root", dir).Output()
}

const mountClusterScope = "independent per-client namespaces on one shared filer and the volume servers; not a cross-client file-sharing test"

// clientMount is one client's outcome: its verdict, or why it has none.
type clientMount struct {
	Client        string          `json:"client"`
	Boot          string          `json:"boot,omitempty"`
	ListingSHA256 string          `json:"listing_sha256,omitempty"`
	Worker        string          `json:"worker"`
	Missing       string          `json:"missing,omitempty"`
	Verdict       json.RawMessage `json:"verdict,omitempty"`
	status        string
	cleanup       string
	gates         int
}

type clusterMountResult struct {
	Schema  string        `json:"schema"`
	Status  string        `json:"status"`
	Cleanup string        `json:"cleanup"`
	Scope   string        `json:"scope"`
	Clients []clientMount `json:"clients"`
}

// mountRunStatus. A client verdict is valid only when it is present, its worker cleanup is PROVEN and it has no gate
// reason. The run is FAIL if any client has a valid FAIL, never hidden by another client; PASS only if all three are
// valid PASS; otherwise INVALID. Cleanup is PROVEN only if every client's is, and is reported beside the status.
func mountRunStatus(cs []clientMount) (status, cleanup string) {
	valid := func(c clientMount) bool { return c.Missing == "" && c.cleanup == "PROVEN" && c.gates == 0 }
	status, cleanup = "PASS", "PROVEN"
	for _, c := range cs {
		if c.Missing != "" || c.cleanup != "PROVEN" {
			cleanup = "UNKNOWN"
		}
		if !valid(c) || c.status != "PASS" {
			status = "INVALID"
		}
	}
	for _, c := range cs {
		if valid(c) && c.status == "FAIL" {
			return "FAIL", cleanup
		}
	}
	return status, cleanup
}

// mountVolumes lists every volume server's HTTP endpoint as [host, port], in host id order.
func mountVolumes(hosts []inventory.Host) ([][]any, error) {
	sorted := append([]inventory.Host{}, hosts...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	var out [][]any
	for _, v := range sorted {
		host, port, err := net.SplitHostPort(endpoint(v, "volume_http"))
		if err != nil {
			return nil, fmt.Errorf("volume %s has no volume_http", v.ID)
		}
		p, err := strconv.Atoi(port)
		if err != nil {
			return nil, err
		}
		out = append(out, []any{host, p})
	}
	if len(out) == 0 {
		return nil, errors.New("no volume server")
	}
	return out, nil
}

// clientMountConfig is the worker config for one client.
func clientMountConfig(d inventory.Document, h, meta inventory.Host, volumes [][]any) ([]byte, error) {
	sched := scheduleFor(MountSoakCase)
	filer := endpoint(meta, "filer_http")
	if filer == "" {
		return nil, errors.New("meta has no filer_http")
	}
	return json.Marshal(struct {
		mountSoakConfig
		Volumes [][]any `json:"volumes"`
	}{mountSoakConfig{Root: h.RunRoot + "/mountsoak", Weed: h.RunRoot + "/weed", Filer: filer, FilerPath: "/testops/" + d.RunID + "/mountsoak/" + h.ID,
		PhasesS: sched.PhasesS, EpisodesS: sched.EpisodesS, Iptables: []string{"sudo", "-n"}, Mounts: mountSoakMounts, DualS: sched.DualS, Budget: sched.Budget}, volumes})
}

// mountNode is what one client needs from its connection.
type mountNode interface {
	Run(context.Context, string) (string, string, int, error)
	Upload(string, string) error
	StreamRun(context.Context, string, io.Writer) error
}

// runClusterMount runs the three clients in parallel and publishes mount-verdicts.json whatever they return.
func runClusterMount(ctx context.Context, d inventory.Document, c CloudConfig, groups map[string][]inventory.Host, dial func(inventory.Host) (mountNode, func(), error)) (string, error) {
	byID := map[string]inventory.Host{}
	for _, h := range groups["client"] {
		byID[h.ID] = h
	}
	if len(byID) != len(mountClients) || len(groups["master"]) != 1 {
		return "", errors.New("mount soak needs exactly client1-3 and one meta host")
	}
	volumes, err := mountVolumes(groups["volume"])
	if err != nil {
		return "", err
	}
	// The whole roster and every config are checked before any client starts, so a refusal never leaves one running.
	hosts, cfgs := make([]inventory.Host, len(mountClients)), make([][]byte, len(mountClients))
	for i, id := range mountClients {
		h, ok := byID[id]
		if !ok {
			return "", fmt.Errorf("mount soak roster lacks %s", id)
		}
		hosts[i] = h
		if cfgs[i], err = clientMountConfig(d, h, groups["master"][0], volumes); err != nil {
			return "", err
		}
	}
	results := make([]clientMount, len(mountClients))
	var wg sync.WaitGroup
	for i := range mountClients {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = runClientMount(ctx, c, hosts[i], cfgs[i], dial)
		}(i)
	}
	wg.Wait()
	status, cleanup := mountRunStatus(results)
	raw, err := json.MarshalIndent(clusterMountResult{Schema: "seaweed-mount-cluster/1", Status: status, Cleanup: cleanup, Scope: mountClusterScope, Clients: results}, "", "  ")
	if err != nil {
		return "", err
	}
	rows := filepath.Join(c.Output, "mount-verdicts.json")
	if err = os.WriteFile(rows, raw, 0600); err != nil {
		return "", err
	}
	if status != "PASS" {
		return rows, fmt.Errorf("mount soak verdict %s (cleanup %s)", status, cleanup)
	}
	return rows, nil
}

// runClientMount: worker, then the bounded listing and stream, admission, identity, and the assessor. Any failure
// after the worker makes the client missing, with the first reason; nothing is inferred for it.
func runClientMount(ctx context.Context, c CloudConfig, h inventory.Host, cfg []byte, dial func(inventory.Host) (mountNode, func(), error)) (r clientMount) {
	r = clientMount{Client: h.ID, Worker: "not started"}
	out := filepath.Join(c.Output, "mount", h.ID)
	if e := os.MkdirAll(out, 0700); e != nil {
		r.Missing = e.Error()
		return
	}
	n, closeNode, e := dial(h)
	if e != nil {
		r.Missing = "connect: " + e.Error()
		return
	}
	defer closeNode()
	seq := 0
	step := func(ctx context.Context, name string, secs int, cmd string) (string, error) {
		seq++
		return cloudStep(ctx, n, out, fmt.Sprintf("mount-step-%03d.json", seq), name, secs, cmd)
	}
	q, root := cloudQuote, h.RunRoot+"/mountsoak"
	boot, e := step(ctx, "boot", 30, "cat /proc/sys/kernel/random/boot_id")
	if e != nil {
		r.Missing = "boot id: " + e.Error()
		return
	}
	r.Boot = strings.TrimSpace(boot)
	if e = stageClientMount(ctx, c, h, n, out, &seq, step); e != nil {
		r.Missing = "stage: " + e.Error()
		return
	}
	_, e = step(ctx, "mount-soak", scheduleFor(MountSoakCase).WorkerS, "cd "+q(h.RunRoot)+" && python3 -I worker/mount_soak.py --config "+q(string(cfg)))
	r.Worker = "ok"
	if e != nil {
		r.Worker = e.Error() // the assessor still decides from what the worker wrote
	}
	dir := filepath.Join(out, "run-root")
	if e = fetchClientRoot(ctx, n, root, dir, r.Boot, &r, step); e != nil {
		r.Missing = e.Error()
		return
	}
	actx, cancel := context.WithTimeout(ctx, mountAssessBound)
	defer cancel()
	body, e := workerFiles.ReadFile("worker/mount_assess.py")
	if e != nil {
		r.Missing = e.Error()
		return
	}
	script := filepath.Join(out, "mount_assess.py")
	if e = os.WriteFile(script, body, 0600); e != nil {
		r.Missing = e.Error()
		return
	}
	raw, e := mountAssess(actx, script, dir)
	_ = os.WriteFile(filepath.Join(out, "mount-verdict.json"), raw, 0600) // kept as a diagnostic whatever happened
	if e != nil {
		r.Missing = "assessor did not complete: " + e.Error() // its output is not a verdict, even if it decodes
		return
	}
	v, verr := decodeMountVerdict(raw)
	if verr != nil {
		r.Missing = "assessor: " + verr.Error()
		return
	}
	r.Verdict, r.status, r.cleanup, r.gates = json.RawMessage(raw), v.Status, v.Cleanup, len(v.GateReasons)
	return
}

// stageClientMount uploads weed and the worker to the client and proves both by sha256.
func stageClientMount(ctx context.Context, c CloudConfig, h inventory.Host, n mountNode, out string, seq *int, step func(context.Context, string, int, string) (string, error)) error {
	q := cloudQuote
	weed, ok := c.Files["weed"]
	if !ok {
		return errors.New("no weed artifact")
	}
	body, err := workerFiles.ReadFile("worker/mount_soak.py")
	if err != nil {
		return err
	}
	local := filepath.Join(out, "mount_soak.py")
	if err = os.WriteFile(local, body, 0600); err != nil {
		return err
	}
	if _, err = step(ctx, "mount-prepare", 30, "mkdir -p "+q(h.RunRoot+"/worker")+" "+q(h.RunRoot+"/mountsoak")); err != nil {
		return err
	}
	for _, f := range []struct{ name, local, remote, sha string }{
		{"weed", weed.Path, h.RunRoot + "/weed", weed.SHA256},
		{"mount_soak.py", local, h.RunRoot + "/worker/mount_soak.py", hash(body)},
	} {
		*seq++
		if err = cloudUpload(n, out, fmt.Sprintf("mount-step-%03d.json", *seq), f.name, f.local, f.remote); err != nil {
			return err
		}
		if _, err = step(ctx, "verify-"+f.name, 60, "printf '%s  %s\\n' "+q(f.sha)+" "+q(f.remote)+" | sha256sum -c - && chmod 0755 "+q(f.remote)); err != nil {
			return err
		}
	}
	return nil
}

// fetchClientRoot: the client's listing, then its stream, admitted under the limits, then the identity check, all
// inside one bound. A stream that ends with an error is a failure even if what arrived was admitted.
func fetchClientRoot(ctx context.Context, n mountNode, root, dir, boot string, r *clientMount, step func(context.Context, string, int, string) (string, error)) error {
	fctx, cancel := context.WithTimeout(ctx, mountFetchBound)
	defer cancel()
	listing, err := step(fctx, "listing", 120, mountListCommand(root))
	if err != nil {
		return fmt.Errorf("listing: %w", err)
	}
	l, err := parseMountListing(listing)
	r.ListingSHA256 = l.SHA256
	if err != nil {
		return fmt.Errorf("listing: %w", err)
	}
	pr, pw := io.Pipe()
	streamed := make(chan error, 1)
	go func() {
		e := n.StreamRun(fctx, mountTarCommand(root), pw)
		if e != nil {
			pw.CloseWithError(e)
		} else {
			pw.Close()
		}
		streamed <- e
	}()
	watch := make(chan struct{})
	go func() { // the bound also unblocks admission if the stream stalls without ending
		select {
		case <-fctx.Done():
			pr.CloseWithError(fctx.Err())
		case <-watch:
		}
	}()
	err = admitMountArchive(pr, dir, mountAdmitLimits, l)
	close(watch)
	if err != nil {
		cancel() // stops the remote stream
		pr.CloseWithError(err)
	}
	select {
	case e := <-streamed:
		if err == nil && e != nil {
			err = fmt.Errorf("stream: %w", e)
		}
	case <-time.After(mountStreamGrace):
		if err == nil {
			err = errors.New("stream did not end")
		}
	}
	if err != nil {
		return fmt.Errorf("admission: %w", err)
	}
	return checkMountIdentity(l, boot, root, dir)
}
