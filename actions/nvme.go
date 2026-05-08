package actions

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/infra"
)

// RegisterNVMeActions registers NVMe/TCP client actions.
func RegisterNVMeActions(r *tr.Registry) {
	r.RegisterFunc("nvme_connect", tr.TierBlock, nvmeConnect)
	r.RegisterFunc("nvme_disconnect", tr.TierBlock, nvmeDisconnect)
	r.RegisterFunc("nvme_get_device", tr.TierBlock, nvmeGetDevice)
	r.RegisterFunc("nvme_cleanup", tr.TierBlock, nvmeCleanup)
}

// nvmeConnect connects to an NVMe/TCP target.
// Params: target (required). Uses TargetSpec.NvmePort and NQN().
// Returns: value = NQN (for subsequent disconnect).
func nvmeConnect(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
	targetName := act.Target
	if targetName == "" {
		return nil, fmt.Errorf("nvme_connect: target is required")
	}

	spec, ok := actx.Scenario.Targets[targetName]
	if !ok {
		return nil, fmt.Errorf("nvme_connect: target %q not in scenario", targetName)
	}

	host, err := GetTargetHost(actx, targetName)
	if err != nil {
		return nil, err
	}

	node, err := GetNode(actx, act.Node)
	if err != nil {
		return nil, fmt.Errorf("nvme_connect: %w", err)
	}

	nqn := spec.NQN()
	port := spec.NvmePort
	if port == 0 {
		port = 4420
	}

	actx.Log("  nvme connect %s -> %s:%d nqn=%s", targetName, host, port, nqn)
	cmd := fmt.Sprintf("nvme connect -t tcp -n %s -a %s -s %d", nqn, host, port)
	stdout, stderr, code, err := node.RunRoot(ctx, cmd)
	if err != nil || code != 0 {
		// Treat "already connected" as success.
		if strings.Contains(stdout+stderr, "already connected") {
			actx.Log("  already connected")
			return map[string]string{"value": nqn}, nil
		}
		return nil, fmt.Errorf("nvme_connect: code=%d stdout=%s stderr=%s err=%v", code, stdout, stderr, err)
	}

	return map[string]string{"value": nqn}, nil
}

// nvmeDisconnect disconnects from an NVMe/TCP target.
// Params: target (required).
func nvmeDisconnect(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
	targetName := act.Target
	if targetName == "" {
		return nil, fmt.Errorf("nvme_disconnect: target is required")
	}

	spec, ok := actx.Scenario.Targets[targetName]
	if !ok {
		return nil, fmt.Errorf("nvme_disconnect: target %q not in scenario", targetName)
	}

	node, err := GetNode(actx, act.Node)
	if err != nil {
		return nil, fmt.Errorf("nvme_disconnect: %w", err)
	}

	nqn := spec.NQN()
	actx.Log("  nvme disconnect nqn=%s", nqn)
	cmd := fmt.Sprintf("nvme disconnect -n %s", nqn)
	stdout, stderr, code, err := node.RunRoot(ctx, cmd)
	if err != nil || code != 0 {
		outStr := stdout + stderr
		// Treat "not connected" / "no subsystem" as success (idempotent).
		if strings.Contains(outStr, "not connected") || strings.Contains(outStr, "No subsystemtype") || strings.Contains(outStr, "Invalid argument") {
			actx.Log("  already disconnected")
			return nil, nil
		}
		return nil, fmt.Errorf("nvme_disconnect: code=%d output=%s err=%v", code, outStr, err)
	}

	return nil, nil
}

// nvmeGetDevice finds the block device path for an NVMe/TCP connection.
// Params: target (required). Polls nvme list-subsys until device appears.
// Returns: value = /dev/nvmeXn1
func nvmeGetDevice(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
	targetName := act.Target
	if targetName == "" {
		return nil, fmt.Errorf("nvme_get_device: target is required")
	}

	spec, ok := actx.Scenario.Targets[targetName]
	if !ok {
		return nil, fmt.Errorf("nvme_get_device: target %q not in scenario", targetName)
	}

	node, err := GetNode(actx, act.Node)
	if err != nil {
		return nil, fmt.Errorf("nvme_get_device: %w", err)
	}

	nqn := spec.NQN()
	actx.Log("  waiting for NVMe device for nqn=%s ...", nqn)

	// Poll for up to 10 seconds.
	deadline := time.After(10 * time.Second)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline:
			return nil, fmt.Errorf("nvme_get_device: timeout waiting for device (nqn=%s)", nqn)
		case <-ticker.C:
			dev, findErr := findNVMeDevice(ctx, node, nqn)
			if findErr != nil {
				continue // retry
			}
			if dev != "" {
				actx.Log("  found device: %s", dev)
				return map[string]string{"value": dev}, nil
			}
		}
	}
}

// nvmeCleanup disconnects all NVMe/TCP subsystems matching our prefix.
func nvmeCleanup(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
	node, err := GetNode(actx, act.Node)
	if err != nil {
		return nil, fmt.Errorf("nvme_cleanup: %w", err)
	}

	cmd := "nvme disconnect-all 2>/dev/null || true"
	node.RunRoot(ctx, cmd)
	actx.Log("  nvme disconnect-all complete")
	return nil, nil
}

