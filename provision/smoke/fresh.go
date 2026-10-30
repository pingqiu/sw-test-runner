package smoke

// This file is a private, typed derivation, not an alternate S0b entry.
import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"time"

	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/infra"
	"github.com/pingqiu/sw-test-runner/inventory"
	"github.com/pingqiu/sw-test-runner/packs/seaweed"
	"gopkg.in/yaml.v3"
)

const freshApprovalPath = "/etc/sw-test-runner/codex02/cloud-approval.json"

type freshApproval struct {
	Version    string     `json:"version"`
	Template   tr.FilePin `json:"template"`
	Executable tr.FilePin `json:"executable"`
	Recipe     tr.FilePin `json:"recipe"`
}
type diskSlot struct {
	ID         string `json:"id"`
	ResourceID string `json:"resource_id"`
	LUN        int    `json:"lun"`
}
type freshTemplate struct {
	Version   string               `json:"version"`
	Config    Config               `json:"config"`
	Requested inventory.Document   `json:"requested"`
	MinMTU    int                  `json:"min_mtu"`
	MaxMTU    int                  `json:"max_mtu"`
	Disks     []diskSlot           `json:"disks"`
	Binding   tr.RunBinding        `json:"binding"`
	Policy    tr.ApprovedRunPolicy `json:"policy"`
	Plan      seaweed.Plan         `json:"plan"`
	Scenario  tr.Scenario          `json:"scenario"`
	GO        tr.ApprovedGO        `json:"go"`
	Bootstrap tr.PolicyBootstrap   `json:"bootstrap"`
}
type diskFact struct {
	ID         string `json:"id"`
	ResourceID string `json:"resource_id"`
	LUN        int    `json:"lun"`
	DeviceID   string `json:"device_id"`
}
type freshFacts struct {
	ExportSHA256 string     `json:"export_sha256"`
	Boot         string     `json:"boot"`
	BootAfter    string     `json:"boot_after"`
	DataAddress  string     `json:"data_address"`
	NIC          string     `json:"nic"`
	MTU          int        `json:"mtu"`
	Disks        []diskFact `json:"disks"`
	Start        time.Time  `json:"start"`
	End          time.Time  `json:"end"`
}
type freshAuthority struct {
	approval freshApproval
	template freshTemplate
	setup    func(context.Context, Config, []byte) ([]byte, error)
	readback func(context.Context, infra.DeploymentPolicy, string) (infra.DeploymentReceipt, error)
	verify   func() error
}

func loadFresh(c Config) (*freshAuthority, error) {
	a := &freshAuthority{}
	b, e := protectedFresh(freshApprovalPath)
	if e != nil || StrictJSON(b, &a.approval) != nil || a.approval.Version != "seaweed.fresh-approval/v1" {
		return nil, errors.New("missing protected approval")
	}
	for _, p := range []tr.FilePin{a.approval.Template, a.approval.Executable, a.approval.Recipe} {
		b, e = protectedFresh(p.Path)
		if e != nil || digest(b) != p.SHA256 {
			return nil, errors.New("protected input pin mismatch")
		}
	}
	exe, e := os.Executable()
	if e != nil {
		return nil, e
	}
	installed, e := filepath.EvalSymlinks(exe)
	if e != nil || installed != a.approval.Executable.Path {
		return nil, errors.New("wrong materializer executable")
	}
	b, e = protectedFresh(a.approval.Template.Path)
	if e != nil || strictFreshTemplate(b, &a.template) != nil {
		return nil, errors.New("invalid template")
	}
	a.setup = func(ctx context.Context, c Config, raw []byte) ([]byte, error) {
		// Recipe is protected and independently pinned, never submitted argv.
		// It installs the fixed package and emits ONLY this bounded typed probe.
		b, e := protectedFresh(a.approval.Recipe.Path)
		if e != nil || digest(b) != a.approval.Recipe.SHA256 {
			return nil, errors.New("setup recipe changed")
		}
		return Native(ctx, c.Output, a.approval.Recipe.Path, filepath.Join(c.Output, "requested-inventory.json"))
	}
	a.readback = func(ctx context.Context, p infra.DeploymentPolicy, key string) (infra.DeploymentReceipt, error) {
		n := &infra.Node{Host: p.Host, User: p.User, HostKey: p.HostKey, KeyFile: key}
		return n.VerifyDeployment(ctx, p)
	}
	a.verify = func() error {
		for _, p := range []tr.FilePin{a.approval.Template, a.approval.Executable, a.approval.Recipe} {
			b, e := protectedFresh(p.Path)
			if e != nil || digest(b) != p.SHA256 {
				return errors.New("delegated input changed")
			}
		}
		b, e := protectedFresh(freshApprovalPath)
		if e != nil {
			return e
		}
		var current freshApproval
		if StrictJSON(b, &current) != nil || !reflect.DeepEqual(current, a.approval) {
			return errors.New("approval changed")
		}
		return nil
	}
	return a, a.check(c)
}

