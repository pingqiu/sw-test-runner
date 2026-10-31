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

	"github.com/pingqiu/sw-test-runner/infra"
	"github.com/pingqiu/sw-test-runner/inventory"
)

// CloudConfig is controller-local configuration for a newly created HTTP VM.
// It is independent of the on-prem ApprovedRun entry and carries no GO/policy.
type CloudConfig struct {
	SSHKey  string               `json:"ssh_key"`
	HostKey string               `json:"-"`
	Output  string               `json:"-"`
	Files   map[string]CloudFile `json:"files"`
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
	if d.Target != "azure" || d.Transport != "http" || d.State != "requested" || len(d.Hosts) != 1 || len(c.Files) != 3 {
		return "", errors.New("one newly created Azure HTTP host and three artifacts required")
	}
	h := d.Hosts[0]
	for _, role := range []string{"master", "filer", "volume", "s3", "client"} {
		if !contains(h.Roles, role) {
			return "", fmt.Errorf("missing role %s", role)
		}
	}
	if h.RunRoot != "/data/seaweed-runs/"+d.RunID {
		return "", errors.New("unexpected cloud run root")
	}
	q := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
	n := &infra.Node{Host: net.JoinHostPort(h.Management.Address, strconv.Itoa(h.Management.Port)), User: h.Management.User, KeyFile: c.SSHKey, HostKey: c.HostKey}
	defer func() {
		if failure != nil {
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
		sequence++
		command := "timeout -k 5 120 sh -ec " + q(cmd)
		out, stderr, rc, e := n.Run(ctx, command)
		w := writeCloudStep(c.Output, fmt.Sprintf("cloud-step-%03d.json", sequence), name, command, out, stderr, rc, e)
		if e != nil || rc != 0 || w != nil {
			return out, fmt.Errorf("cloud step %s command=%q rc=%d stderr=%s: %v", name, command, rc, stderr, errors.Join(e, w))
		}
		return out, nil
	}
	upload := func(name, local, remote string) error {
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
	if _, e = run("prepare", "cloud-init status --wait; sudo install -d -o "+q(h.Management.User)+" "+q(root)+"; mkdir -p "+q(root+"/worker")+" "+q(root+"/volume")+" "+q(root+"/master")+" "+q(root+"/filer")); e != nil {
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
	for _, name := range []string{"kv_smoke.py", "soak_native_worker.py", "soak_oracle.py"} {
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
	if _, e = run("configure", "cd "+q(root)+"; tar -xf native.tar; test -d seaweedkv_native; printf '%s\\n' '[filer.options.kvcache]' 'enabled = true' 'authority_mode = \"route-by-key\"' '[leveldb2]' 'enabled = true' "+q("dir = \""+root+"/filer\"")+" > filer.toml"); e != nil {
		return "", e
	}
	ip := h.Data.Address
	service := func(role, cmd string) error {
		_, e := run("start-"+role, "sudo systemd-run --unit="+q("sw-cloud-"+d.RunID+"-"+role)+" --property=WorkingDirectory="+q(root)+" "+cmd)
		return e
	}
	master := endpoint(h, "master_http")
	for _, s := range []struct{ role, cmd string }{
		{"master", q(root+"/weed") + " master -ip=" + ip + " -port=" + strconv.Itoa(servicePort(h, "master_http")) + " -mdir=" + q(root+"/master") + " -volumeSizeLimitMB=32 -defaultReplication=000 -peers=none"},
		{"volume", q(root+"/weed-volume") + " --master=" + master + " --ip=" + ip + " --port=" + strconv.Itoa(servicePort(h, "volume_http")) + " --port.grpc=" + strconv.Itoa(servicePort(h, "volume_grpc")) + " --dir=" + q(root+"/volume") + " --max=8"},
		{"filer", q(root+"/weed") + " -config_dir=" + q(root) + " filer -master=" + master + " -ip=" + ip + " -port=" + strconv.Itoa(servicePort(h, "filer_http")) + " -port.grpc=" + strconv.Itoa(servicePort(h, "filer_grpc"))},
		{"s3", q(root+"/weed") + " s3 -filer=" + endpoint(h, "filer_http") + " -port=" + strconv.Itoa(servicePort(h, "s3_http"))},
	} {
		if e = service(s.role, s.cmd); e != nil {
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
	// SmokeArgs only needs endpoints. This local execution view is not published
	// as an observed inventory and is not S0b admission.
	d.State = "resolved"
	args, e := SmokeArgs(d, h.ID, SmokeSettings{NativeDir: root, HelperDir: root + "/worker", Keys: 16, Length: 1 << 20})
	if e != nil {
		return "", e
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
	}
	if len(lines) == 0 || json.Unmarshal([]byte(lines[len(lines)-1]), &summary) != nil || summary.Status != "PASS" || summary.Ack != 16 || summary.Exact != 16 || summary.Wrong != 0 {
		return output, errors.New("missing or failed KV smoke")
	}
	return output, nil
}

func servicePort(h inventory.Host, name string) int {
	for _, s := range h.Services {
		if s.Name == name {
			return s.Port
		}
	}
	return 0
}
