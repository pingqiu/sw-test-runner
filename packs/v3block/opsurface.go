package v3block

// Helpers on the operator surface shared by the typed operations: the
// blockvolume status read, the kernel NVMe/TCP host (through its fabrics
// interface, no nvme-cli), the plain-detach answer, the filer listing and
// process checks by recorded pid. They observe and report; they decide no
// outcome of an operation.

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/pingqiu/sw-test-runner/declared"
)

// run runs one command and returns stdout, stderr, the exit code and
// whether the command could not be run at all.
type runner interface {
	Run(ctx context.Context, cmd string) (string, string, int, error)
	RunRoot(ctx context.Context, cmd string) (string, string, int, error)
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// vsStatus is the part of blockvolume's /status the actions read.
type vsStatus struct {
	VolumeID             string
	Epoch                uint64
	Healthy              bool
	AuthorityRole        string
	FrontendPrimaryReady bool
}

// serving: this host serves vol as primary at a non-zero epoch with its
// frontend ready.
func (st vsStatus) serving(vol string) bool {
	return st.VolumeID == vol && st.Healthy && st.AuthorityRole == "primary" && st.FrontendPrimaryReady && st.Epoch > 0
}

func readStatus(ctx context.Context, node runner, statusAddr, vol string) (vsStatus, bool) {
	stdout, _, code, err := node.Run(ctx, fmt.Sprintf("curl -s -m 2 %s", shellQuote("http://"+statusAddr+"/status?volume="+vol)))
	var st vsStatus
	if err != nil || code != 0 || json.Unmarshal([]byte(stdout), &st) != nil {
		return st, false
	}
	return st, true
}

// nvmeConnect connects the kernel NVMe/TCP host to nqn at host:port through
// the kernel's fabrics interface (declared.NVMeConnect).
func nvmeConnect(ctx context.Context, node runner, host, port, nqn, extra string) string {
	return declared.NVMeConnect(ctx, node, host, port, nqn, extra)
}

// nvmeDisconnect deletes every kernel controller of subsystem nqn through
// sysfs (what `nvme disconnect -n` does). It certifies the disconnect only
// from a successful scan that finds no controller of nqn: a failed delete, or
// a scan that cannot read sysfs, is not "disconnected".
func nvmeDisconnect(ctx context.Context, node runner, nqn string) string {
	_, why := nvmeDisconnectCount(ctx, node, nqn)
	return why
}

// nvmeDisconnectCount is nvmeDisconnect, also answering how many
// controllers it deleted (-1 when that is not known).
func nvmeDisconnectCount(ctx context.Context, node runner, nqn string) (int, string) {
	cmd := fmt.Sprintf(`n=0; for d in /sys/class/nvme/nvme*; do [ -e "$d" ] || continue; q=$(cat "$d/subsysnqn") || exit 3; [ "$q" = %s ] || continue; echo 1 > "$d/delete_controller" || exit 4; n=$((n+1)); done; echo "deleted $n"`, shellQuote(nqn))
	stdout, stderr, code, err := node.RunRoot(ctx, cmd)
	if err != nil || code != 0 {
		return -1, fmt.Sprintf("nvme disconnect: exit %d: %s", code, firstLine(stderr))
	}
	n := -1
	if f := strings.Fields(stdout); len(f) == 2 && f[0] == "deleted" {
		n, _ = strconv.Atoi(f[1])
	}
	why := nvmeConfirmGone(ctx, node, nqn)
	return n, why
}

// nvmeConfirmGone waits until a scan finds no controller of nqn.
func nvmeConfirmGone(ctx context.Context, node runner, nqn string) string {
	end := time.Now().Add(5 * time.Second)
	for {
		state, _, why := nvmeScan(ctx, node, nqn)
		switch {
		case state == nvmeAbsent:
			return ""
		case state == nvmeUnreadable:
			return "nvme disconnect: cannot confirm: " + why
		case time.Now().After(end):
			return "nvme disconnect: a controller of " + nqn + " remains"
		}
		if !pause(ctx, 250*time.Millisecond) {
			return "nvme disconnect: a controller of " + nqn + " remains at the runner deadline"
		}
	}
}

// What a scan of the kernel's NVMe controllers says about one subsystem
// (declared.NVMeScan).
const (
	nvmeAbsent     = declared.NVMeAbsent
	nvmeOne        = declared.NVMeOne
	nvmeIncomplete = declared.NVMeIncomplete
	nvmeUnreadable = declared.NVMeUnreadable
)

// nvmeScan reads every controller's subsystem NQN and the namespaces of the
// ones that match (declared.NVMeScan).
func nvmeScan(ctx context.Context, node runner, nqn string) (state, dev, why string) {
	return declared.NVMeScan(ctx, node, nqn)
}

// deviceOf waits until the subsystem has exactly one controller with one
// namespace and returns its device (declared.NVMeDevice): no I/O goes to an
// identity the runner could not establish.
func deviceOf(ctx context.Context, node runner, nqn string, bound time.Duration) (string, string) {
	return declared.NVMeDevice(ctx, node, nqn, bound)
}

// listenerPIDs maps each port to the pids holding a TCP listener on it
// (declared.ListenerPIDs): a failed read is an error, never "nobody
// listens".
func listenerPIDs(ctx context.Context, node runner, ports []string) (map[string][]string, error) {
	return declared.ListenerPIDs(ctx, node, ports)
}

// A process the runner started is identified by its pid AND its start time
// (declared.ProcStart): a pid alone may be recycled by another process after
// the first one exits.
const (
	procRunning    = declared.ProcRunning
	procGone       = declared.ProcGone
	procUnreadable = declared.ProcUnreadable
)

// procStart reads a pid's start time (declared.ProcStart).
func procStart(ctx context.Context, node runner, pid string) (string, string) {
	return declared.ProcStart(ctx, node, pid)
}

// procStartCmd is the shell that reads a pid's start time
// (declared.ProcStartCmd).
func procStartCmd(proc, pid string) string { return declared.ProcStartCmd(proc, pid) }

// procState says whether the process (pid, start) still runs
// (declared.ProcState).
func procState(ctx context.Context, node runner, pid, start string) string {
	return declared.ProcState(ctx, node, pid, start)
}

// alive reports whether the process (pid, start) runs; an unreadable check is
// not "alive".
func alive(ctx context.Context, node runner, pid, start string) bool {
	return procState(ctx, node, pid, start) == procRunning
}

// waitGone waits until the process (pid, start) is gone. It certifies the exit
// only from a successful check that finds it gone: a check that keeps failing
// is reported, not taken for an exit.
func waitGone(ctx context.Context, node runner, pid, start string, bound time.Duration) string {
	return declared.WaitGone(ctx, node, pid, start, bound)
}

// parseDetach reads the sealed generation of a plain detach: the JSON answer
// on stdout and exactly "sealed HEAD generation: G" on stderr, both naming
// the same generation, for this volume at this epoch.
func parseDetach(stdout, stderr, vol string, epoch uint64) (uint64, error) {
	var a struct {
		VolumeID  string `json:"volumeID"`
		HeadGen   uint64 `json:"headGen"`
		HeadEpoch uint64 `json:"headEpoch"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &a); err != nil {
		return 0, fmt.Errorf("detach answer is not JSON: %q", firstLine(stdout))
	}
	if a.VolumeID != vol || a.HeadGen == 0 || a.HeadEpoch != epoch {
		return 0, fmt.Errorf("detach answer %+v; want %s at epoch %d with a generation", a, vol, epoch)
	}
	if strings.TrimSpace(stderr) != fmt.Sprintf("sealed HEAD generation: %d", a.HeadGen) {
		return 0, fmt.Errorf("detach stderr %q does not state generation %d", firstLine(stderr), a.HeadGen)
	}
	return a.HeadGen, nil
}