func (a *freshAuthority) check(c Config) error {
	t := a.template
	if a.verify == nil || a.verify() != nil {
		return errors.New("delegation is not protected")
	}
	if t.Version != "seaweed.fresh-template/v1" || !reflect.DeepEqual(c, t.Config) || len(t.Requested.Hosts) != 1 || t.Requested.Target != "azure" || t.Requested.Transport != "http" || t.Requested.State != "requested" || t.Requested.Observation != nil || len(t.Binding.Hosts) != 1 || len(t.Policy.Deployments) != 1 || t.Plan.Requested != "" || t.Plan.Resolved != "" || t.Scenario.Metadata["seaweed_plan"] != "" || t.GO.RequestSHA256 != "" || !t.GO.AllowMutating || len(t.GO.Cleanup) != 0 || t.GO.SourceID == "" || !reflect.DeepEqual(t.GO.Binding, t.Binding) || t.MinMTU < 576 || t.MaxMTU < t.MinMTU || t.MaxMTU > 65536 {
		return errors.New("invalid fixed fresh template")
	}
	h, d := t.Binding.Hosts[0], t.Policy.Deployments[0]
	_, _, decl, e := inputs(c)
	if e != nil || h.Node != t.Requested.Hosts[0].ID || h.HostKey == "" || d.HostKey != h.HostKey || d.NodeID != h.Node || d.User != h.User || d.ExpectedBootID != "" || t.Bootstrap.Policy.Path != t.Binding.Policy.Path || t.Bootstrap.Approval.Path == "" || t.Binding.Workload != "kv" || t.Binding.Transport != "http" || !t.Binding.WorkDeadline.Equal(decl.Run.Work) || !t.Binding.HardDeadline.Equal(decl.Run.Hard) || t.Binding.LeaseResource != "azure-"+decl.Group || len(t.Disks) != len(t.Requested.Hosts[0].Disks) {
		return errors.New("incomplete fixed binding")
	}
	if a.setup == nil || a.readback == nil {
		return errors.New("qualified setup unavailable")
	}
	if !time.Now().Before(t.Binding.WorkDeadline) {
		return errors.New("original work deadline expired")
	}
	if _, e := tr.ReadLiveLease(t.Bootstrap.LedgerDir, t.Binding.LeaseID, t.Binding.LeaseOwner); e != nil {
		return e
	}
	if _, e := os.Lstat(t.Bootstrap.CancelFile); !os.IsNotExist(e) {
		return errors.New("cancelled or unknown cancellation")
	}
	return nil
}

