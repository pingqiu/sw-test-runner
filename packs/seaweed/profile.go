package seaweed

import (
	"encoding/json"
	"errors"
	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/inventory"
	"path"
	"slices"
	"strings"
)

type ProfileConfig struct {
	PCI            string            `json:"pci"`
	PhysicalPCI    string            `json:"physical_pci"`
	Firmware       string            `json:"firmware"`
	Host           string            `json:"host"`
	BootID         string            `json:"boot_id"`
	Root           string            `json:"root"`
	Deadline       string            `json:"deadline"`
	Device         string            `json:"device"`
	Port           int               `json:"port"`
	GIDIndex       int               `json:"gid_index"`
	Address        string            `json:"address"`
	NIC            string            `json:"nic"`
	MTU            int               `json:"mtu"`
	ActiveMTU      int               `json:"active_mtu"`
	Roles          []string          `json:"roles"`
	Services       map[string]int    `json:"services"`
	ServerAddress  string            `json:"server_address"`
	ServerServices map[string]int    `json:"server_services"`
	Release        string            `json:"release"`
	Products       map[string]string `json:"products"`
	Disks          []inventory.Disk  `json:"disks"`
	// Soak plans only (lab_profile.py master_toml / ready_assign); absent otherwise.
	Rotation    *Rotation     `json:"rotation,omitempty"`
	ReadyAssign *AssignLayout `json:"ready_assign,omitempty"`
}

func checkProfile(j tr.JobPin, in tr.PackInputs, d *inventory.Document, host, action, role string, s SmokeSettings) error {
	want, err := ProfileArgs(d, host, action, role, s, in.Files[host])
	if err != nil {
		return err
	}
	if !slices.Equal(want, j.Argv) || j.Executable != "/usr/bin/python3.12" || in.Files[host][j.Executable] != j.SHA256 {
		return errors.New("profile argv/interpreter not pinned to joined inventory")
	}
	for _, name := range []string{"lab_profile.py", "vf_preflight.py"} {
		body, err := workerFiles.ReadFile("worker/" + name)
		if err != nil {
			return err
		}
		if in.Files[host][path.Join(s.HelperDir, name)] != hash(body) {
			return errors.New("profile helper not protected/read back")
		}
		found := false
		for _, raw := range in.Scripts {
			if hash(raw) == hash(body) {
				found = true
			}
		}
		if !found {
			return errors.New("profile helper not independently scanned")
		}
	}
	return nil
}

func ProfileArgs(d *inventory.Document, host, action, role string, s SmokeSettings, files map[string]string) ([]string, error) {
	if d == nil || d.State != "resolved" || d.Transport != "dc" || d.Observation == nil || len(d.Hosts) != 2 || !slices.Contains([]string{"preflight", "deploy", "ready", "collect"}, action) {
		return nil, errors.New("M01/M02 DC profile only")
	}
	release := path.Dir(s.HelperDir)
	if release == "/" || !path.IsAbs(release) || path.Clean(s.HelperDir) != s.HelperDir || s.HelperDir != path.Join(release, "worker") || s.NativeDir != path.Join(release, "native") {
		return nil, errors.New("profile protected release layout")
	}
	var h, server *inventory.Host
	for i := range d.Hosts {
		x := &d.Hosts[i]
		if x.ID != "m01" && x.ID != "m02" {
			return nil, errors.New("profile supports exactly M01 and M02")
		}
		if x.ID == host {
			h = x
		}
		if x.ID == "m02" && slices.Equal(x.Roles, []string{"master", "filer", "volume"}) {
			server = x
		}
		if x.ID == "m01" && !slices.Equal(x.Roles, []string{"client"}) {
			return nil, errors.New("M01 client only")
		}
	}
	if h == nil || server == nil || h.RDMA.GIDIndex == nil || h.RDMA.Port != 1 || h.RDMA.Device != "mlx5_0" || (action == "deploy" && (role == "client" || !contains(h.Roles, role))) || (action != "deploy" && role != "") {
		return nil, errors.New("profile host/role/device mismatch")
	}
	c := ProfileConfig{Host: host, Root: h.RunRoot, Deadline: d.Deadlines.Work, Device: h.RDMA.Device, Port: h.RDMA.Port, GIDIndex: *h.RDMA.GIDIndex, Address: h.Data.Address, NIC: h.Data.NIC, MTU: h.Data.MTU, ActiveMTU: h.RDMA.ActiveMTU, Roles: h.Roles, Services: map[string]int{}, ServerServices: map[string]int{}, ServerAddress: server.Data.Address, Release: release, Products: map[string]string{}, Rotation: s.rotation, ReadyAssign: s.readyAssign}
	// Fixed M01/M02 profile identity from the sealed bbb01f8 packet's
	// observations/{m01,m02}-fabric-evidence.json, manifest 60f4f17f.
	// A replacement requires a new reviewed profile, never historical credit.
	c.PCI = "/sys/devices/pci0000:00/0000:00:01.0/0000:01:00.1"
	c.PhysicalPCI = "/sys/devices/pci0000:00/0000:00:01.0/0000:01:00.0"
	c.Firmware = "16.33.1048"
	c.Disks = append([]inventory.Disk{}, h.Disks...)
	for _, o := range d.Observation.Hosts {
		if o.HostID == host {
			c.BootID = o.BootID
		}
	}
	if c.BootID == "" {
		return nil, errors.New("profile missing boot")
	}
	for _, v := range h.Services {
		c.Services[v.Name] = v.Port
	}
	for _, v := range server.Services {
		c.ServerServices[v.Name] = v.Port
	}
	for _, name := range []string{"master_http", "master_grpc", "filer_http", "filer_grpc", "volume_http", "volume_grpc", "rdma", "rdma_push", "rdma_control", "dc_control"} {
		if c.ServerServices[name] == 0 {
			return nil, errors.New("profile missing explicit port: " + name)
		}
	}
	if host == "m02" {
		for _, name := range []string{"weed", "weed-volume"} {
			pin := files[path.Join(release, "bin", name)]
			if len(pin) != 64 || strings.Trim(pin, "0123456789abcdef") != "" {
				return nil, errors.New("profile missing product pin")
			}
			c.Products[name] = pin
		}
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	return []string{"-I", path.Join(s.HelperDir, "lab_profile.py"), "--config", string(raw), "--action", action, "--role", role}, nil
}
