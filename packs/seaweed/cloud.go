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
	SSHKey   string               `json:"ssh_key"`
	HostKey  string               `json:"-"`
	HostKeys map[string]string    `json:"-"`
	Output   string               `json:"-"`
	Files    map[string]CloudFile `json:"files"`
	// Chaos opts a ten-node run into the read-restart/freeze cases after the
	// functional phases. Absent or false leaves the run unchanged.
	Chaos bool `json:"chaos,omitempty"`
}
type CloudFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// CheckFiles is local-only and runs before any billable create.
func (c CloudConfig) CheckFiles() error {
	if len(c.Files) != 3 {
		return errors.New("require weed, weed-volume and native.tar")
	}
	for _, name := range []string{"weed", "weed-volume", "native.tar"} {
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
	if d.Target != "azure" || d.Transport != "http" || d.State != "requested" || len(c.Files) != 3 {
		return "", errors.New("one newly created Azure HTTP host and three artifacts required")
	}
	if len(d.Hosts) != 1 {
		return runCloudCluster(ctx, *d, c)
	}
	if c.Chaos {
		return "", errors.New("chaos needs the ten-node cluster")
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
	if h.RunRoot != "/data/seaweed-runs/"+d.RunID {
		return "", errors.New("unexpected cloud run root")
	}
	q := cloudQuote
	n := &infra.Node{Host: net.JoinHostPort(h.Management.Address, strconv.Itoa(h.Management.Port)), User: h.Management.User, KeyFile: c.SSHKey, HostKey: c.HostKey}
	if phase == "collect" {
		return "", collectCloudFailure(ctx, n, c.Output, h.RunRoot, d.RunID, q)
	}
	defer func() {
		if failure != nil && phase == "" {
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
		if phase == "load" {
			seconds = 32*27 + 30 // Batch pacing plus completion margin; setup keeps its short bound.
			if deadline, ok := ctx.Deadline(); ok {
				seconds = min(seconds, int(time.Until(deadline).Seconds())-5)
			}
			if seconds <= 0 {
				return "", context.DeadlineExceeded
			}
		}
		command := fmt.Sprintf("timeout -k 5 %d sh -ec ", seconds) + q(cmd)
		out, stderr, rc, e := n.Run(ctx, command)
		w := writeCloudStep(c.Output, fmt.Sprintf("cloud-step-%03d.json", sequence), name, command, out, stderr, rc, e)
		if e != nil || rc != 0 || w != nil {
			return out, fmt.Errorf("cloud step %s command=%q rc=%d stderr=%s: %v", name, command, rc, stderr, errors.Join(e, w))
		}
		return out, nil
	}
	upload := func(name, local, remote string) error {
		if phase != "" && (phase != "prepare" || !cloudArtifact(h, name)) {
			return nil
		}
		sequence++
		command := "upload " + local + " -> " + remote
		e := n.Upload(local, remote)
		rc := 0
		if e != nil {
			rc = -1
		}
		w := writeCloudStep(c.Output, fmt.Sprintf("cloud-step-%03d.json", sequence), "upload-"+name, command, "", "", rc, e)
		if e != nil || w != nil {
			return fmt.Errorf("cloud step upload-%s command=%q: %v", name, command, errors.Join(e, w))
		}
		return nil
	}
	root := h.RunRoot
	if phase == "" || phase == "prepare" {
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
				if _, e = run("grow-"+v.ID, "curl -fsS --max-time 15 "+q("http://"+endpoint(meta, "master_http")+"/vol/grow?count=1&collection=lmcache&dataNode="+endpoint(v, "volume_http"))); e != nil {
					return "", e
				}
			}
		}
		return "", nil
	}
	if _, e = run("prepare", "sudo install -d -o "+q(h.Management.User)+" "+q(root)+"; mkdir -p "+q(root+"/worker")+" "+q(root+"/volume")+" "+q(root+"/master")+" "+q(root+"/filer")); e != nil {
		return "", e
	}
	for _, name := range []string{"weed", "weed-volume", "native.tar"} {
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
	helpers := []string{"kv_smoke.py", "soak_native_worker.py", "soak_oracle.py"}
	if phase != "" {
		helpers = append(helpers, "cloud_batch.py")
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
	native := "tar -xf native.tar; test -d seaweedkv_native; "
	if phase != "" && !contains(h.Roles, "client") {
		native = ""
	}
	if _, e = run("configure", "cd "+q(root)+"; "+native+"printf '%s\\n' '[filer.options.kvcache]' 'enabled = true' 'authority_mode = \"route-by-key\"' '[leveldb2]' 'enabled = true' "+q("dir = \""+root+"/filer\"")+" > filer.toml"); e != nil {
		return "", e
	}
	service := func(role, cmd string) error {
		_, e := run("start-"+role, cloudStart(d, root, role, cmd))
		return e
	}
	master := endpoint(meta, "master_http")
	commands := cloudServices(h, meta, root, phase != "")
	for _, role := range []string{"master", "volume", "filer", "s3"} {
		if e = service(role, commands[role]); e != nil {
			return "", e
		}
	}
	defer func() {
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
	args, e := SmokeArgs(&d, h.ID, SmokeSettings{NativeDir: root, HelperDir: root + "/worker", Keys: 16, Length: 1 << 20})
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

func cloudQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

// cloudStart starts one service generation as a transient unit; deploy uses the
// role's unit name, a chaos replacement the same command under unit role+"-g2".
func cloudStart(d inventory.Document, root, unit, cmd string) string {
	return "sudo systemd-run --unit=" + cloudQuote("sw-cloud-"+d.RunID+"-"+unit) + " --property=WorkingDirectory=" + cloudQuote(root) + " " + cmd
}

// cloudServices is the single source of each role's service command.
func cloudServices(h, meta inventory.Host, root string, cluster bool) map[string]string {
	q, ip, master := cloudQuote, h.Data.Address, endpoint(meta, "master_http")
	volumeMB, maxVolumes := "32", "8"
	if cluster {
		volumeMB, maxVolumes = "256", "16"
	}
	return map[string]string{
		"master": q(root+"/weed") + " master -ip=" + ip + " -port=" + strconv.Itoa(servicePort(h, "master_http")) + " -mdir=" + q(root+"/master") + " -volumeSizeLimitMB=" + volumeMB + " -defaultReplication=000 -peers=none",
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
