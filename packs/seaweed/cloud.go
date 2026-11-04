package seaweed

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pingqiu/sw-test-runner/infra"
	"github.com/pingqiu/sw-test-runner/inventory"
)

// CloudConfig is controller-local configuration for a newly created HTTP VM.
// It is independent of the on-prem ApprovedRun entry and carries no GO/policy.
type CloudConfig struct {
	MasterWrites bool                 `json:"master_writes,omitempty"`
	ECStage1     bool                 `json:"ec_stage1,omitempty"`
	SSHKey       string               `json:"ssh_key"`
	HostKey      string               `json:"-"`
	HostKeys     map[string]string    `json:"-"`
	Output       string               `json:"-"`
	Files        map[string]CloudFile `json:"files"`

	ECUnderQuorum bool `json:"ec_under_quorum,omitempty"`
	// Chaos opts a ten-node run into the read-restart/freeze cases after the
	// functional phases. Absent or false leaves the run unchanged.
	Chaos bool `json:"chaos,omitempty"`
	// Replication "001" (opt-in, ten-node only) keeps two copies on different
	// volume servers; absent keeps the default 000 run unchanged.
	Replication string `json:"replication,omitempty"`
	// ChaosSet "A" (C1-C3) or "B" (C4-C6) runs stage 2 under replication 001.
	ChaosSet string `json:"chaos_set,omitempty"`
	// KVSoak runs the two-host KV soak from SoakProfile, the profile input
	// the bundle is built from, uploading its release files from SoakRelease.
	ReadCase string `json:"read_case,omitempty"`
	KVSoak   bool   `json:"kv_soak,omitempty"`
	// KVLoad runs the same two-host pipeline with the profile's client pattern.
	KVLoad      bool   `json:"kv_load,omitempty"`
	SoakProfile string `json:"-"`
	SoakRelease string `json:"-"`
	localHost   string // the host the controller itself runs on: reached locally, not over ssh
}

// node reaches a host: locally for the controller's own host, else over ssh
// with the configured pin or, failing that, the inventory's own pin.
func (c CloudConfig) node(h inventory.Host) *infra.Node {
	if h.ID == c.localHost && c.localHost != "" {
		return &infra.Node{IsNative: true}
	}
	pin := c.HostKey
	if pin == "" && h.Management.HostKey != nil {
		pin = *h.Management.HostKey
	}
	return cloudNode(h, c.SSHKey, pin)
}

type CloudFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// CheckFiles is local-only and runs before any billable create.
func (c CloudConfig) CheckFiles() error {
	if len(c.Files) != len(c.artifactNames()) {
		return errors.New("require weed, weed-volume and native.tar")
	}
	for _, name := range c.artifactNames() {
		f, ok := c.Files[name]
		if !ok {
			return errors.New("missing product artifact")
		}
		b, e := os.ReadFile(f.Path)
		if e != nil || fmt.Sprintf("%x", sha256.Sum256(b)) != f.SHA256 {
			return fmt.Errorf("artifact hash mismatch: %s", name)
		}
	}
	return nil
}

