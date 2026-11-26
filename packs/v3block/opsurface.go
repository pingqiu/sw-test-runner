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
func nvmeConnect(ctx context.Context, node runner, host, port, nqn string) string {
	if !regexp.MustCompile(`^[0-9.]+$`).MatchString(host) || !regexp.MustCompile(`^[0-9]+$`).MatchString(port) || !regexp.MustCompile(`^[A-Za-z0-9.:_-]+$`).MatchString(nqn) {
		return "nvme connect: malformed address or nqn"
	}
	opts := fmt.Sprintf("transport=tcp,traddr=%s,trsvcid=%s,nqn=%s", host, port, nqn)
	if _, stderr, code, err := node.RunRoot(ctx, fmt.Sprintf("printf %%s %s > /dev/nvme-fabrics", shellQuote(opts))); err != nil || code != 0 {
		return "nvme connect: " + firstLine(stderr)
	}
	return ""
}

// nvmeDisconnect deletes every kernel controller of subsystem nqn through
// sysfs (what `nvme disconnect -n` does) and confirms none remains.
func nvmeDisconnect(ctx context.Context, node runner, nqn string) string {
	cmd := fmt.Sprintf(`for d in /sys/class/nvme/nvme*; do [ "$(cat "$d/subsysnqn" 2>/dev/null)" = %s ] && echo 1 > "$d/delete_controller"; done; true`, shellQuote(nqn))
	if _, stderr, code, err := node.RunRoot(ctx, cmd); err != nil || code != 0 {
		return "nvme disconnect: " + firstLine(stderr)
	}
	if _, why := deviceOf(ctx, node, nqn, 500*time.Millisecond); why == "" {
		return "nvme disconnect: a controller of " + nqn + " remains"
	}
	return ""
}

// deviceOf finds the block device of the connected subsystem nqn in sysfs.
func deviceOf(ctx context.Context, node runner, nqn string, bound time.Duration) (string, string) {
	cmd := fmt.Sprintf(`for d in /sys/class/nvme/nvme*; do [ "$(cat "$d/subsysnqn" 2>/dev/null)" = %s ] || continue; for n in "$d"/nvme*n*; do [ -e "$n" ] && basename "$n"; done; done | sed -E 's/c[0-9]+n/n/' | sort -u`, shellQuote(nqn))
	end := time.Now().Add(bound)
	for time.Now().Before(end) {
		stdout, _, _, _ := node.Run(ctx, cmd)
		if devs := strings.Fields(stdout); len(devs) == 1 {
			return "/dev/" + devs[0], ""
		} else if len(devs) > 1 {
			return "", fmt.Sprintf("subsystem %s has %d namespaces", nqn, len(devs))
		}
		time.Sleep(250 * time.Millisecond)
	}
	return "", "no block device for " + nqn
}

// blockSHA is the sha256 of the block a write of tag puts down, computed here
// so a write whose outcome is unknown still names the value it attempted.
func blockSHA(tag string) string {
	b := []byte(strings.Repeat(tag, 4096/len(tag)+1))[:4096]
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// alive reports whether the recorded pid still runs.
func alive(ctx context.Context, node runner, pid string) bool {
	if !regexp.MustCompile(`^[0-9]+$`).MatchString(pid) {
		return false
	}
	out, _, _, _ := node.Run(ctx, "kill -0 "+pid+" 2>/dev/null && echo alive")
	return strings.TrimSpace(out) == "alive"
}

func waitGone(ctx context.Context, node runner, pid string, bound time.Duration) string {
	end := time.Now().Add(bound)
	for time.Now().Before(end) {
		if out, _, _, _ := node.Run(ctx, "kill -0 "+pid+" 2>/dev/null && echo alive"); strings.TrimSpace(out) != "alive" {
			return ""
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Sprintf("pid %s still running after %s", pid, bound)
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

