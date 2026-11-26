// Package cloud holds the controller-side pieces of a cloud VM power fault:
// the audited az argv, the instance-view parser, the run's pack context
// (inventory, managed resources, SSH identity) and the node's boot id read
// over its management link. The fault's rules and custody are the runner's
// ApplyWitnessedPowerFault; this package only supplies its calls.
package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"

	tr "github.com/pingqiu/sw-test-runner"
)

// vmID is the only shape of resource id these calls accept: one virtual
// machine of one resource group (subscription a UUID, the group as the
// audited RG rule, the name as Azure's VM name rule).
var vmID = regexp.MustCompile(`^/subscriptions/[0-9a-fA-F-]{36}/resourceGroups/[A-Za-z0-9_-]{1,90}/providers/Microsoft\.Compute/virtualMachines/[A-Za-z0-9_.-]{1,64}$`)

// cmdUnsafe are the characters the Windows az.cmd wrapper would interpret
// (provision/smoke/AZURE-ARGV-AUDIT.md): no argument may carry one.
const cmdUnsafe = "|&<>^%\"'"

// The three az calls of a power fault, as plain argv lists.
func powerOffArgv(id string) []string {
	return []string{"vm", "stop", "--ids", id, "--skip-shutdown", "--only-show-errors"}
}
func powerOnArgv(id string) []string {
	return []string{"vm", "start", "--ids", id, "--only-show-errors"}
}
func viewArgv(id string) []string {
	return []string{"vm", "get-instance-view", "--ids", id, "--output", "json", "--only-show-errors"}
}

// checkArgv refuses a resource id of any other shape, and any argument that
// carries a character the Windows wrapper would interpret.
func checkArgv(id string, argv []string) error {
	if !vmID.MatchString(id) {
		return fmt.Errorf("az: %q is not a virtual machine resource id", id)
	}
	for _, a := range argv {
		if strings.ContainsAny(a, cmdUnsafe) {
			return fmt.Errorf("az: argument %q carries a character the wrapper would interpret", a)
		}
	}
	return nil
}

// Az runs one az call: argv only, no shell, bounded by ctx. It is a variable
// so the controls run without Azure.
var Az = func(ctx context.Context, argv []string) (stdout, stderr string, err error) {
	cmd := exec.CommandContext(ctx, "az", argv...)
	cmd.WaitDelay = 3 * time.Second
	var out, errOut strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err = cmd.Run()
	return out.String(), errOut.String(), err
}

func run(ctx context.Context, id string, argv []string) (string, error) {
	if err := checkArgv(id, argv); err != nil {
		return "", err
	}
	out, errOut, err := Az(ctx, argv)
	if err != nil {
		return out, fmt.Errorf("az %s: %w: %s", argv[0]+" "+argv[1], err, firstLine(errOut))
	}
	return out, nil
}

// PowerOff stops the VM without a guest shutdown (a power cut, not a halt).
func PowerOff(ctx context.Context, id string) error {
	_, err := run(ctx, id, powerOffArgv(id))
	return err
}

// PowerOn starts the VM.
func PowerOn(ctx context.Context, id string) error {
	_, err := run(ctx, id, powerOnArgv(id))
	return err
}

// View reads the VM's instance view: its vmId and its power state.
func View(ctx context.Context, id string) (tr.VMPowerView, error) {
	out, err := run(ctx, id, viewArgv(id))
	if err != nil {
		return tr.VMPowerView{}, err
	}
	return parseView([]byte(out))
}

// parseView reads vmId and the PowerState/* status of an instance view. A
// missing vmId, no power state, or two power states is an error: the fault
// never acts on an identity or a state it could not read exactly.
func parseView(raw []byte) (tr.VMPowerView, error) {
	var doc struct {
		VMID         string `json:"vmId"`
		InstanceView *struct {
			Statuses []struct {
				Code string `json:"code"`
			} `json:"statuses"`
		} `json:"instanceView"`
		Statuses []struct {
			Code string `json:"code"`
		} `json:"statuses"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return tr.VMPowerView{}, fmt.Errorf("instance view: %w", err)
	}
	statuses := doc.Statuses
	if doc.InstanceView != nil {
		statuses = append(statuses, doc.InstanceView.Statuses...)
	}
	var power []string
	for _, s := range statuses {
		if p, ok := strings.CutPrefix(s.Code, "PowerState/"); ok {
			power = append(power, p)
		}
	}
	switch {
	case doc.VMID == "":
		return tr.VMPowerView{}, errors.New("instance view: no vmId")
	case len(power) != 1 || power[0] == "":
		return tr.VMPowerView{}, fmt.Errorf("instance view: %d power states %v", len(power), power)
	}
	return tr.VMPowerView{VMID: doc.VMID, PowerState: power[0]}, nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}
