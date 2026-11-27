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
// controllers it deleted (declared.NVMeDisconnectCount).
func nvmeDisconnectCount(ctx context.Context, node runner, nqn string) (int, string) {
	return declared.NVMeDisconnectCount(ctx, node, nqn)
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
