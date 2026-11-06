package seaweed

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/pingqiu/sw-test-runner/inventory"
	"golang.org/x/crypto/ssh"
)

type swctlAdapterHost struct {
	commands     []string
	uploads      map[string][]byte
	wrong, dirty bool
	connections  int
}

func (h *swctlAdapterHost) ConnectContext(context.Context) error { h.connections++; return nil }
func (h *swctlAdapterHost) Close()                               {}
func (h *swctlAdapterHost) Upload(local, remote string) error {
	raw, e := os.ReadFile(local)
	if e == nil {
		h.uploads[filepath.Base(remote)] = raw
	}
	return e
}
func (h *swctlAdapterHost) Run(ctx context.Context, command string) (string, string, int, error) {
	h.commands = append(h.commands, command)
	if strings.Contains(command, "kv_smoke.py") && strings.Contains(command, "--config") {
		if h.wrong {
			return `{"status":"PASS","ack":16,"exact":15,"wrong":1}`, "", 0, nil
		}
		return `{"status":"PASS","ack":16,"exact":16,"wrong":0}`, "", 0, nil
	}
	if strings.Contains(command, "systemctl list-units") && h.dirty {
		return "owned units=1 ports=0", "", 1, nil
	}
	return "", "", 0, nil
}

// The CLI is a pinned executable stand-in, and only host I/O is faked. The
// public RunCloudHTTP path still selects, binds, stages and judges the smoke.
func swctlAdapterFixture(t *testing.T, mode string) ([]byte, CloudConfig, *swctlAdapterHost, string) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("Linux CLI fixture")
	}
	var d inventory.Document
	raw, e := os.ReadFile("../../inventory/testdata/http.requested.json")
	if e != nil {
		t.Fatal(e)
	}
	json.Unmarshal(raw, &d)
	public, _, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	key, e := ssh.NewPublicKey(public)
	if e != nil {
		t.Fatal(e)
	}
	pin := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
	gid := 3
	no := false
	d.Target = "onprem"
	d.State = "requested"
	d.Transport = "dc"
	d.ManagedResourceIDs = []string{}
	d.Observation = nil
	d.RunID = "swctl-m2-fixture"
	d.ArtifactManifestSHA256 = strings.Repeat("a", 64)
	h := inventory.Host{ID: "fixture", Roles: []string{"master", "filer", "volume", "client"}, Management: inventory.Management{Address: "127.0.0.1", Port: 22, User: "testdev", CredentialRef: "fixture", HostKey: &pin}, Data: inventory.DataNetwork{Address: "10.0.2.1", NIC: "eth0", MTU: 1500}, RunRoot: filepath.ToSlash(t.TempDir()), Disks: []inventory.Disk{{ID: "data", Mount: "/data/nvme", FormatAllowed: &no}}, RDMA: inventory.RDMA{Status: "requested", Device: "mlx5_0", Port: 1, GIDIndex: &gid, Capabilities: []string{"dc"}}, Services: []inventory.Service{{Name: "master_http", Port: 9333}, {Name: "master_grpc", Port: 19333}, {Name: "filer_http", Port: 8888}, {Name: "filer_grpc", Port: 18888}, {Name: "volume_http", Port: 8080}, {Name: "volume_grpc", Port: 18080}, {Name: "rdma", Port: 18516}, {Name: "rdma_control", Port: 18517}, {Name: "dc_control", Port: 18520}}}
	d.Hosts = []inventory.Host{h}
	raw, _ = json.Marshal(d)
	if _, e = inventory.Decode(raw); e != nil {
		t.Fatal(e)
	}
	root := t.TempDir()
	cluster := filepath.Join(root, "cluster.yaml")
	binary := filepath.Join(root, "swctl")
	calls := filepath.Join(root, "calls.jsonl")
	identity := filepath.Join(root, "key-ref-not-read")
	profile := fmt.Sprintf("name: fixture-cluster\nservice_user: testdev\nrelease: {manifest_sha256: '%s'}\nssh: {user: testdev, identity_ref: 'controller-file:%s'}\nhosts:\n- name: fixture\n  address: 127.0.0.1\n  host_key_sha256: %s\n  roles: [master, filer, volume, lmcache]\n  rdma: {data_address: 10.0.2.1, device: mlx5_0, gid_index: 3}\nkv:\n  filer: {port: 8888, grpc_port: 18888}\n  volume: {rdma: {port: 18516, push: {port: 18519, control_port: 18517}, dc: {push_control_port: 18520}}}\n  security: {}\n  lmcache:\n    adapter_params: {filer_grpc: '10.0.2.1:18888', num_workers: 4, pipes: 8, source_range_mr: true}\n    client_dc: {enabled: true, control_addr: '10.0.2.1:18520', device: mlx5_0, port: 1, gid_index: 3}\n", d.ArtifactManifestSHA256, identity, ssh.FingerprintSHA256(key))
	if mode == "foreign-owner" {
		profile = strings.Replace(profile, "address: 127.0.0.1", "address: 127.0.0.2", 1)
	}
	if e = os.WriteFile(cluster, []byte(profile), 0600); e != nil {
		t.Fatal(e)
	}
	program := "#!/usr/bin/python3\nimport json,sys,pathlib,hashlib\nMODE=" + fmt.Sprintf("%q", mode) + "\nCALLS=pathlib.Path(" + fmt.Sprintf("%q", calls) + ")\nORIGINAL=pathlib.Path(" + fmt.Sprintf("%q", cluster) + ")\n" + swctlCLIStandin
	if e = os.WriteFile(binary, []byte(program), 0700); e != nil {
		t.Fatal(e)
	}
	digest := sha256.Sum256([]byte(program))
	host := &swctlAdapterHost{uploads: map[string][]byte{}, wrong: mode == "wrong-bytes", dirty: mode == "dirty"}
	c := CloudConfig{SWCtl: &SWCtlConfig{Binary: CloudFile{Path: binary, SHA256: fmt.Sprintf("%x", digest)}, Cluster: cluster, ArtifactsDir: root}, Output: t.TempDir(), Files: map[string]CloudFile{}, SSHKey: identity, swctlNodes: func(inventory.Host) swctlSmokeNode { return host }}
	if mode == "binary-tamper" {
		c.SWCtl.Binary.SHA256 = strings.Repeat("0", 64)
	}
	if mode == "mixed" {
		c.KVLoad = true
	}
	return raw, c, host, calls
}
func TestSWCtlSmokeActualCallerPreservesOracleAndAlwaysClassifiesTeardown(t *testing.T) {
	for _, tc := range []struct {
		mode  string
		pass  bool
		calls []string
	}{
		{"success", true, []string{"plan install", "install", "doctor", "probe", "uninstall"}},
		{"changed-source", true, []string{"plan install", "install", "doctor", "probe", "uninstall"}},
		{"wrong-bytes", false, []string{"plan install", "install", "doctor", "uninstall"}},
		{"doctor-bad", false, []string{"plan install", "install", "doctor", "uninstall"}},
		{"doctor-off", false, []string{"plan install", "install", "doctor", "uninstall"}},
		{"doctor-missing", false, []string{"plan install", "install", "doctor", "uninstall"}},
		{"probe-wrong", false, []string{"plan install", "install", "doctor", "probe", "uninstall"}},
		{"probe-missing", false, []string{"plan install", "install", "doctor", "probe", "uninstall"}},
		{"uninstall-partial", false, []string{"plan install", "install", "doctor", "probe", "uninstall"}},
		{"uninstall-unknown", false, []string{"plan install", "install", "doctor", "probe", "uninstall"}},
		{"dirty", false, []string{"plan install", "install", "doctor", "probe", "uninstall"}},
		{"install-refused", false, []string{"plan install", "install"}},
		{"install-unknown", false, []string{"plan install", "install"}},
		{"foreign-owner", false, []string{"plan install"}},
		{"mixed", false, []string{"plan install"}},
		{"binary-tamper", false, nil},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			raw, c, host, calls := swctlAdapterFixture(t, tc.mode)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			rows, e := RunCloudHTTP(ctx, raw, c)
			if (e == nil) != tc.pass {
				t.Fatalf("pass=%t rows=%s error=%v", tc.pass, rows, e)
			}
			data, _ := os.ReadFile(calls)
			actions := []string{}
			hash := ""
			for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
				if line == "" {
					continue
				}
				var row struct{ Action, SHA string }
				if json.Unmarshal([]byte(line), &row) != nil {
					t.Fatal("bad fixture call record")
				}
				actions = append(actions, row.Action)
				if hash == "" {
					hash = row.SHA
				}
				if hash != row.SHA {
					t.Fatal("mutable source redirected a CLI command")
				}
			}
			if !reflect.DeepEqual(actions, tc.calls) && !(len(actions) == 0 && len(tc.calls) == 0) {
				t.Fatalf("calls %v want %v; error %v", actions, tc.calls, e)
			}
			ran := false
			for _, cmd := range host.commands {
				if strings.Contains(cmd, "kv_smoke.py") && strings.Contains(cmd, "--config") {
					ran = true
					if !strings.Contains(cmd, `"keys":16`) || !strings.Contains(cmd, `"length":1048576`) || !strings.Contains(cmd, `"transport":"dc"`) {
						t.Fatalf("workload changed %s", cmd)
					}
				}
			}
			shouldRun := tc.mode == "success" || tc.mode == "changed-source" || tc.mode == "wrong-bytes" || tc.mode == "probe-wrong" || tc.mode == "probe-missing" || tc.mode == "uninstall-unknown" || tc.mode == "uninstall-partial" || tc.mode == "dirty"
			if ran != shouldRun {
				t.Fatalf("worker dispatch %t expected %t", ran, shouldRun)
			}
			if ran {
				for _, name := range cloudHelpers("", "") {
					want, _ := workerFiles.ReadFile("worker/" + name)
					if string(host.uploads[name]) != string(want) {
						t.Fatalf("worker/oracle changed %s", name)
					}
				}
			}
			if tc.mode == "install-refused" || tc.mode == "install-unknown" || tc.mode == "foreign-owner" || tc.mode == "mixed" || tc.mode == "binary-tamper" {
				if host.connections != 0 {
					t.Fatal("host work before admitted install")
				}
			}
			if tc.pass {
				if _, e := os.Stat(rows); e != nil {
					t.Fatal(e)
				}
				doctor, _ := os.ReadFile(filepath.Join(c.Output, "swctl-doctor.json"))
				if !strings.Contains(string(doctor), "NATIVE_STATS_ABSENT") || !strings.Contains(string(doctor), "NOT_PROVEN") {
					t.Fatal("expected uncertainty erased")
				}
			}
		})
	}
}

