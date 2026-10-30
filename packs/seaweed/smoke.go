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

//go:embed worker/kv_smoke.py worker/soak_native_worker.py worker/soak_oracle.py
var workerFiles embed.FS

type SmokeSettings struct {
	NativeDir string `json:"native_dir"`
	HelperDir string `json:"helper_dir"`
	Keys      int    `json:"keys"`
	Length    int    `json:"length"`
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
	if d == nil || d.State != "resolved" || (d.Transport != "dc" && d.Transport != "rc" && d.Transport != "http") || s.Keys < 1 || s.Keys > 16 || s.Length < 1 || s.Length > 4*1024*1024 || s.NativeDir == "/" || !path.IsAbs(s.NativeDir) || path.Clean(s.NativeDir) != s.NativeDir || s.HelperDir == "/" || !path.IsAbs(s.HelperDir) || path.Clean(s.HelperDir) != s.HelperDir {
		return nil, errors.New("invalid bounded smoke settings")
	}
	var filer, volume, worker *inventory.Host
	for i := range d.Hosts {
		h := &d.Hosts[i]
		if contains(h.Roles, "filer") {
			if filer != nil {
				return nil, errors.New("single-filer smoke only")
			}
			filer = h
		}
		if contains(h.Roles, "volume") {
			if volume != nil {
				return nil, errors.New("single-volume smoke only")
			}
			volume = h
		}
		if h.ID == client && contains(h.Roles, "client") {
			worker = h
		}
	}
	if filer == nil || volume == nil || worker == nil || (d.Transport != "http" && worker.RDMA.GIDIndex == nil) {
		return nil, errors.New("missing filer/volume/client or GID")
	}
	c := SmokeConfig{RunID: d.RunID, Client: client, Transport: d.Transport, FilerGRPC: endpoint(*filer, "filer_grpc"), FilerHTTP: "http://" + endpoint(*filer, "filer_http"), Root: "/testops/" + d.RunID + "/" + client, NativeDir: s.NativeDir, HelperDir: s.HelperDir, Keys: s.Keys, Length: s.Length}
	if d.Transport != "http" {
		c.RDMAAddress, c.RDMAControl, c.DCControl = endpoint(*volume, "rdma"), endpoint(*volume, "rdma_control"), endpoint(*volume, "dc_control")
		c.Device, c.DevicePort, c.GIDIndex = worker.RDMA.Device, worker.RDMA.Port, worker.RDMA.GIDIndex
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	return []string{"-I", path.Join(s.HelperDir, "kv_smoke.py"), "--config", string(raw)}, nil
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
	files := in.Files[client]
	for name := range files {
		for _, stem := range []string{"soak_native_worker", "soak_oracle"} {
			if strings.HasPrefix(name, path.Join(s.HelperDir, stem)+"/") || name == path.Join(s.HelperDir, stem+".pyc") || strings.HasPrefix(name, path.Join(s.HelperDir, "__pycache__", stem)+".") {
				return errors.New("competing smoke helper package/bytecode refused")
			}
		}
	}
	for _, name := range []string{"kv_smoke.py", "soak_native_worker.py", "soak_oracle.py"} {
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
	native := false
	for name := range files {
		if strings.HasPrefix(name, s.NativeDir+"/seaweedkv_native/") && strings.HasSuffix(name, ".so") {
			native = true
		}
	}
	if !native {
		return errors.New("native extension not in read-back deployment")
	}
	return nil
}
