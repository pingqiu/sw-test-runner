package seaweed

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/pingqiu/sw-test-runner/inventory"
	"golang.org/x/crypto/ssh"
	"gopkg.in/yaml.v3"
)

// SWCtlConfig selects only the existing KV smoke; it adds no workload or oracle.
// Paths are controller-local. Secrets are resolved solely by the pinned CLI.
type SWCtlConfig struct {
	Binary       CloudFile `json:"binary"`
	Cluster      string    `json:"cluster"`
	ArtifactsDir string    `json:"artifacts_dir"`
}
type swctlReply struct {
	Schema, Command, Cluster, Status string
	Partial                          bool
	Items                            []struct{ ID, Host, Status, Code string }
}
type swctlCluster struct {
	Name    string `yaml:"name"`
	User    string `yaml:"service_user"`
	Release struct {
		SHA string `yaml:"manifest_sha256"`
	} `yaml:"release"`
	SSH struct {
		User     string `yaml:"user"`
		Identity string `yaml:"identity_ref"`
	} `yaml:"ssh"`
	Hosts []struct {
		Name, Address string
		HostKey       string `yaml:"host_key_sha256"`
		Roles         []string
		RDMA          struct {
			Address string `yaml:"data_address"`
			Device  string
			GID     int `yaml:"gid_index"`
		}
		Data []string `yaml:"data_paths"`
	} `yaml:"hosts"`
	KV struct {
		Filer struct {
			Port int
			GRPC int `yaml:"grpc_port"`
		}
		Volume struct {
			Port int
			GRPC int `yaml:"grpc_port"`
			RDMA struct {
				Port int
				Push struct {
					Port    int
					Control int `yaml:"control_port"`
				}
				DC struct {
					Control int `yaml:"push_control_port"`
				}
			}
		}
		LMCache struct {
			Adapter struct {
				Filer   string `yaml:"filer_grpc"`
				Workers int    `yaml:"num_workers"`
				Pipes   int
				Range   *bool `yaml:"push_source_range_mr"`
			} `yaml:"adapter_params"`
			DC struct {
				Enabled bool
				Address string `yaml:"control_addr"`
				Device  string
				Port    *int
				GID     *int `yaml:"gid_index"`
			} `yaml:"client_dc"`
		} `yaml:"lmcache"`
		Rotation struct{ Enabled bool }
		Security map[string]any
	} `yaml:"kv"`
}