// This producer emulates only the CLI envelope/process boundary. Mono owns
// its full strict schema, install, identity, journal and native-probe controls.
const swctlCLIStandin = `
action=' '.join(sys.argv[1:3]) if sys.argv[1]=='plan' else sys.argv[1]
path=pathlib.Path(sys.argv[sys.argv.index('--cluster')+1])
with CALLS.open('a') as f:f.write(json.dumps({'Action':action,'SHA':hashlib.sha256(path.read_bytes()).hexdigest()})+'\n')
if MODE=='changed-source' and action=='plan install':ORIGINAL.write_text('changed original must not redirect the bound copy')
r={'schema':'swctl.result/v1','command':action,'cluster':'fixture-cluster','status':'PASS','partial':False,'items':[{'id':'fixture-effect','host':'fixture','status':'PASS','code':'OK'}]};exit=0
if action=='install' and MODE.startswith('install-'):
 r['status']='REFUSED' if MODE=='install-refused' else 'UNKNOWN';r['partial']=MODE=='install-unknown';exit=65 if MODE=='install-refused' else 2
if action=='doctor':
 rows=[]
 for name in ['rdma.device','rdma.port_state','rdma.gid','push.listener','dc.ready','dc.route','kv.budget_refusals','rotation.headroom','rotation.activity','native.stats','lmcache.shape','version.pins']:
  status='PASS';code='OK'
  if name in ['dc.route','kv.budget_refusals']:status='NOT_PROVEN';code='KV_COUNTER_NO_TRAFFIC'
  if name in ['rotation.headroom','rotation.activity']:status='NOT_APPLICABLE';code='MODE_OFF'
  if name=='native.stats':status='UNKNOWN';code='NATIVE_STATS_ABSENT'
  rows.append({'id':name,'host':'fixture','status':status,'code':code})
 for role in ['master','filer','volume','lmcache']:rows.append({'id':'cfg.nonpublic','host':'fixture','status':'PASS','code':'OK','observed':{'unit':'swctl-fixture-cluster-'+role+'.service'}})
 if MODE=='doctor-bad':rows[0]['status']='UNKNOWN';rows[0]['code']='RDMA_UNREADABLE'
 if MODE=='doctor-missing':rows.pop()
 if MODE=='doctor-off':rows[3]['status']='NOT_APPLICABLE';rows[3]['code']='MODE_OFF'
 r['items']=rows;r['status']='UNKNOWN';exit=2
if action=='probe':
 r['items']=[{'id':'probe.native','host':'fixture','status':'PASS','code':'PROBE_EXACT'}]
 if MODE=='probe-wrong':r['status']='FAIL';r['items'][0]['status']='FAIL';r['items'][0]['code']='PROBE_WRONG_BYTES';exit=1
 if MODE=='probe-missing':r['items']=[]
if action=='uninstall' and MODE=='uninstall-partial':r['partial']=True
if action=='uninstall' and MODE=='uninstall-unknown':r['status']='UNKNOWN';r['partial']=True;exit=2
print(json.dumps(r));sys.exit(exit)
`
