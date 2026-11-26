package cloud

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// ContextEnv names the pack context seaweed-cloud hands an exec pack.
const ContextEnv = "SEAWEED_CLOUD_PACK_CONTEXT"

// Host is what a power fault needs of one inventory host.
type Host struct {
	ID         string // the inventory host id
	ResourceID string // the VM's ARM id
	Address    string // management address
	Port       int
	User       string
	HostKey    string // the approved OpenSSH public host key
}

// Run is the part of the run's pack context and inventory a power fault reads.
type Run struct {
	RunID   string
	Owned   []string // managed_resource_ids
	Hosts   map[string]Host
	SSHKey  string // private key path (never read here)
	Context string // the context file's path, for the record
	// The run's declared deadlines (inventory deadlines.work and .hard).
	Work, Hard time.Time
}

// LoadRun reads the pack context and its inventory. Anything missing or
// inconsistent is an error: the fault never guesses a VM, a key or an owner.
func LoadRun(contextPath string) (Run, error) {
	var ctx struct {
		Schema    string            `json:"schema"`
		RunID     string            `json:"run_id"`
		Inventory string            `json:"inventory"`
		SSHKey    string            `json:"ssh_key"`
		HostKey   string            `json:"host_key"`
		HostKeys  map[string]string `json:"host_keys"`
	}
	if err := readJSON(contextPath, &ctx); err != nil {
		return Run{}, fmt.Errorf("pack context: %w", err)
	}
	if ctx.Schema != "seaweed.cloud-pack-context/v1" || ctx.Inventory == "" || ctx.SSHKey == "" {
		return Run{}, errors.New("pack context: not a seaweed.cloud-pack-context/v1 with an inventory and an ssh key")
	}
	var inv struct {
		Schema    string   `json:"schema"`
		RunID     string   `json:"run_id"`
		Target    string   `json:"target"`
		Owned     []string `json:"managed_resource_ids"`
		Deadlines struct {
			Work time.Time `json:"work"`
			Hard time.Time `json:"hard"`
		} `json:"deadlines"`
		Hosts []struct {
			ID         string `json:"id"`
			ResourceID string `json:"resource_id"`
			Management struct {
				Address string `json:"address"`
				Port    int    `json:"port"`
				User    string `json:"user"`
				HostKey string `json:"host_key"`
			} `json:"management"`
		} `json:"hosts"`
	}
	if err := readJSON(ctx.Inventory, &inv); err != nil {
		return Run{}, fmt.Errorf("inventory: %w", err)
	}
	if inv.Schema != "seaweed.inventory/v1" || inv.Target != "azure" || inv.RunID == "" || len(inv.Owned) == 0 || len(inv.Hosts) == 0 {
		return Run{}, errors.New("inventory: not an azure seaweed.inventory/v1 with a run id, managed resources and hosts")
	}
	if inv.Deadlines.Work.IsZero() || !inv.Deadlines.Hard.After(inv.Deadlines.Work) {
		return Run{}, errors.New("inventory: no work deadline before a hard deadline")
	}
	if ctx.RunID != "" && ctx.RunID != inv.RunID {
		return Run{}, fmt.Errorf("pack context: run %q, but its inventory is run %q", ctx.RunID, inv.RunID)
	}
	r := Run{RunID: inv.RunID, Owned: inv.Owned, Hosts: map[string]Host{}, SSHKey: ctx.SSHKey, Context: contextPath, Work: inv.Deadlines.Work, Hard: inv.Deadlines.Hard}
	for _, h := range inv.Hosts {
		key := h.Management.HostKey
		if k := ctx.HostKeys[h.ID]; k != "" {
			key = k
		} else if key == "" {
			key = ctx.HostKey
		}
		if h.ID == "" || h.ResourceID == "" || h.Management.Address == "" || h.Management.User == "" || key == "" {
			return Run{}, fmt.Errorf("inventory: host %q lacks a resource id, a management address and user, or an approved host key", h.ID)
		}
		if _, dup := r.Hosts[h.ID]; dup {
			return Run{}, fmt.Errorf("inventory: host %q twice", h.ID)
		}
		port := h.Management.Port
		if port == 0 {
			port = 22
		}
		r.Hosts[h.ID] = Host{ID: h.ID, ResourceID: h.ResourceID, Address: h.Management.Address, Port: port, User: h.Management.User, HostKey: strings.TrimSpace(key)}
	}
	for id := range ctx.HostKeys {
		if _, ok := r.Hosts[id]; !ok {
			return Run{}, fmt.Errorf("pack context: a host key for %q, which is not an inventory host", id)
		}
	}
	return r, nil
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