// RunCloudHTTP installs and exercises the same bounded HTTP worker as #19.
// The caller owns this fresh VM and always destroys it, even on failure.
func RunCloudHTTP(ctx context.Context, raw []byte, c CloudConfig) (bundle string, failure error) {
	if _, bounded := ctx.Deadline(); !bounded {
		return "", errors.New("cloud phase deadline required")
	}
	d, e := inventory.Decode(raw)
	if e != nil {
		return "", e
	}
	if c.ReadCase != "" {
		if c.ReadCase != "volume-restart" && c.ReadCase != "filer-restart" && c.ReadCase != "volume-freeze" || c.KVSoak || c.KVLoad {
			return "", errors.New("read_case requires one supported case without kv_soak or kv_load")
		}
	}
	if c.KVSoak || c.KVLoad || c.ReadCase != "" {
		if d.Target != "onprem" || len(d.Hosts) != 2 || len(c.Files) != 0 || c.SoakRelease == "" || c.ECStage1 || c.MasterWrites || c.Chaos || c.Replication != "" || c.ChaosSet != "" || c.KVSoak && c.KVLoad {
			return "", errors.New("kv_soak requires a two-host onprem inventory; read_case requires a two-host onprem inventory and no other phase")
		}
		return runDirectSoak(ctx, raw, c)
	}
	if d.Target != "azure" && !directTarget(d.Target) || d.Transport != "http" || d.State != "requested" || len(c.Files) != len(c.artifactNames()) {
		return "", errors.New("one Azure, onprem or docker HTTP host and three artifacts required")
	}
	if directTarget(d.Target) && len(d.Hosts) != 1 {
		return "", errors.New("a direct ssh target runs one host")
	}
	if c.MasterWrites && (!directTarget(d.Target) || len(d.Hosts) != 1 || c.ECStage1 || c.Chaos || c.Replication != "" || c.ChaosSet != "") {
		return "", errors.New("master writes requires one direct HTTP host without other phases")
	}
	if c.ECUnderQuorum && !c.ECStage1 {
		return "", errors.New("ec_under_quorum requires ec_stage1")
	}
	if c.ECStage1 && (!directTarget(d.Target) || len(d.Hosts) != 1 || c.Chaos || c.Replication != "" || c.ChaosSet != "") {
		return "", errors.New("EC stage1 requires one direct HTTP host without other phases")
	}
	if c.ECStage1 {
		if _, e = ecDirectPorts(d.Hosts[0]); e != nil {
			return "", e
		}
	}
	if c.Replication != "" && c.Replication != "001" {
		return "", errors.New("replication must be absent or 001")
	}
	if _, ok := chaos2Sets[c.ChaosSet]; c.ChaosSet != "" && (!ok || c.Replication != "001" || c.Chaos) {
		return "", errors.New("chaos_set must be A or B, with replication 001 and without stage-1 chaos")
	}
	if len(d.Hosts) != 1 {
		return runCloudCluster(ctx, *d, c)
	}
	if c.Chaos || c.Replication != "" || c.ChaosSet != "" {
		return "", errors.New("chaos and replication need the ten-node cluster")
	}
	h := d.Hosts[0]
	for _, role := range []string{"master", "filer", "volume", "s3", "client"} {
		if !contains(h.Roles, role) {
			return "", fmt.Errorf("missing role %s", role)
		}
	}
	return runCloudNode(ctx, *d, h, c, "")
}

