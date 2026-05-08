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
	r.RegisterFunc("nvme_id_ctrl", tr.TierBlock, nvmeIdCtrl)
	r.RegisterFunc("nvme_id_ns", tr.TierBlock, nvmeIdNs)
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

// ============================================================
// nvme_id_ctrl / nvme_id_ns
// ============================================================

// IdCtrl is the parsed Identify Controller view exposing the fields
// our assertions care about. Spec field names are preserved.
type IdCtrl struct {
	VID       uint16 `json:"vid"`
	SSVID     uint16 `json:"ssvid"`
	SN        string `json:"sn"`
	MN        string `json:"mn"`
	FR        string `json:"fr"`
	CMIC      uint8  `json:"cmic"`
	MDTS      uint8  `json:"mdts"`
	CNTLID    uint16 `json:"cntlid"`
	Ver       uint32 `json:"ver"`
	OACS      uint16 `json:"oacs"`
	ANATT     uint8  `json:"anatt"`
	ANACAP    uint8  `json:"anacap"`
	ANAGRPMAX uint32 `json:"anagrpmax"`
	NANAGRPID uint32 `json:"nanagrpid"`
	NN        uint32 `json:"nn"`
	SubNQN    string `json:"subnqn"`
}

// IdNs is the parsed Identify Namespace view.
type IdNs struct {
	NSZE     uint64 `json:"nsze"`
	NCAP     uint64 `json:"ncap"`
	NUSE     uint64 `json:"nuse"`
	NSFEAT   uint8  `json:"nsfeat"`
	NLBAF    uint8  `json:"nlbaf"`
	FLBAS    uint8  `json:"flbas"`
	NMIC     uint8  `json:"nmic"`
	ANAGRPID uint32 `json:"anagrpid"`
	NGUID    string `json:"nguid"`
	EUI64    string `json:"eui64"`
}

// parseIdCtrl parses the JSON output of `nvme id-ctrl -o json`.
// Trims trailing whitespace on string fields (sn/mn/fr are space-
// padded to fixed widths in the wire format).
func parseIdCtrl(stdout string) (*IdCtrl, error) {
	stdout = strings.TrimSpace(stdout)
	if stdout == "" {
		return nil, fmt.Errorf("parseIdCtrl: empty input")
	}
	var c IdCtrl
	if err := json.Unmarshal([]byte(stdout), &c); err != nil {
		return nil, fmt.Errorf("parseIdCtrl: %w", err)
	}
	c.SN = strings.TrimSpace(c.SN)
	c.MN = strings.TrimSpace(c.MN)
	c.FR = strings.TrimSpace(c.FR)
	c.SubNQN = strings.TrimSpace(c.SubNQN)
	return &c, nil
}

// parseIdNs parses the JSON output of `nvme id-ns -o json`.
func parseIdNs(stdout string) (*IdNs, error) {
	stdout = strings.TrimSpace(stdout)
	if stdout == "" {
		return nil, fmt.Errorf("parseIdNs: empty input")
	}
	var n IdNs
	if err := json.Unmarshal([]byte(stdout), &n); err != nil {
		return nil, fmt.Errorf("parseIdNs: %w", err)
	}
	return &n, nil
}

// nvmeIdCtrl runs `nvme id-ctrl -o json <dev>` on the node and returns
// the parsed IdCtrl. The device path is taken from params.dev or
// resolved via the target's NQN.
//
// Params:
//
//	target: optional, used for sysfs device resolution if dev not set
//	dev:    optional, controller device path (e.g. /dev/nvme1)
//	node:   optional, defaults to local
//
// Returns: value = JSON-marshalled IdCtrl
func nvmeIdCtrl(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
	dev, err := resolveNVMeCtrlDevice(ctx, actx, act)
	if err != nil {
		return nil, fmt.Errorf("nvme_id_ctrl: %w", err)
	}
	node, err := GetNode(actx, act.Node)
	if err != nil {
		return nil, fmt.Errorf("nvme_id_ctrl: %w", err)
	}
	cmd := fmt.Sprintf("nvme id-ctrl -o json %s 2>/dev/null", dev)
	stdout, stderr, code, err := node.RunRoot(ctx, cmd)
	if err != nil || code != 0 {
		return nil, fmt.Errorf("nvme_id_ctrl: code=%d stderr=%s err=%v", code, stderr, err)
	}
	parsed, err := parseIdCtrl(stdout)
	if err != nil {
		return nil, fmt.Errorf("nvme_id_ctrl: parse: %w", err)
	}
	out, err := json.Marshal(parsed)
	if err != nil {
		return nil, fmt.Errorf("nvme_id_ctrl: marshal: %w", err)
	}
	actx.Log("  id-ctrl %s: cmic=%d cntlid=%d nn=%d subnqn=%s",
		dev, parsed.CMIC, parsed.CNTLID, parsed.NN, parsed.SubNQN)
	return map[string]string{"value": string(out)}, nil
}