func (s SWCtlConfig) call(ctx context.Context, c CloudConfig, action string) (swctlReply, int, error) {
	var result swctlReply
	file, e := os.Open(s.Binary.Path)
	if e != nil {
		return result, -1, errors.New("swctl binary unreadable")
	}
	info, e := file.Stat()
	hash := sha256.New()
	n, readErr := io.Copy(hash, io.LimitReader(file, 256<<20+1))
	file.Close()
	if e != nil || !info.Mode().IsRegular() || readErr != nil || n > 256<<20 || fmt.Sprintf("%x", hash.Sum(nil)) != s.Binary.SHA256 {
		return result, -1, errors.New("swctl binary pin mismatch")
	}
	args := strings.Fields(action)
	args = append(args, "--cluster", s.Cluster, "--json")
	if !strings.HasPrefix(action, "plan ") {
		args = append(args, "--state-dir", filepath.Join(c.Output, "swctl-state"))
	}
	if action == "install" {
		args = append(args, "--artifacts-dir", s.ArtifactsDir)
	}
	cmd := exec.CommandContext(ctx, s.Binary.Path, args...)
	cmd.Stderr = io.Discard
	pipe, e := cmd.StdoutPipe()
	if e != nil {
		return result, -1, e
	}
	if e = cmd.Start(); e != nil {
		return result, -1, e
	}
	raw, e := io.ReadAll(io.LimitReader(pipe, 1<<20+1))
	if len(raw) > 1<<20 {
		cmd.Process.Kill()
		cmd.Wait()
		return result, -1, errors.New("swctl result cap exceeded")
	}
	wait := cmd.Wait()
	code := 0
	if wait != nil {
		code = -1
		var exit *exec.ExitError
		if errors.As(wait, &exit) {
			code = exit.ExitCode()
		}
	}
	if e != nil {
		return result, code, e
	}
	if e = os.WriteFile(filepath.Join(c.Output, "swctl-"+strings.ReplaceAll(action, " ", "-")+".json"), raw, 0600); e != nil {
		return result, code, e
	}
	if json.Unmarshal(raw, &result) != nil || result.Schema != "swctl.result/v1" || result.Command != action {
		return result, code, errors.New("invalid swctl envelope")
	}
	if ctx.Err() != nil {
		return result, code, ctx.Err()
	}
	return result, code, nil
}
func bindSWCtl(d inventory.Document, c CloudConfig) (swctlCluster, inventory.Host, error) {
	var profile swctlCluster
	var client inventory.Host
	refuse := func() (swctlCluster, inventory.Host, error) {
		return profile, client, errors.New("swctl KV smoke profile/inventory mismatch or unsupported smoke rendering")
	}
	raw, e := os.ReadFile(c.SWCtl.Cluster)
	if e != nil || len(raw) > 256<<10 || yaml.Unmarshal(raw, &profile) != nil {
		return refuse()
	}
	if d.Target != "onprem" || d.State != "requested" || (d.Transport != "rc" && d.Transport != "dc") || len(c.Files) != 0 || c.KVSoak || c.KVLoad || c.ReadCase != "" || c.ECStage1 || c.ECUnderQuorum || c.ECReconstruction || c.ECVolumeImpl != "" || c.MasterWrites || c.Chaos || c.ChaosSet != "" || c.Replication != "" || len(c.ClusterChaosCases) != 0 {
		return refuse()
	}
	if profile.Name == "" || profile.User == "" || profile.Release.SHA != d.ArtifactManifestSHA256 || profile.SSH.Identity != "controller-file:"+c.SSHKey || len(profile.Hosts) != len(d.Hosts) {
		return refuse()
	}
	adapter := profile.KV.LMCache.Adapter
	if adapter.Workers != 4 || adapter.Pipes != 8 || adapter.Range == nil || !*adapter.Range {
		return refuse()
	}
	// The old smoke has no credential/TLS channel; refuse rather than silently
	// ignore one or modify its payload, operations, metadata read or oracle.
	for _, name := range []string{"filer_jwt_signing_key_ref", "filer_read_jwt_signing_key_ref", "grpc_tls"} {
		if v := profile.KV.Security[name]; v != nil {
			if name != "grpc_tls" {
				return refuse()
			}
			values, ok := v.(map[string]any)
			if !ok {
				return refuse()
			}
			for _, value := range values {
				if value != nil && value != "" {
					return refuse()
				}
			}
		}
	}
	if profile.KV.LMCache.DC.Enabled != (d.Transport == "dc") {
		return refuse()
	}
	for _, h := range d.Hosts {
		found := false
		for _, p := range profile.Hosts {
			if p.Name != h.ID {
				continue
			}
			found = true
			pin := c.HostKey
			if pin == "" && h.Management.HostKey != nil {
				pin = *h.Management.HostKey
			}
			key, _, _, _, e := ssh.ParseAuthorizedKey([]byte(pin))
			if e != nil || ssh.FingerprintSHA256(key) != strings.TrimRight(p.HostKey, "=") || p.Address != h.Management.Address || h.Management.Port != 22 || profile.SSH.User != h.Management.User || p.RDMA.Address != h.Data.Address {
				return refuse()
			}
			roles := map[string]bool{}
			for _, role := range p.Roles {
				if role == "lmcache" {
					role = "client"
				}
				roles[role] = true
			}
			if len(roles) != len(h.Roles) {
				return refuse()
			}
			for _, role := range h.Roles {
				if !roles[role] {
					return refuse()
				}
			}
			if contains(h.Roles, "client") {
				if client.ID != "" {
					return refuse()
				}
				client = h
				if d.Transport == "dc" && (h.RDMA.GIDIndex == nil || *h.RDMA.GIDIndex != swctlInt(profile.KV.LMCache.DC.GID, 3) || h.RDMA.Device != profile.KV.LMCache.DC.Device || h.RDMA.Port != swctlInt(profile.KV.LMCache.DC.Port, 1)) {
					return refuse()
				}
			}
			if contains(h.Roles, "filer") {
				grpc := profile.KV.Filer.GRPC
				if grpc == 0 {
					grpc = profile.KV.Filer.Port + 10000
				}
				if endpoint(h, "filer_http") != fmt.Sprintf("%s:%d", p.RDMA.Address, profile.KV.Filer.Port) || endpoint(h, "filer_grpc") != fmt.Sprintf("%s:%d", p.RDMA.Address, grpc) || adapter.Filer != endpoint(h, "filer_grpc") {
					return refuse()
				}
			}
			if contains(h.Roles, "volume") {
				if endpoint(h, "rdma") != fmt.Sprintf("%s:%d", p.RDMA.Address, profile.KV.Volume.RDMA.Port) || endpoint(h, "rdma_control") != fmt.Sprintf("%s:%d", p.RDMA.Address, profile.KV.Volume.RDMA.Push.Control) {
					return refuse()
				}
				if d.Transport == "dc" && (endpoint(h, "dc_control") != fmt.Sprintf("%s:%d", p.RDMA.Address, profile.KV.Volume.RDMA.DC.Control) || endpoint(h, "dc_control") != profile.KV.LMCache.DC.Address) {
					return refuse()
				}
			}
		}
		if !found {
			return refuse()
		}
	}
	if client.ID == "" {
		return refuse()
	}
	return profile, client, nil
}
func allowedSmokeDoctor(r swctlReply, d inventory.Document, p swctlCluster) bool {
	if len(r.Items) == 0 || r.Partial || (r.Status != "PASS" && r.Status != "UNKNOWN" && r.Status != "NOT_PROVEN" && r.Status != "WARN") {
		return false
	}
	counts := map[string]map[string]int{}
	for _, h := range d.Hosts {
		counts[h.ID] = map[string]int{}
	}
	for _, i := range r.Items {
		if counts[i.Host] == nil {
			return false
		}
		counts[i.Host][i.ID]++
	}
	ids := []string{"rdma.device", "rdma.port_state", "rdma.gid", "push.listener", "dc.ready", "dc.route", "kv.budget_refusals", "rotation.headroom", "rotation.activity", "native.stats", "lmcache.shape", "version.pins"}
	for _, h := range d.Hosts {
		if len(counts[h.ID]) != 13 || counts[h.ID]["cfg.nonpublic"] != len(h.Roles) {
			return false
		}
		for _, id := range ids {
			if counts[h.ID][id] != 1 {
				return false
			}
		}
	}
	hosts := map[string]inventory.Host{}
	for _, h := range d.Hosts {
		hosts[h.ID] = h
	}
	for _, i := range r.Items {
		h := hosts[i.Host]
		applicable := true
		switch i.ID {
		case "push.listener":
			applicable = contains(h.Roles, "volume") && p.KV.Volume.RDMA.Push.Port > 0
		case "dc.ready":
			applicable = contains(h.Roles, "volume") && p.KV.Volume.RDMA.DC.Control > 0
		case "dc.route":
			applicable = contains(h.Roles, "volume") && p.KV.LMCache.DC.Enabled
		case "kv.budget_refusals":
			applicable = contains(h.Roles, "volume")
		case "rotation.headroom":
			applicable = contains(h.Roles, "volume") && p.KV.Rotation.Enabled
		case "rotation.activity":
			applicable = contains(h.Roles, "master") && p.KV.Rotation.Enabled
		case "native.stats", "lmcache.shape":
			applicable = contains(h.Roles, "client")
		}
		if !applicable {
			if i.Status != "NOT_APPLICABLE" || i.Code != "MODE_OFF" {
				return false
			}
			continue
		}
		if i.Status == "PASS" {
			continue
		}
		if i.ID == "native.stats" && i.Status == "UNKNOWN" && i.Code == "NATIVE_STATS_ABSENT" {
			continue
		}
		if i.ID == "rotation.activity" && i.Status == "NOT_PROVEN" && i.Code == "ROTATION_NOT_OBSERVED" {
			continue
		}
		if (i.ID == "dc.route" || i.ID == "kv.budget_refusals") && i.Status == "NOT_PROVEN" && i.Code == "KV_COUNTER_NO_TRAFFIC" {
			continue
		}
		return false
	}
	return true
}
func runSWCtlSmoke(ctx context.Context, d inventory.Document, c CloudConfig) (rows string, failure error) {
	s := *c.SWCtl
	original, e := os.Open(s.Cluster)
	if e != nil {
		return "", errors.New("swctl cluster unreadable")
	}
	raw, e := io.ReadAll(io.LimitReader(original, 256<<10+1))
	original.Close()
	if e != nil || len(raw) > 256<<10 {
		return "", errors.New("swctl cluster cap exceeded")
	}
	private, e := os.MkdirTemp("", "swctl-input-")
	if e != nil {
		return "", e
	}
	defer os.RemoveAll(private)
	s.Cluster = filepath.Join(private, "cluster.yaml")
	if e = os.WriteFile(s.Cluster, raw, 0600); e != nil {
		return "", e
	}
	plan, code, e := s.call(ctx, c, "plan install")
	if e != nil || code != 0 || plan.Status != "PASS" {
		return "", errors.Join(e, errors.New("swctl static install plan refused"))
	}
	s.Cluster = filepath.Join(c.Output, "swctl-cluster.yaml")
	if e = os.WriteFile(s.Cluster, raw, 0600); e != nil {
		return "", e
	}
	c.SWCtl = &s
	profile, client, e := bindSWCtl(d, c)
	if e != nil {
		return "", e
	}
	if plan.Cluster != profile.Name {
		return "", errors.New("swctl plan cluster mismatch")
	}
	helpers := filepath.ToSlash(filepath.Join(client.RunRoot, "swctl-smoke-helpers"))
	q := cloudQuote
	release := "/opt/swctl/" + profile.Name + "/releases/" + profile.Release.SHA
	view := d
	view.State = "resolved" // endpoint view only, not an observed inventory.
	args, e := SmokeArgs(&view, client.ID, SmokeSettings{NativeDir: release + "/venv", HelperDir: helpers, Keys: 16, Length: 1 << 20})
	if e != nil {
		return "", e
	}
	installed, code, e := s.call(ctx, c, "install")
	if e != nil || code != 0 || installed.Status != "PASS" || installed.Partial || installed.Cluster != profile.Name {
		// A partial persistent installation needs exact CLI reconciliation. Never
		// uninstall a predecessor merely because this attempt was refused.
		return "", errors.Join(e, fmt.Errorf("swctl install exit %d; partial=%t; retain journal for exact resume", code, installed.Partial))
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		r, exit, e := s.call(cleanup, c, "uninstall")
		if e != nil || exit != 0 || r.Status != "PASS" || r.Partial || r.Cluster != profile.Name {
			failure = errors.Join(failure, e, errors.New("swctl uninstall unconfirmed; retain state"))
			return
		}
		failure = errors.Join(failure, swctlPostflight(cleanup, d, c, profile.Name))
	}()
	n := swctlNodeFor(c, client)
	if e = n.ConnectContext(ctx); e != nil {
		return "", e
	}
	defer n.Close()
	_, e = cloudStep(ctx, n, c.Output, "swctl-helper-prepare.json", "prepare-smoke-helpers", 20, "umask 077; mkdir -p "+q(client.RunRoot)+" && mkdir "+q(helpers))
	if e != nil {
		return "", e
	}
	defer func() {
		clean, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		node := swctlNodeFor(c, client)
		if e := node.ConnectContext(clean); e != nil {
			failure = errors.Join(failure, e)
			return
		}
		defer node.Close()
		command := "rm -f --"
		for _, name := range cloudHelpers("", "") {
			command += " " + q(helpers+"/"+name)
		}
		command += " && rmdir -- " + q(helpers)
		_, e := cloudStep(clean, node, c.Output, "swctl-helper-cleanup.json", "remove-smoke-helpers", 20, command)
		failure = errors.Join(failure, e)
	}()
	scratch, e := os.MkdirTemp(c.Output, "swctl-helper-")
	if e != nil {
		return "", e
	}
	defer os.RemoveAll(scratch)
	for _, name := range cloudHelpers("", "") {
		raw, e := workerFiles.ReadFile("worker/" + name)
		if e != nil {
			return "", e
		}
		file := filepath.Join(scratch, name)
		if e = os.WriteFile(file, raw, 0600); e != nil {
			return "", e
		}
		if e = cloudUpload(n, c.Output, "swctl-upload-"+name+".json", name, file, helpers+"/"+name); e != nil {
			return "", e
		}
	}
	doctor, exit, e := s.call(ctx, c, "doctor")
	if e != nil || exit != 0 && exit != 2 && exit != 3 || doctor.Cluster != profile.Name || !allowedSmokeDoctor(doctor, d, profile) {
		return "", errors.Join(e, errors.New("unexpected swctl doctor row"))
	}
	command := "LD_LIBRARY_PATH=" + q(release+"/venv/lib/swctl-native") + " " + q(release+"/venv/bin/python")
	for _, arg := range args {
		command += " " + q(arg)
	}
	stdout, e := cloudStep(ctx, n, c.Output, "swctl-kv-smoke.json", "kv-smoke", 120, command)
	rows = filepath.Join(c.Output, "cloud-kv.jsonl")
	failure = errors.Join(e, os.WriteFile(rows, []byte(stdout), 0600), qualifyCloudSmoke(stdout, ""))
	if failure != nil {
		return rows, failure
	}
	probe, exit, e := s.call(ctx, c, "probe")
	if e != nil || exit != 0 || !swctlProbePassed(probe, profile.Name, client.ID) {
		return rows, errors.Join(e, errors.New("swctl native probe did not pass"))
	}
	return rows, nil
}
func swctlPostflight(ctx context.Context, d inventory.Document, c CloudConfig, cluster string) error {
	var failure error
	for _, h := range d.Hosts {
		n := swctlNodeFor(c, h)
		if e := n.ConnectContext(ctx); e != nil {
			failure = errors.Join(failure, e)
			continue
		}
		command := "units=$(sudo -n systemctl list-units --all --no-legend " + cloudQuote("swctl-"+cluster+"-*.service") + ") || exit 3; test -z \"$units\" || exit 1; sockets=$(ss -Hltn) || exit 3; "
		ports := []string{}
		for _, s := range h.Services {
			ports = append(ports, fmt.Sprint(s.Port))
		}
		if len(ports) > 0 {
			command += "printf '%s\\n' \"$sockets\" | grep -E ':(" + strings.Join(ports, "|") + ")[[:space:]]'; rc=$?; test \"$rc\" = 1 || exit 1; "
		}
		command += "echo 'owned units=0 ports=0'"
		_, e := cloudStep(ctx, n, c.Output, "swctl-postflight-"+h.ID+".json", "postflight-"+h.ID, 30, command)
		failure = errors.Join(failure, e)
		n.Close()
	}
	return failure
}

func swctlProbePassed(r swctlReply, cluster, client string) bool {
	if r.Status != "PASS" || r.Cluster != cluster || r.Partial {
		return false
	}
	found := 0
	for _, i := range r.Items {
		if i.Status != "PASS" {
			return false
		}
		if i.ID == "probe.native" && i.Host == client && i.Code == "PROBE_EXACT" {
			found++
		}
	}
	return found == 1
}

type swctlSmokeNode interface {
	ConnectContext(context.Context) error
	Run(context.Context, string) (string, string, int, error)
	Upload(string, string) error
	Close()
}

func swctlNodeFor(c CloudConfig, h inventory.Host) swctlSmokeNode {
	if c.swctlNodes != nil {
		return c.swctlNodes(h)
	}
	return c.node(h)
}

func swctlInt(value *int, fallback int) int {
	if value == nil {
		return fallback
	}
	return *value
}