func runCloudNode(ctx context.Context, d inventory.Document, h inventory.Host, c CloudConfig, phase string) (bundle string, failure error) {
	var e error
	meta := h
	for _, candidate := range d.Hosts {
		if contains(candidate.Roles, "master") {
			meta = candidate
		}
	}
	if h.RunRoot != cloudRunRoot(d, h) {
		return "", errors.New("unexpected cloud run root")
	}
	q := cloudQuote
	n := cloudNode(h, c.SSHKey, c.HostKey)
	if phase == "collect" {
		return "", collectCloudFailure(ctx, n, c.Output, h.RunRoot, d.RunID, q)
	}
	defer func() {
		// A direct target's cleanup already captured the service journal before
		// its teardown; a second fetch after teardown would overwrite it.
		if failure != nil && phase == "" && !directTarget(d.Target) {
			if e := collectCloudFailure(ctx, n, c.Output, h.RunRoot, d.RunID, q); e != nil {
				failure = errors.Join(failure, fmt.Errorf("save failure-log fetch: %w", e))
			}
		}
	}()
	if e = c.CheckFiles(); e != nil {
		return "", e
	}
	if e = n.ConnectContext(ctx); e != nil {
		return "", fmt.Errorf("cloud step connect: %w", e)
	}
	defer n.Close()
	sequence := 0
	run := func(name, cmd string) (string, error) {
		if phase != "" && !cloudStepEnabled(h, phase, name) {
			return "", nil
		}
		sequence++
		seconds := 120
		if c.ECStage1 || c.MasterWrites {
			seconds = 190
		}
		if c.ECUnderQuorum && strings.HasPrefix(name, "ec-s2-5-") {
			seconds = 600
		}
		if phase == "load" {
			seconds = 32*27 + 30 // Batch pacing plus completion margin; setup keeps its short bound.
			if deadline, ok := ctx.Deadline(); ok {
				seconds = min(seconds, int(time.Until(deadline).Seconds())-5)
			}
			if seconds <= 0 {
				return "", context.DeadlineExceeded
			}
		}
		return cloudStep(ctx, n, c.Output, fmt.Sprintf("cloud-step-%03d.json", sequence), name, seconds, cmd)
	}
	upload := func(name, local, remote string) error {
		if phase != "" && (phase != "prepare" || !cloudArtifact(h, name)) {
			return nil
		}
		sequence++
		return cloudUpload(n, c.Output, fmt.Sprintf("cloud-step-%03d.json", sequence), name, local, remote)
	}
	root := h.RunRoot
	if (phase == "" || phase == "prepare") && !directTarget(d.Target) { // an existing host has no cloud-init
		command := "timeout -k 5 120 sh -ec 'cloud-init status --wait --format json'"
		out, stderr, rc, runErr := n.Run(ctx, command)
		gateErr := errors.Join(runErr, qualifyCloudInit(out, rc))
		gateErr = errors.Join(gateErr,
			writeCloudStep(c.Output, "cloud-init-step.json", "cloud-init", command, out, stderr, rc, gateErr),
			os.WriteFile(filepath.Join(c.Output, "cloud-init-status.json"), []byte(out), 0600))
		if gateErr != nil {
			return "", fmt.Errorf("cloud step cloud-init command=%q: %w", command, gateErr)
		}
	}
	if phase == "before" || phase == "after" {
		url := "http://" + endpoint(h, "volume_http") + "/status"
		if contains(h.Roles, "master") {
			url = "http://" + endpoint(h, "master_http") + "/dir/status"
		}
		raw, e := run("snapshot", "curl -fsS --max-time 10 "+q(url))
		return "", errors.Join(e, os.WriteFile(filepath.Join(c.Output, "status.json"), []byte(raw), 0600))
	}
	if phase == "grow" {
		for _, v := range d.Hosts {
			if contains(v.Roles, "volume") {
				if _, e = run("grow-"+v.ID, "curl -fsS --max-time 15 "+q("http://"+endpoint(meta, "master_http")+"/vol/grow?count=1&collection=lmcache&dataNode="+endpoint(v, "volume_http")+cloudGrowReplication(c.Replication))); e != nil {
					return "", e
				}
			}
		}
		return "", nil
	}
	prepare := "sudo install -d -o " + q(h.Management.User) + " " + q(root) + "; mkdir -p " + q(root+"/worker") + " " + q(root+"/volume") + " " + q(root+"/master") + " " + q(root+"/filer")
	if c.ECStage1 {
		prepare = "sudo install -d -o " + q(h.Management.User) + " " + q(root) + "; mkdir -p " + q(root+"/worker")
	}
	if _, e = run("prepare", prepare); e != nil {
		return "", e
	}
	for _, name := range c.artifactNames() {
		if e = upload(name, c.Files[name].Path, root+"/"+name); e != nil {
			return "", e
		}
		if _, e = run("verify-"+name, "printf '%s  %s\\n' "+q(c.Files[name].SHA256)+" "+q(root+"/"+name)+" | sha256sum -c -"); e != nil {
			return "", e
		}
	}
	scratch, e := os.MkdirTemp(c.Output, "cloud-worker-")
	if e != nil {
		return "", e
	}
	defer os.RemoveAll(scratch)
	helpers := cloudHelpers(phase, c.ChaosSet)
	if c.ECStage1 {
		helpers = []string{"ec_service.py", "ec_setup.py", "ec_capture.py", "ec_chaos.py", "chaos_load.py"}
	}
	for _, name := range helpers {
		b, e := workerFiles.ReadFile("worker/" + name)
		if e != nil {
			return "", e
		}
		p := filepath.Join(scratch, name)
		if e = os.WriteFile(p, b, 0600); e != nil {
			return "", e
		}
		if e = upload(name, p, root+"/worker/"+name); e != nil {
			return "", e
		}
	}
	if !c.ECStage1 {
		native := "tar -xf native.tar; test -d seaweedkv_native; "
		if phase != "" && !contains(h.Roles, "client") {
			native = ""
		}
		if _, e = run("configure", "cd "+q(root)+"; "+native+"printf '%s\\n' '[filer.options.kvcache]' 'enabled = true' 'authority_mode = \"route-by-key\"' '[leveldb2]' 'enabled = true' "+q("dir = \""+root+"/filer\"")+" > filer.toml"); e != nil {
			return "", e
		}
	}
	direct := directTarget(d.Target) && phase == ""
	if direct {
		// An existing host outlives the run, unlike a destroyed VM. Registered
		// before any start, so a failed start, a failed step or a cancelled run
		// still collects the service logs, then stops every unit this run started
		// and requires a clean postflight, over its own connection and deadline.
		defer func() { failure = errors.Join(failure, directCleanup(c, d, h)) }()
	}
	if c.ECStage1 {
		return runDirectEC(d, h, c, run)
	}
	service := func(role, cmd string) error {
		_, e := run("start-"+role, cloudStart(d, root, role, cmd))
		return e
	}
	master := endpoint(meta, "master_http")
	commands := cloudServices(h, meta, root, phase != "", c.Replication)
	if c.MasterWrites {
		commands["filer"] = strings.Replace(commands["filer"], " filer ", " -v=1 filer ", 1)
	}
	for _, role := range []string{"master", "volume", "filer", "s3"} {
		if e = service(role, commands[role]); e != nil {
			return "", e
		}
	}
	defer func() {
		if direct {
			return // directCleanup collects the logs before its teardown
		}
		logs, collectErr := run("collect-services", "sudo journalctl --no-pager -u "+q("sw-cloud-"+d.RunID+"-*")+" -n 300")
		failure = errors.Join(failure, collectErr, os.WriteFile(filepath.Join(c.Output, "cloud-services.log"), []byte(logs), 0600))
	}()
	for _, url := range []string{"http://" + master + "/dir/status", "http://" + endpoint(h, "volume_http") + "/status", "http://" + endpoint(h, "filer_http") + "/", "http://" + endpoint(h, "s3_http") + "/"} {
		if _, e = run("ready-"+url, "until curl -fsS --max-time 3 "+q(url)+" >/dev/null; do sleep 1; done"); e != nil {
			return "", e
		}
	}
	if phase != "" && phase != "load" {
		return "", nil
	}
	// SmokeArgs only needs endpoints. This local execution view is not published
	// as an observed inventory and is not S0b admission.
	d.State = "resolved"
	if phase == "load" {
		for _, v := range d.Hosts {
			if contains(v.Roles, "volume") {
				d.Hosts = []inventory.Host{meta, v, h}
				break
			}
		}
	}
	args, e := SmokeArgs(&d, h.ID, SmokeSettings{NativeDir: root, HelperDir: root + "/worker", Keys: 16, Length: 1 << 20, Replication: c.Replication})
	if e != nil {
		return "", e
	}
	if phase == "load" {
		args[1] = root + "/worker/cloud_batch.py"
		args = append(args, "--s3", "http://"+endpoint(meta, "s3_http"), "--master", "http://"+endpoint(meta, "master_http"))
	}
	cmd := "python3"
	for _, arg := range args {
		cmd += " " + q(arg)
	}
	if c.MasterWrites {
		return runDirectMasterWrites(ctx, d, h, c, args, run)
	}
	stdout, e := run("kv-smoke", cmd)
	output := filepath.Join(c.Output, "cloud-kv.jsonl")
	if w := os.WriteFile(output, []byte(stdout), 0600); w != nil {
		return output, w
	}
	if e != nil {
		return output, e
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	var summary struct {
		Status            string
		Ack, Exact, Wrong int
		S3Ack             int `json:"s3_ack"`
		S3Exact           int `json:"s3_exact"`
	}
	want := 16
	if phase == "load" {
		want = 512
	}
	if len(lines) == 0 || json.Unmarshal([]byte(lines[len(lines)-1]), &summary) != nil || summary.Status != "PASS" || summary.Ack != want || summary.Exact != want || summary.Wrong != 0 || phase == "load" && (summary.S3Ack != 32 || summary.S3Exact != 32) {
		return output, errors.New("missing or failed KV smoke")
	}
	return output, nil
}

// cloudHelpers lists the embedded worker files a node phase uploads.
func cloudHelpers(phase, chaosSet string) []string {
	helpers := []string{"kv_smoke.py", "soak_native_worker.py", "soak_oracle.py"}
	if phase != "" {
		helpers = append(helpers, "cloud_batch.py")
	}
	if phase != "" && chaosSet != "" {
		helpers = append(helpers, "chaos_load.py")
	}
	return helpers
}

// directTarget is an existing host reached over plain ssh: no cloud lifecycle,
// no cloud-init, and the run cleans up after itself.
func directTarget(target string) bool { return target == "onprem" || target == "docker" }

// cloudRunRoot is the only run root a host may declare: the VM's data path, or
// for a direct target a directory under the ssh user's home.
func cloudRunRoot(d inventory.Document, h inventory.Host) string {
	if !directTarget(d.Target) {
		return "/data/seaweed-runs/" + d.RunID
	}
	home := "/home/" + h.Management.User
	if h.Management.User == "root" {
		home = "/root"
	}
	return home + "/sw-runs/" + d.RunID
}

// Controller-side bounds of a direct run's cleanup: the diagnostic capture,
// then the teardown (stop and postflight), each with its own connection.
var directCollectBound, directTeardownBound = 30 * time.Second, 90 * time.Second

// directCleanup collects this run's service logs, then stops its transient
// units, then checks that none remain and no declared service port listens.
// It opens its own connection under its own deadline: the run's connection
// closes when the run's context is cancelled, and a cleanup that cannot reach
// the host is a failure, never a clean result.
func directCleanup(c CloudConfig, d inventory.Document, h inventory.Host) error {
	units := cloudQuote("sw-cloud-" + d.RunID + "-*")
	// Diagnostics first, on their own connection and controller-side deadline,
	// so a stalled journal can never spend the teardown's time or connection.
	collect := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), directCollectBound)
		defer cancel()
		n := c.node(h)
		if err := n.ConnectContext(ctx); err != nil {
			return fmt.Errorf("service-log capture did not reach the host: %w", err)
		}
		defer n.Close()
		command := "sudo journalctl --no-pager -u " + units + " -n 300"
		if c.ECStage1 || c.MasterWrites {
			command += "; find " + cloudQuote(h.RunRoot) + " -maxdepth 1 -name '*.jsonl' -type f -print -exec cat {} +"
		}
		logs, err := cloudStep(ctx, n, c.Output, "cloud-collect.json", "collect-services", int(directCollectBound/time.Second), command)
		return errors.Join(err, os.WriteFile(filepath.Join(c.Output, "cloud-services.log"), []byte(logs), 0600))
	}()
	ctx, cancel := context.WithTimeout(context.Background(), directTeardownBound)
	defer cancel()
	n := c.node(h)
	if err := n.ConnectContext(ctx); err != nil {
		return errors.Join(collect, fmt.Errorf("cleanup did not reach the host: %w", err))
	}
	defer n.Close()
	_, stop := cloudStep(ctx, n, c.Output, "cloud-cleanup.json", "cleanup",
		60, "sudo -n systemctl stop "+units+"; sudo -n systemctl reset-failed "+units+" 2>/dev/null || true")
	ports := []string{}
	for _, s := range h.Services {
		ports = append(ports, strconv.Itoa(s.Port))
	}
	if c.ECStage1 {
		extra, err := ecDirectPorts(h)
		if err != nil {
			return errors.Join(collect, stop, err)
		}
		ports = extra
	}
	// A probe that cannot run is a failure, never a count of zero: an empty
	// listing from a failed systemctl, ss or grep would otherwise read as clean.
	// grep -c exits 1 for no match (count 0) and 2 for an error.
	check := "l=$(sudo -n systemctl list-units --all --no-legend " + units + ") || { echo 'postflight: systemctl failed'; exit 3; }" +
		"; u=$(printf '%s' \"$l\" | grep -c .) || [ $? -eq 1 ] || { echo 'postflight: grep failed'; exit 3; }; p=0"
	if len(ports) > 0 {
		check += "; s=$(ss -Hltn) || { echo 'postflight: ss failed'; exit 3; }" +
			"; p=$(printf '%s\\n' \"$s\" | grep -cE ':(" + strings.Join(ports, "|") + ")[[:space:]]') || [ $? -eq 1 ] || { echo 'postflight: grep failed'; exit 3; }"
	}
	check += "; echo \"units=$u ports=$p\"; [ \"$u\" = 0 ] && [ \"$p\" = 0 ]"
	out, post := cloudStep(ctx, n, c.Output, "cloud-postflight.json", "postflight", 30, check)
	if post != nil {
		post = fmt.Errorf("postflight not clean (%s): %w", strings.TrimSpace(out), post)
	}
	return errors.Join(collect, stop, post)
}