func (a *freshAuthority) derive(raw, probe []byte, start, now time.Time) (map[string][]byte, error) {
	var t freshTemplate
	b, _ := json.Marshal(a.template)
	if e := json.Unmarshal(b, &t); e != nil {
		return nil, e
	} // private value copy
	var f freshFacts
	if len(probe) > 16384 || StrictJSON(probe, &f) != nil || f.ExportSHA256 != digest(raw) || f.Boot == "" || f.Boot != f.BootAfter || f.Start.Before(start) || f.End.Before(f.Start) || f.End.After(now) || !now.Before(t.Binding.WorkDeadline) {
		return nil, errors.New("unqualified capture/export/boot/time")
	}
	d, e := inventory.Decode(raw)
	if e != nil {
		return nil, e
	}
	if len(d.Hosts) != 1 {
		return nil, errors.New("foreign host set")
	}
	ip := d.Hosts[0].Management.Address
	// Only the export's IP is variable. Everything else, including the entire
	// exact-name resource set, is independently fixed by the approved template.
	comparison := *d
	comparison.Hosts = append([]inventory.Host(nil), d.Hosts...)
	comparison.Hosts[0].Management.Address = t.Requested.Hosts[0].Management.Address
	if !reflect.DeepEqual(comparison, t.Requested) {
		return nil, errors.New("unapproved Terraform shape/resource/field")
	}
	h := &d.Hosts[0]
	if f.DataAddress != h.Data.Address || f.NIC == "" || f.MTU < t.MinMTU || f.MTU > t.MaxMTU || h.Data.NIC != "" && h.Data.NIC != f.NIC || h.Data.MTU != 0 && h.Data.MTU != f.MTU {
		return nil, errors.New("foreign NIC/data/MTU")
	}
	h.Data.NIC, h.Data.MTU = f.NIC, f.MTU
	if len(f.Disks) != len(t.Disks) {
		return nil, errors.New("foreign disk set")
	}
	for i := range h.Disks {
		s, found := diskSlot{}, false
		for _, slot := range t.Disks {
			if slot.ID == h.Disks[i].ID {
				if found {
					return nil, errors.New("duplicate disk slot")
				}
				s, found = slot, true
			}
		}
		matches := 0
		for _, v := range f.Disks {
			if v.ID == s.ID && v.ResourceID == s.ResourceID && v.LUN == s.LUN && v.DeviceID != "" && (h.Disks[i].DeviceID == "" || h.Disks[i].DeviceID == v.DeviceID) {
				h.Disks[i].DeviceID = v.DeviceID
				matches++
			}
		}
		if !found || matches != 1 || h.Disks[i].FormatAllowed == nil || *h.Disks[i].FormatAllowed {
			return nil, errors.New("unqualified disk/LUN/format")
		}
	}
	key := t.Binding.Hosts[0].HostKey
	if h.Management.HostKey == nil || *h.Management.HostKey != "" && *h.Management.HostKey != key {
		return nil, errors.New("foreign key")
	}
	h.Management.HostKey = &key
	d.Observation = &inventory.Observation{Hosts: []inventory.HostObservation{{HostID: h.ID, BootID: f.Boot, CapturedAt: f.End.UTC().Format(time.RFC3339Nano), EvidenceSHA256: digest(probe)}}}
	resolved, e := inventory.Resolve(raw, *d)
	if e != nil {
		return nil, e
	}
	t.Plan.Requested, t.Plan.Resolved = string(raw), string(resolved)
	p, e := seaweed.MarshalPlan(t.Plan)
	if e != nil {
		return nil, e
	}
	if t.Scenario.Metadata == nil {
		t.Scenario.Metadata = map[string]string{}
	}
	t.Scenario.Metadata["seaweed_plan"] = string(p)
	scenario, e := yaml.Marshal(t.Scenario)
	if e != nil {
		return nil, e
	}
	endpoint := net.JoinHostPort(ip, fmt.Sprint(h.Management.Port))
	t.Binding.Hosts[0].Host = endpoint
	t.Policy.Deployments[0].Host, t.Policy.Deployments[0].ExpectedBootID = endpoint, f.Boot
	files := map[string][]byte{}
	put := func(pin *tr.FilePin, v any) error {
		b, e := json.Marshal(v)
		if e != nil {
			return e
		}
		if _, dup := files[pin.Path]; dup || !filepath.IsAbs(pin.Path) {
			return errors.New("duplicate/nonabsolute publication")
		}
		files[pin.Path] = b
		pin.SHA256 = digest(b)
		return nil
	}
	if e = put(&t.Binding.Artifacts, tr.ApprovedArtifacts{Version: "s0b-artifacts-v1", Deployments: t.Policy.Deployments}); e != nil {
		return nil, e
	}
	if e = put(&t.Binding.Policy, t.Policy); e != nil {
		return nil, e
	}
	if e = put(&t.Binding.Inventory, tr.ApprovedInventory{Version: "s0b-inventory-v1", Hosts: t.Binding.Hosts}); e != nil {
		return nil, e
	}
	if _, dup := files[t.Binding.Scenario.Path]; dup {
		return nil, errors.New("duplicate scenario path")
	}
	files[t.Binding.Scenario.Path] = scenario
	t.Binding.Scenario.SHA256 = digest(scenario)
	requestPin := tr.FilePin{Path: t.Config.Request}
	if e = put(&requestPin, tr.ClaimedRequest{Version: "s0b-request-v1", Binding: t.Binding}); e != nil {
		return nil, e
	}
	t.GO.Binding, t.GO.RequestSHA256 = t.Binding, requestPin.SHA256
	if e = put(&t.Bootstrap.Approval, t.GO); e != nil {
		return nil, e
	}
	t.Bootstrap.Policy = t.Binding.Policy
	bootstrapPin := tr.FilePin{Path: t.Config.Policy}
	if e = put(&bootstrapPin, t.Bootstrap); e != nil {
		return nil, e
	}
	return files, nil
}