// nvmeIdNs runs `nvme id-ns -o json <ns_dev>` on the node and returns
// the parsed IdNs.
//
// Params:
//
//	target: optional, used for sysfs device resolution if dev not set
//	dev:    optional, namespace device path (e.g. /dev/nvme1n1)
//	node:   optional, defaults to local
//
// Returns: value = JSON-marshalled IdNs
func nvmeIdNs(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
	dev, err := resolveNVMeNSDevice(ctx, actx, act)
	if err != nil {
		return nil, fmt.Errorf("nvme_id_ns: %w", err)
	}
	node, err := GetNode(actx, act.Node)
	if err != nil {
		return nil, fmt.Errorf("nvme_id_ns: %w", err)
	}
	cmd := fmt.Sprintf("nvme id-ns -o json %s 2>/dev/null", dev)
	stdout, stderr, code, err := node.RunRoot(ctx, cmd)
	if err != nil || code != 0 {
		return nil, fmt.Errorf("nvme_id_ns: code=%d stderr=%s err=%v", code, stderr, err)
	}
	parsed, err := parseIdNs(stdout)
	if err != nil {
		return nil, fmt.Errorf("nvme_id_ns: parse: %w", err)
	}
	out, err := json.Marshal(parsed)
	if err != nil {
		return nil, fmt.Errorf("nvme_id_ns: marshal: %w", err)
	}
	actx.Log("  id-ns %s: nsze=%d nmic=%d anagrpid=%d nguid=%s",
		dev, parsed.NSZE, parsed.NMIC, parsed.ANAGRPID, parsed.NGUID)
	return map[string]string{"value": string(out)}, nil
}

// resolveNVMeCtrlDevice picks a controller device path: explicit
// params.dev wins; otherwise the first live path under the target's
// NQN. Empty string is an error (caller cannot proceed).
func resolveNVMeCtrlDevice(ctx context.Context, actx *tr.ActionContext, act tr.Action) (string, error) {
	if dev, ok := act.Params["dev"]; ok && dev != "" {
		return dev, nil
	}
	if act.Target == "" {
		return "", fmt.Errorf("either params.dev or target is required")
	}
	spec, ok := actx.Scenario.Targets[act.Target]
	if !ok {
		return "", fmt.Errorf("target %q not in scenario", act.Target)
	}
	node, err := GetNode(actx, act.Node)
	if err != nil {
		return "", err
	}
	stdout, _, _, _ := node.RunRoot(ctx, "nvme list-subsys -o json 2>/dev/null")
	view, err := parseListSubsys(stdout)
	if err != nil {
		return "", fmt.Errorf("list-subsys parse: %w", err)
	}
	sub := view.findByNQN(spec.NQN())
	if sub == nil {
		return "", fmt.Errorf("NQN %q not present", spec.NQN())
	}
	for _, p := range sub.Paths {
		if p.Name != "" && strings.EqualFold(p.State, "live") {
			return "/dev/" + p.Name, nil
		}
	}
	return "", fmt.Errorf("no live controller path for %s", spec.NQN())
}

// resolveNVMeNSDevice picks a namespace device path: explicit
// params.dev wins; otherwise the merged ns device under the
// subsystem matching the target's NQN.
func resolveNVMeNSDevice(ctx context.Context, actx *tr.ActionContext, act tr.Action) (string, error) {
	if dev, ok := act.Params["dev"]; ok && dev != "" {
		return dev, nil
	}
	if act.Target == "" {
		return "", fmt.Errorf("either params.dev or target is required")
	}
	spec, ok := actx.Scenario.Targets[act.Target]
	if !ok {
		return "", fmt.Errorf("target %q not in scenario", act.Target)
	}
	node, err := GetNode(actx, act.Node)
	if err != nil {
		return "", err
	}
	dev, err := findNVMeDevice(ctx, node, spec.NQN())
	if err != nil {
		return "", err
	}
	if dev == "" {
		return "", fmt.Errorf("no namespace device for %s", spec.NQN())
	}
	return dev, nil
}