// cloudUpload copies one local file to the host and journals it as dir/file.
func cloudUpload(n *infra.Node, dir, file, name, local, remote string) error {
	command := "upload " + local + " -> " + remote
	e := n.Upload(local, remote)
	rc := 0
	if e != nil {
		rc = -1
	}
	w := writeCloudStep(dir, file, "upload-"+name, command, "", "", rc, e)
	if e != nil || w != nil {
		return fmt.Errorf("cloud step upload-%s command=%q: %v", name, command, errors.Join(e, w))
	}
	return nil
}

func cloudNode(h inventory.Host, key, pin string) *infra.Node {
	return &infra.Node{Host: net.JoinHostPort(h.Management.Address, strconv.Itoa(h.Management.Port)), User: h.Management.User, KeyFile: key, HostKey: pin}
}

// cloudStep runs one bounded remote command and journals it as dir/file.
func cloudStep(ctx context.Context, n *infra.Node, dir, file, name string, seconds int, cmd string) (string, error) {
	command := fmt.Sprintf("timeout -k 5 %d sh -ec ", seconds) + cloudQuote(cmd)
	out, stderr, rc, e := n.Run(ctx, command)
	w := writeCloudStep(dir, file, name, command, out, stderr, rc, e)
	if e != nil || rc != 0 || w != nil {
		return out, fmt.Errorf("cloud step %s command=%q rc=%d stderr=%s: %v", name, command, rc, stderr, errors.Join(e, w))
	}
	return out, nil
}

func cloudQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

// cloudStart starts one service generation as a transient unit; deploy uses the
// role's unit name, a chaos replacement the same command under unit role+"-g2".
func cloudStart(d inventory.Document, root, unit, cmd string) string {
	return "sudo systemd-run --unit=" + cloudQuote("sw-cloud-"+d.RunID+"-"+unit) + unitLifetime(d) + " --property=WorkingDirectory=" + cloudQuote(root) + " " + cmd
}

// cloudNow is the controller clock; a var so a test can pin it.
var cloudNow = time.Now

// unitLifetime gives a transient unit a relative runtime limit, the time left to
// the run's hard deadline when its start is built, so systemd on the host stops
// it by itself if the controller dies mid-run. It's a backstop, not an exact
// stop at Hard: dispatch delay and systemd's stop grace (TimeoutStopSec) can
// carry the stop past Hard, and the controller's own cleanup stays independent
// of it. Empty only for a document with no hard deadline.
func unitLifetime(d inventory.Document) string {
	hard, err := time.Parse(time.RFC3339Nano, d.Deadlines.Hard)
	if err != nil {
		return ""
	}
	left := (hard.Sub(cloudNow()) + time.Second - 1) / time.Second // whole seconds, rounded up
	return " --property=RuntimeMaxSec=" + strconv.FormatInt(int64(max(left, 1)), 10)
}