func (a *freshAuthority) publish(ctx context.Context, c Config, raw []byte) error {
	if e := a.check(c); e != nil {
		return e
	}
	if e := stamp(filepath.Join(c.Output, "materialization-started"), "one derivation only\n"); e != nil {
		return e
	}
	start := time.Now()
	probe, e := a.setup(ctx, c, raw)
	if e != nil {
		return errors.New("setup failed; private stdout is not approval")
	}
	files, e := a.derive(raw, probe, start, time.Now())
	if e != nil {
		return e
	}
	var f freshFacts
	_ = StrictJSON(probe, &f)
	var artifacts tr.ApprovedArtifacts
	if e = json.Unmarshal(files[a.template.Binding.Artifacts.Path], &artifacts); e != nil {
		return e
	}
	p := artifacts.Deployments[0]
	qualifiedStart := time.Now()
	r, e := a.readback(ctx, p, a.template.Bootstrap.KeyFiles[p.NodeID])
	policyBytes, _ := json.Marshal(p)
	if e != nil || r.NodeID != p.NodeID || r.User != p.User || r.PolicySHA256 != digest(policyBytes) || r.ReadStart.Before(qualifiedStart) || r.ReadEnd.Before(r.ReadStart) || r.ReadEnd.After(time.Now()) || r.BootID != f.Boot || r.WorkloadUID != p.WorkloadUID || r.Host != p.Host || r.HostKey != p.HostKey || len(r.Files) != len(p.Files) {
		return errors.New("setup/readback generation mismatch")
	}
	for _, file := range p.Files {
		if r.Files[file.Path] != file.SHA256 {
			return errors.New("fixed deployed bytes changed")
		}
	}
	if e = a.check(c); e != nil {
		return e
	}
	if ctx.Err() != nil || !time.Now().Before(a.template.Binding.WorkDeadline) {
		return errors.New("cancelled/late publication")
	}
	// Bootstrap last: every file is exclusive, protected and fsynced. Partial
	// publication never dispatches; subsequent attempts cannot overwrite it.
	for name, b := range files {
		if name != c.Policy {
			if e = publishFresh(name, b); e != nil {
				return e
			}
		}
	}
	receipt := map[string]any{"template": a.approval.Template, "materializer": a.approval.Executable, "recipe": a.approval.Recipe, "source_id": a.template.GO.SourceID, "requested_sha256": digest(raw), "evidence_sha256": digest(probe), "documents": map[string]string{}}
	receipt["readback_sha256"] = digest([]byte(r.Transcript))
	for name, b := range files {
		receipt["documents"].(map[string]string)[name] = digest(b)
	}
	b, _ := json.Marshal(receipt)
	if e = publishFresh(filepath.Join(c.Output, "derivation.json"), b); e != nil {
		return e
	}
	if e = publishFresh(filepath.Join(c.Output, "observation.json"), probe); e != nil {
		return e
	}
	if e = publishFresh(filepath.Join(c.Output, "readback.txt"), []byte(r.Transcript)); e != nil {
		return e
	}
	return publishFresh(c.Policy, files[c.Policy])
}
