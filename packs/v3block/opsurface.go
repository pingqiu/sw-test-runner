package v3block

// Helpers on the operator surface shared by the typed operations: the
// blockvolume status read, the kernel NVMe/TCP host (through its fabrics
// interface, no nvme-cli), the plain-detach answer, the filer listing and
// process checks by recorded pid. They observe and report; they decide no
// outcome of an operation.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
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
// the kernel's fabrics interface, the call nvme-cli makes (no tool needed).
func nvmeConnect(ctx context.Context, node runner, host, port, nqn, extra string) string {
	if !regexp.MustCompile(`^[0-9.]+$`).MatchString(host) || !regexp.MustCompile(`^[0-9]+$`).MatchString(port) || !regexp.MustCompile(`^[A-Za-z0-9.:_-]+$`).MatchString(nqn) {
		return "nvme connect: malformed address or nqn"
	}
	if !regexp.MustCompile(`^(,[a-z_]+=-?[0-9]+)*$`).MatchString(extra) {
		return "nvme connect: malformed options " + extra
	}
	opts := fmt.Sprintf("transport=tcp,traddr=%s,trsvcid=%s,nqn=%s%s", host, port, nqn, extra)
	if _, stderr, code, err := node.RunRoot(ctx, fmt.Sprintf("printf %%s %s > /dev/nvme-fabrics", shellQuote(opts))); err != nil || code != 0 {
		return "nvme connect: " + firstLine(stderr)
	}
	return ""
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

// What a scan of the kernel's NVMe controllers says about one subsystem.
const (
	nvmeAbsent     = "absent"     // no controller of the subsystem: a scan that read everything
	nvmeOne        = "one"        // one controller with exactly one namespace: its device
	nvmeIncomplete = "incomplete" // a controller without exactly one namespace (still coming up, or not ours)
	nvmeUnreadable = "unreadable" // the scan failed: nothing may be concluded
)

// nvmeScan reads every controller's subsystem NQN and the namespaces of the
// ones that match. A read error, or a command that fails, is unreadable:
// absent and present are concluded only from a scan that completed.
func nvmeScan(ctx context.Context, node runner, nqn string) (state, dev, why string) {
	cmd := fmt.Sprintf(`for d in /sys/class/nvme/nvme*; do [ -e "$d" ] || continue; q=$(cat "$d/subsysnqn") || exit 3; [ "$q" = %s ] || continue; echo "ctrl $(basename "$d")"; for n in "$d"/nvme*n*; do [ -e "$n" ] && echo "ns $(basename "$n")"; done; done; echo end`, shellQuote(nqn))
	stdout, stderr, code, err := node.Run(ctx, cmd)
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if err != nil || code != 0 || len(lines) == 0 || lines[len(lines)-1] != "end" {
		return nvmeUnreadable, "", fmt.Sprintf("sysfs scan exit %d: %s", code, firstLine(stderr))
	}
	var ctrls, nss []string
	for _, l := range lines[:len(lines)-1] {
		switch {
		case strings.HasPrefix(l, "ctrl "):
			ctrls = append(ctrls, l[5:])
		case strings.HasPrefix(l, "ns "):
			nss = append(nss, regexp.MustCompile(`c[0-9]+n`).ReplaceAllString(l[3:], "n"))
		default:
			return nvmeUnreadable, "", "sysfs scan printed " + firstLine(l)
		}
	}
	switch {
	case len(ctrls) == 0:
		return nvmeAbsent, "", ""
	case len(ctrls) == 1 && len(nss) == 1:
		return nvmeOne, "/dev/" + nss[0], ""
	}
	return nvmeIncomplete, "", fmt.Sprintf("subsystem %s: %d controllers, %d namespaces", nqn, len(ctrls), len(nss))
}

// deviceOf waits until the subsystem has exactly one controller with one
// namespace and returns its device. An unreadable scan or a subsystem that
// never becomes exactly one device refuses: no I/O goes to an identity the
// runner could not establish.
func deviceOf(ctx context.Context, node runner, nqn string, bound time.Duration) (string, string) {
	end := time.Now().Add(bound)
	for {
		state, dev, why := nvmeScan(ctx, node, nqn)
		if state == nvmeOne {
			return dev, ""
		}
		if state == nvmeUnreadable {
			return "", why
		}
		if time.Now().After(end) {
			if state == nvmeAbsent {
				return "", "no controller of " + nqn
			}
			return "", why
		}
		if !pause(ctx, 250*time.Millisecond) {
			return "", "no single device of " + nqn + " by the runner deadline"
		}
	}
}

// listenerPIDs maps each port to the pids holding a TCP listener on it, from
// one `ss -ltnp` read (root sees every process). A failed read is an error,
// never "nobody listens".
func listenerPIDs(ctx context.Context, node runner, ports []string) (map[string][]string, error) {
	stdout, stderr, code, err := node.RunRoot(ctx, "ss -Hltnp")
	if err != nil || code != 0 {
		return nil, fmt.Errorf("ss exit %d: %s", code, firstLine(stderr))
	}
	out := map[string][]string{}
	want := map[string]bool{}
	for _, p := range ports {
		want[p] = true
	}
	pidRe := regexp.MustCompile(`pid=([0-9]+)`)
	for _, l := range strings.Split(stdout, "\n") {
		f := strings.Fields(l)
		if len(f) < 4 {
			continue
		}
		local := f[3]
		port := local[strings.LastIndex(local, ":")+1:]
		if !want[port] {
			continue
		}
		pids := pidRe.FindAllStringSubmatch(l, -1)
		if len(pids) == 0 {
			out[port] = append(out[port], "?") // held by a process ss could not name
		}
		for _, m := range pids {
			out[port] = append(out[port], m[1])
		}
	}
	return out, nil
}

// blockSHA is the sha256 of the block a write of tag puts down, computed here
// so a write whose outcome is unknown still names the value it attempted.
func blockSHA(tag string) string {
	b := []byte(strings.Repeat(tag, 4096/len(tag)+1))[:4096]
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// A process the runner started is identified by its pid AND its start time
// (field 22 of /proc/PID/stat): a pid alone may be recycled by another
// process after the first one exits.
const (
	procRunning    = "running"    // the pid runs with the recorded start time
	procGone       = "gone"       // no such pid, or a different process now holds it
	procUnreadable = "unreadable" // the check failed: nothing may be concluded
)

// procStart reads a pid's start time; "" with procGone when the pid does not
// exist, procUnreadable when the read failed.
func procStart(ctx context.Context, node runner, pid string) (string, string) {
	if !regexp.MustCompile(`^[0-9]+$`).MatchString(pid) {
		return "", procGone
	}
	stdout, _, code, err := node.Run(ctx, procStartCmd("/proc", pid))
	out := strings.TrimSpace(stdout)
	switch {
	case err != nil || code != 0:
		return "", procUnreadable
	case out == "absent":
		return "", procGone
	case regexp.MustCompile(`^start [0-9]+$`).MatchString(out):
		return out[6:], procRunning
	}
	return "", procUnreadable
}

// procStartCmd is the shell that reads a pid's start time: field 22 of
// PROC/PID/stat. The command name (field 2) may hold spaces and parentheses,
// so the text is cut after the LAST ')'; the start time is then the 20th
// remaining field, which the shell must write ${20} ($20 is ${2} then "0").
// It prints "start N", "absent" for no such pid, or fails (exit 3) when the
// pid exists but its stat cannot be read.
func procStartCmd(proc, pid string) string {
	return fmt.Sprintf(`if [ -e %[1]s/%[2]s ]; then s=$(cat %[1]s/%[2]s/stat) || exit 3; s=${s##*) }; set -- $s; [ -n "${20}" ] || exit 3; echo "start ${20}"; else echo absent; fi`, proc, pid)
}

// procState says whether the process (pid, start) still runs.
func procState(ctx context.Context, node runner, pid, start string) string {
	if start == "" {
		return procUnreadable // no identity was recorded: nothing may be concluded
	}
	got, state := procStart(ctx, node, pid)
	if state == procRunning && got != start {
		return procGone // the pid now names another process
	}
	return state
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
	end := time.Now().Add(bound)
	last := ""
	for {
		switch last = procState(ctx, node, pid, start); last {
		case procGone:
			return ""
		}
		if time.Now().After(end) {
			if last == procUnreadable {
				return fmt.Sprintf("pid %s: its state could not be read within %s", pid, bound)
			}
			return fmt.Sprintf("pid %s still running after %s", pid, bound)
		}
		if !pause(ctx, 250*time.Millisecond) {
			return fmt.Sprintf("pid %s: not gone by the runner deadline", pid)
		}
	}
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