// cloudServices is the single source of each role's service command.
func cloudGrowReplication(replication string) string {
	if replication == "" {
		return ""
	}
	return "&replication=" + replication
}

func cloudServices(h, meta inventory.Host, root string, cluster bool, replication string) map[string]string {
	q, ip, master := cloudQuote, h.Data.Address, endpoint(meta, "master_http")
	if replication == "" {
		replication = "000"
	}
	volumeMB, maxVolumes := "32", "8"
	if cluster {
		volumeMB, maxVolumes = "256", "16"
	}
	return map[string]string{
		"master": q(root+"/weed") + " master -ip=" + ip + " -port=" + strconv.Itoa(servicePort(h, "master_http")) + " -mdir=" + q(root+"/master") + " -volumeSizeLimitMB=" + volumeMB + " -defaultReplication=" + replication + " -peers=none",
		"volume": q(root+"/weed-volume") + " --master=" + master + " --ip=" + ip + " --port=" + strconv.Itoa(servicePort(h, "volume_http")) + " --port.grpc=" + strconv.Itoa(servicePort(h, "volume_grpc")) + " --dir=" + q(root+"/volume") + " --max=" + maxVolumes,
		"filer":  q(root+"/weed") + " -config_dir=" + q(root) + " filer -master=" + master + " -ip=" + ip + " -port=" + strconv.Itoa(servicePort(h, "filer_http")) + " -port.grpc=" + strconv.Itoa(servicePort(h, "filer_grpc")),
		"s3":     q(root+"/weed") + " s3 -filer=" + endpoint(meta, "filer_http") + " -port=" + strconv.Itoa(servicePort(h, "s3_http")),
	}
}

func servicePort(h inventory.Host, name string) int {
	for _, s := range h.Services {
		if s.Name == name {
			return s.Port
		}
	}
	return 0
}

func (c CloudConfig) artifactNames() []string {
	if c.ECStage1 {
		return []string{"weed"}
	}
	return []string{"weed", "weed-volume", "native.tar"}
}
