package seaweed

import (
	"embed"
	"encoding/json"
	"errors"
	"net"
	"path"
	"strconv"
	"strings"

	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/inventory"
)

//go:embed worker/kv_smoke.py worker/soak_native_worker.py worker/soak_oracle.py worker/soak_stats.py worker/soak_load.py worker/lab_profile.py worker/vf_preflight.py worker/cloud_batch.py worker/chaos_load.py
var workerFiles embed.FS

type SmokeSettings struct {
	NativeDir string `json:"native_dir"`
	HelperDir string `json:"helper_dir"`
	Keys      int    `json:"keys"`
	Length    int    `json:"length"`
	// Replication is the cloud-only KV loader copy count ("" keeps 000).
	Replication string `json:"replication,omitempty"`
	// Soak-only profile fields, set by Plan.profileSettings from Plan.Soak.
	// Unexported: plan JSON cannot set them here.
	rotation    *Rotation
	readyAssign *AssignLayout
	soakSetup   *SoakSetup
}

// SmokeConfig is emitted by the common inventory consumer, never a cloud driver.
// The protected, read-back script consumes exactly these arguments.
type SmokeConfig struct {
	RunID       string `json:"run_id"`
	Client      string `json:"client"`
	Transport   string `json:"transport"`
	FilerGRPC   string `json:"filer_grpc"`
	FilerHTTP   string `json:"filer_http"`
	Root        string `json:"root"`
	RDMAAddress string `json:"rdma_address,omitempty"`
	RDMAControl string `json:"rdma_control,omitempty"`
	DCControl   string `json:"dc_control,omitempty"`
	Device      string `json:"device,omitempty"`
	DevicePort  int    `json:"device_port,omitempty"`
	GIDIndex    *int   `json:"gid_index,omitempty"`
	NativeDir   string `json:"native_dir"`
	HelperDir   string `json:"helper_dir"`
	Keys        int    `json:"keys"`
	Length      int    `json:"length"`
	Replication string `json:"replication,omitempty"`
}

func endpoint(h inventory.Host, name string) string {
	for _, s := range h.Services {
		if s.Name == name {
			return net.JoinHostPort(h.Data.Address, strconv.Itoa(s.Port))
		}
	}
	return ""
}

// SmokeArgs produces the exact bounded smoke argv from the joined inventory.
// It does not execute, import a native module or grant admission.
func SmokeArgs(d *inventory.Document, client string, s SmokeSettings) ([]string, error) {
	c, err := smokeConfig(d, client, s)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	return []string{"-I", path.Join(s.HelperDir, "kv_smoke.py"), "--config", string(raw)}, nil
}

func smokeConfig(d *inventory.Document, client string, s SmokeSettings) (SmokeConfig, error) {
	if d == nil || d.State != "resolved" || (d.Transport != "dc" && d.Transport != "rc" && d.Transport != "http") || s.Keys < 1 || s.Keys > 16 || s.Length < 1 || s.Length > 4*1024*1024 || s.NativeDir == "/" || !path.IsAbs(s.NativeDir) || path.Clean(s.NativeDir) != s.NativeDir || s.HelperDir == "/" || !path.IsAbs(s.HelperDir) || path.Clean(s.HelperDir) != s.HelperDir {
		return SmokeConfig{}, errors.New("invalid bounded smoke settings")
	}
	var filer, volume, worker *inventory.Host
	for i := range d.Hosts {
		h := &d.Hosts[i]
		if contains(h.Roles, "filer") {
			if filer != nil {
				return SmokeConfig{}, errors.New("single-filer smoke only")
			}
			filer = h
		}
		if contains(h.Roles, "volume") {
			if volume != nil {
				return SmokeConfig{}, errors.New("single-volume smoke only")
			}
			volume = h
		}
		if h.ID == client && contains(h.Roles, "client") {
			worker = h
		}
	}
	if s.Replication != "" && (s.Replication != "001" || d.Transport != "http") {
		return SmokeConfig{}, errors.New("replication 001 is cloud HTTP only")
	}
	if filer == nil || volume == nil || worker == nil || (d.Transport != "http" && worker.RDMA.GIDIndex == nil) {
		return SmokeConfig{}, errors.New("missing filer/volume/client or GID")
	}
	c := SmokeConfig{RunID: d.RunID, Client: client, Transport: d.Transport, FilerGRPC: endpoint(*filer, "filer_grpc"), FilerHTTP: "http://" + endpoint(*filer, "filer_http"), Root: "/testops/" + d.RunID + "/" + client, NativeDir: s.NativeDir, HelperDir: s.HelperDir, Keys: s.Keys, Length: s.Length, Replication: s.Replication}
	if d.Transport != "http" {
		c.RDMAAddress, c.RDMAControl, c.DCControl = endpoint(*volume, "rdma"), endpoint(*volume, "rdma_control"), endpoint(*volume, "dc_control")
		c.Device, c.DevicePort, c.GIDIndex = worker.RDMA.Device, worker.RDMA.Port, worker.RDMA.GIDIndex
	}
	return c, nil
}
func checkSmoke(j tr.JobPin, in tr.PackInputs, d *inventory.Document, client string, s SmokeSettings) error {
	want, err := SmokeArgs(d, client, s)
	if err != nil {
		return err
	}
	if len(want) != len(j.Argv) {
		return errors.New("smoke argv mismatch")
	}
	for i := range want {
		if want[i] != j.Argv[i] {
			return errors.New("smoke argv not derived from joined inventory")
		}
	}
	return checkPinned(in, client, s, []string{"kv_smoke.py", "soak_native_worker.py", "soak_oracle.py"}, true)
}

// checkPinned: each named helper is the embedded byte-for-byte source, read
// back from the host and independently scanned; no competing package or
// bytecode can shadow it; the native extension is deployed when required.
func checkPinned(in tr.PackInputs, host string, s SmokeSettings, names []string, native bool) error {
	files := in.Files[host]
	for name := range files {
		for _, stem := range []string{"soak_native_worker", "soak_oracle", "soak_stats", "soak_load", "kv_smoke"} {
			if strings.HasPrefix(name, path.Join(s.HelperDir, stem)+"/") || name == path.Join(s.HelperDir, stem+".pyc") || strings.HasPrefix(name, path.Join(s.HelperDir, "__pycache__", stem)+".") {
				return errors.New("competing smoke helper package/bytecode refused")
			}
		}
	}
	for _, name := range names {
		body, err := workerFiles.ReadFile("worker/" + name)
		if err != nil {
			return err
		}
		remote := path.Join(s.HelperDir, name)
		if files[remote] != hash(body) {
			return errors.New("smoke/helper bytes not protected and read back")
		}
		scanned := false
		for _, raw := range in.Scripts {
			if hash(raw) == hash(body) {
				scanned = true
			}
		}
		if !scanned {
			return errors.New("smoke/helper source not independently pinned and scanned")
		}
	}
	found := !native
	for name := range files {
		if strings.HasPrefix(name, s.NativeDir+"/seaweedkv_native/") && strings.HasSuffix(name, ".so") {
			found = true
		}
	}
	if !found {
		return errors.New("native extension not in read-back deployment")
	}
	return nil
}