// findNVMeDevice resolves the merged namespace device path for the
// given NQN. Under native NVMe multipath both controllers share one
// namespace device that hangs off /sys/class/nvme-subsystem/<sub>/,
// not off either controller. Walk sysfs first; if that returns
// nothing (e.g. multipath disabled, single controller), fall back
// to deriving /dev/<path-name>n1 from the controller name.
//
// Returns "" when the NQN is not yet present (caller polls).
func findNVMeDevice(ctx context.Context, node *infra.Node, nqn string) (string, error) {
	cmd := "nvme list-subsys -o json 2>/dev/null"
	stdout, _, code, err := node.RunRoot(ctx, cmd)
	if err != nil || code != 0 {
		return "", fmt.Errorf("nvme list-subsys failed: code=%d err=%v", code, err)
	}

	view, parseErr := parseListSubsys(stdout)
	if parseErr != nil {
		return "", fmt.Errorf("nvme list-subsys parse: %w", parseErr)
	}

	sub := view.findByNQN(nqn)
	if sub == nil {
		return "", nil // NQN not yet present
	}

	// Preferred: sysfs walk gives the merged ns device name.
	devs, sysErr := nsDevicesViaSysfs(ctx, node, nqn)
	if sysErr == nil && len(devs) > 0 {
		return devs[0], nil
	}

	// Fallback: derive /dev/<controller>n1. Single-path topologies
	// produce the right answer; under multipath this is the bug we
	// fixed by preferring sysfs.
	for _, p := range sub.Paths {
		if p.Name == "" {
			continue
		}
		if strings.EqualFold(p.Transport, "tcp") && strings.EqualFold(p.State, "live") {
			return "/dev/" + p.Name + "n1", nil
		}
	}
	for _, p := range sub.Paths {
		if p.Name != "" {
			return "/dev/" + p.Name + "n1", nil
		}
	}
	return "", nil
}

// nsDevicesViaSysfs returns the namespace devices owned by the
// subsystem matching nqn, by walking /sys/class/nvme-subsystem.
// Empty result with nil error means "no subsystem matched" — the
// caller should not treat that as failure, just as "not yet."
func nsDevicesViaSysfs(ctx context.Context, node *infra.Node, nqn string) ([]string, error) {
	cmd := fmt.Sprintf(`
for d in /sys/class/nvme-subsystem/*/; do
  if [ "$(cat "$d/subsysnqn" 2>/dev/null)" = "%s" ]; then
    for entry in "$d"*; do
      base=$(basename "$entry")
      case "$base" in
        nvme[0-9]*n[0-9]*) echo "/dev/$base" ;;
      esac
    done
  fi
done`, nqn)
	stdout, _, code, err := node.RunRoot(ctx, cmd)
	if err != nil || code != 0 {
		return nil, fmt.Errorf("sysfs walk: code=%d err=%v", code, err)
	}
	var devs []string
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		devs = append(devs, line)
	}
	return devs, nil
}

// ListSubsysView is the structured result of parsing
// `nvme list-subsys -o json`. Pure-data, deterministic; safe for
// fixture-test round-trip.
type ListSubsysView struct {
	Subsystems []Subsystem `json:"subsystems"`
}

// Subsystem is one NVMe subsystem (post-merge), addressed by NQN
// and reachable through one or more controller Paths.
type Subsystem struct {
	Name     string `json:"name"`
	NQN      string `json:"nqn"`
	IOPolicy string `json:"io_policy,omitempty"`
	Paths    []Path `json:"paths"`
}

// Path is one controller route into a subsystem.
type Path struct {
	Name      string `json:"name"`
	Transport string `json:"transport"`
	Address   string `json:"address,omitempty"`
	State     string `json:"state,omitempty"`
}

// findByNQN returns the first subsystem matching nqn, or nil.
func (v *ListSubsysView) findByNQN(nqn string) *Subsystem {
	for i := range v.Subsystems {
		if v.Subsystems[i].NQN == nqn {
			return &v.Subsystems[i]
		}
	}
	return nil
}

// parseListSubsys parses the JSON output of `nvme list-subsys -o
// json`. Handles both the host-wrapper shape (top-level array of
// hosts, each with a Subsystems array) and the older flat shape
// (single object with Subsystems). Pure function; fixture-tested.
func parseListSubsys(stdout string) (*ListSubsysView, error) {
	stdout = strings.TrimSpace(stdout)
	if stdout == "" {
		return &ListSubsysView{}, nil
	}

	view := &ListSubsysView{}

	// Try host-wrapper shape first.
	var hosts []rawHost
	if err := json.Unmarshal([]byte(stdout), &hosts); err == nil {
		for _, h := range hosts {
			for _, raw := range h.Subsystems {
				view.Subsystems = append(view.Subsystems, raw.toSubsystem())
			}
		}
		return view, nil
	}

	// Flat shape (older nvme-cli).
	var single rawHost
	if err := json.Unmarshal([]byte(stdout), &single); err != nil {
		return nil, err
	}
	for _, raw := range single.Subsystems {
		view.Subsystems = append(view.Subsystems, raw.toSubsystem())
	}
	return view, nil
}

type rawHost struct {
	Subsystems []rawSubsystem `json:"Subsystems"`
}

type rawSubsystem struct {
	Name     string    `json:"Name"`
	NQN      string    `json:"NQN"`
	IOPolicy string    `json:"IOPolicy"`
	Paths    []rawPath `json:"Paths"`
}

type rawPath struct {
	Name      string `json:"Name"`
	Transport string `json:"Transport"`
	Address   string `json:"Address"`
	State     string `json:"State"`
}

func (r rawSubsystem) toSubsystem() Subsystem {
	out := Subsystem{
		Name:     r.Name,
		NQN:      r.NQN,
		IOPolicy: r.IOPolicy,
	}
	for _, p := range r.Paths {
		out.Paths = append(out.Paths, Path{
			Name:      p.Name,
			Transport: p.Transport,
			Address:   p.Address,
			State:     p.State,
		})
	}
	return out
}
