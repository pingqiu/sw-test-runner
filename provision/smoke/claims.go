package smoke

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// RG claims (#140): which run owns which resource group, independent of
// where its output directory lives. A run takes the claim of its
// subscription+RG before it creates its output directory and releases it
// only when its outcome is clean (or it never started an apply, so it cannot
// own anything in the RG). A dead controller does not release its claim: the
// RG may hold live resources; cleanup (destroy) reaching a clean outcome
// does. Two runs on different RGs proceed concurrently; a second run on a
// claimed RG is refused RG_CLAIMED.
type rgClaim struct {
	Schema        string `json:"schema"`
	Subscription  string `json:"subscription"`
	ResourceGroup string `json:"resource_group"`
	Run           string `json:"run"`
	Output        string `json:"output"`
	Host          string `json:"host"`
	PID           int    `json:"pid"`
	ProcStart     string `json:"proc_start"` // /proc/<pid>/stat start time (Linux), defeats pid reuse
	CreatedAt     string `json:"created_at"`
	LaunchToken   string `json:"launch_token"`
}

// DefaultClaimsDir is the controller's claims directory,
// $HOME/.local/state/seaweed-cloud/claims. The CLI always sets
// Controller.ClaimsDir to it; an empty ClaimsDir (library use, unit tests of
// other behaviour) takes no claims.
func DefaultClaimsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "seaweed-cloud", "claims"), nil
}

var claimPartRE = regexp.MustCompile(`^[a-z0-9._()-]{1,90}$`)

// claimPath is the claim file of subscription+RG (Azure names are
// case-insensitive, so the key is lower case).
func claimPath(dir, subscription, group string) (string, error) {
	s, g := strings.ToLower(subscription), strings.ToLower(group)
	if !claimPartRE.MatchString(s) || !claimPartRE.MatchString(g) {
		return "", fmt.Errorf("subscription/resource group %q/%q cannot name a claim", subscription, group)
	}
	return filepath.Join(dir, s+"__"+g+".json"), nil
}

func readClaim(p string) (rgClaim, error) {
	var c rgClaim
	b, err := os.ReadFile(p)
	if err == nil {
		err = StrictJSON(b, &c)
	}
	return c, err
}

// outputSettled reports whether a run's output can no longer own cloud
// resources: its outcome is clean, or it never started an apply.
func outputSettled(output string) bool {
	if _, err := os.Stat(filepath.Join(output, "apply-started")); os.IsNotExist(err) {
		return true
	}
	var o runOutcome
	b, err := os.ReadFile(filepath.Join(output, "outcome.json"))
	return err == nil && StrictJSON(b, &o) == nil && o.clean()
}

// claimLive reports whether the claim's controller is still running: same
// host, the pid exists and its start time matches. Another host is never
// judged dead.
func claimLive(c rgClaim) (live bool, why string) {
	host, _ := os.Hostname()
	if c.Host != host {
		return true, "held on another host " + c.Host
	}
	start, err := procStart(c.PID)
	if err != nil {
		return false, "pid " + strconv.Itoa(c.PID) + " is gone"
	}
	if c.ProcStart != "" && start != c.ProcStart {
		return false, "pid " + strconv.Itoa(c.PID) + " was reused"
	}
	return true, "pid " + strconv.Itoa(c.PID) + " is alive"
}

// takeClaim claims subscription+RG for the run writing output. A claim held
// by another output is replaced only when that output is settled (clean, or
// it never applied) and its controller is not live; otherwise RG_CLAIMED.
func takeClaim(dir, subscription, group, output, token string) (string, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	p, err := claimPath(dir, subscription, group)
	if err != nil {
		return "", err
	}
	host, _ := os.Hostname()
	start, _ := procStart(os.Getpid())
	rec, _ := json.Marshal(rgClaim{Schema: "seaweed.cloud-claim/v1", Subscription: subscription, ResourceGroup: group, Run: filepath.Base(output),
		Output: filepath.Clean(output), Host: host, PID: os.Getpid(), ProcStart: start, CreatedAt: time.Now().UTC().Format(time.RFC3339), LaunchToken: token})
	for try := 0; try < 2; try++ {
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err == nil {
			_, err = f.Write(rec)
			err = errors.Join(err, f.Close())
			if err != nil {
				_ = os.Remove(p)
			}
			return p, err
		}
		if !os.IsExist(err) {
			return "", err
		}
		held, err := readClaim(p)
		if err != nil {
			return "", fmt.Errorf("claim %s unreadable (%v): inspect it; remove it only after the RG is proven empty", p, err)
		}
		live, why := claimLive(held)
		if held.Output == filepath.Clean(output) || live || !outputSettled(held.Output) {
			return "", fmt.Errorf("resource group %s is claimed by run %s (output %s, %s, created %s)", group, held.Run, held.Output, why, held.CreatedAt)
		}
		if err = os.Remove(p); err != nil && !os.IsNotExist(err) {
			return "", err
		}
	}
	return "", fmt.Errorf("claim %s changed while it was being replaced", p)
}

// releaseClaim removes the claim of subscription+RG if output holds it.
func releaseClaim(dir, subscription, group, output string) error {
	p, err := claimPath(dir, subscription, group)
	if err != nil {
		return err
	}
	held, err := readClaim(p)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil || held.Output != filepath.Clean(output) {
		return err
	}
	return os.Remove(p)
}

// ClaimStatus is one claim as `seaweed-cloud claims` prints it.
type ClaimStatus struct {
	rgClaim
	Live    bool   `json:"live"`
	Why     string `json:"why"`
	Settled bool   `json:"output_settled"`
}

// Claims lists every claim with its controller's liveness and whether its
// output is settled. It never releases anything.
func Claims(dir string) ([]ClaimStatus, error) {
	names, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	out := []ClaimStatus{}
	for _, n := range names {
		c, err := readClaim(n)
		if err != nil {
			out = append(out, ClaimStatus{rgClaim: rgClaim{Output: n}, Why: "unreadable: " + err.Error()})
			continue
		}
		live, why := claimLive(c)
		out = append(out, ClaimStatus{rgClaim: c, Live: live, Why: why, Settled: outputSettled(c.Output)})
	}
	return out, nil
}
