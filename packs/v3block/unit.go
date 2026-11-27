package v3block

// The holder's unit policy, read from the product's shipped unit file at the
// product commit the run built (holder_unit.file in product/checks.json;
// the commit is WORK/product.sha, written by the build step), never re-typed.
// The runner emulates the unit with it (supervise, faults.go); a run that
// does says so in its header (unitHeader), and is not evidence for the
// unit's own behaviour.

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	tr "github.com/pingqiu/sw-test-runner"
)

const unitPolicyVar = "v3unit_policy"

// unitPolicy is what the runner reads from the unit's [Service] section.
type unitPolicy struct {
	File    string `json:"file"`
	Commit  string `json:"commit"`
	Restart string `json:"restart"`
	// Exit statuses that never restart (RestartPreventExitStatus).
	PreventStatus []int `json:"restart_prevent_exit_status"`
	// Seconds; TimeoutStopSec is -1 when the file does not set it.
	RestartSec     float64 `json:"restart_sec"`
	TimeoutStopSec float64 `json:"timeout_stop_sec"`
}

func (p unitPolicy) restartSec() time.Duration {
	return time.Duration(p.RestartSec * float64(time.Second))
}

// cleanSignals are the signals systemd counts as a clean exit for
// Restart=on-failure: SIGHUP, SIGINT, SIGPIPE, SIGTERM.
var cleanSignals = map[int]bool{1: true, 2: true, 13: true, 15: true}

// restartable: would the unit start the holder again after this wait status?
// A status above 128 is read as death by signal status-128 (the wrapper's
// shell cannot tell it from an exit code above 128).
func (p unitPolicy) restartable(status int) bool {
	for _, s := range p.PreventStatus {
		if s == status {
			return false
		}
	}
	switch p.Restart {
	case "always":
		return true
	case "on-failure":
		clean := status == 0 || (status > 128 && cleanSignals[status-128])
		return !clean
	}
	return false // "no"
}

// parseUnit reads Restart, RestartPreventExitStatus, RestartSec and
// TimeoutStopSec from a unit file's [Service] section. Restart and RestartSec
// must be set; a value the runner cannot emulate is an error, never a guess.
func parseUnit(text string) (unitPolicy, error) {
	p := unitPolicy{TimeoutStopSec: -1}
	keys := map[string]string{}
	section := ""
	var logical []string
	cont := ""
	for _, l := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		if strings.HasSuffix(l, "\\") {
			cont += strings.TrimSuffix(l, "\\") + " "
			continue
		}
		logical = append(logical, cont+l)
		cont = ""
	}
	for _, l := range logical {
		l = strings.TrimSpace(l)
		switch {
		case l == "" || l[0] == '#' || l[0] == ';':
			continue
		case l[0] == '[':
			section = l
			continue
		}
		k, v, ok := strings.Cut(l, "=")
		if section == "[Service]" && ok {
			keys[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	p.Restart = keys["Restart"]
	switch p.Restart {
	case "no", "always", "on-failure":
	case "":
		return p, fmt.Errorf("unit file sets no Restart in [Service]")
	default:
		return p, fmt.Errorf("unit Restart=%s: the runner emulates only no, always and on-failure", p.Restart)
	}
	for _, f := range strings.Fields(keys["RestartPreventExitStatus"]) {
		n, err := strconv.Atoi(f)
		if err != nil || n < 0 || n > 255 {
			return p, fmt.Errorf("unit RestartPreventExitStatus entry %q: the runner emulates exit statuses only", f)
		}
		p.PreventStatus = append(p.PreventStatus, n)
	}
	rs, ok := keys["RestartSec"]
	if !ok {
		return p, fmt.Errorf("unit file sets no RestartSec in [Service]")
	}
	var err error
	if p.RestartSec, err = systemdSeconds(rs); err != nil {
		return p, fmt.Errorf("unit RestartSec: %w", err)
	}
	if ts, ok := keys["TimeoutStopSec"]; ok {
		if p.TimeoutStopSec, err = systemdSeconds(ts); err != nil {
			return p, fmt.Errorf("unit TimeoutStopSec: %w", err)
		}
	}
	return p, nil
}

var spanRe = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)([a-z]*)$`)

// systemdSeconds reads a systemd time span ("5s", "500ms", "1min 30s", "30").
func systemdSeconds(s string) (float64, error) {
	unit := map[string]float64{"": 1, "s": 1, "sec": 1, "second": 1, "seconds": 1, "ms": 1e-3, "msec": 1e-3, "us": 1e-6, "usec": 1e-6,
		"m": 60, "min": 60, "minute": 60, "minutes": 60, "h": 3600, "hr": 3600, "hour": 3600, "hours": 3600}
	f := strings.Fields(s)
	if len(f) == 0 {
		return 0, fmt.Errorf("empty time span")
	}
	total := 0.0
	for _, x := range f {
		m := spanRe.FindStringSubmatch(x)
		if m == nil {
			return 0, fmt.Errorf("time span %q", s)
		}
		k, ok := unit[m[2]]
		if !ok {
			return 0, fmt.Errorf("time span %q: unit %q", s, m[2])
		}
		n, _ := strconv.ParseFloat(m[1], 64)
		total += n * k
	}
	return total, nil
}

// unitFile is the unit's path in the product tree (product/checks.json).
func unitFile() (string, error) {
	var c struct {
		Unit *struct {
			File string `json:"file"`
		} `json:"holder_unit"`
	}
	if json.Unmarshal(checksJSON, &c) != nil || c.Unit == nil || c.Unit.File == "" || strings.Contains(c.Unit.File, "..") || strings.HasPrefix(c.Unit.File, "/") {
		return "", fmt.Errorf("product/checks.json names no holder_unit file")
	}
	return c.Unit.File, nil
}

// readUnitPolicy reads the unit file from the product checkout src at the
// commit WORK/product.sha names.
func readUnitPolicy(ctx context.Context, node runner, src, work string) (unitPolicy, error) {
	path, err := unitFile()
	if err != nil {
		return unitPolicy{}, err
	}
	if src == "" || work == "" {
		return unitPolicy{}, fmt.Errorf("product_src and work are needed to read the unit file %s", path)
	}
	sha, stderr, code, rerr := node.Run(ctx, "cat "+shellQuote(work+"/product.sha"))
	sha = strings.TrimSpace(sha)
	if rerr != nil || code != 0 || !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(sha) {
		return unitPolicy{}, fmt.Errorf("the product commit (%s/product.sha) is unreadable: %s", work, firstLine(stderr))
	}
	text, stderr, code, rerr := node.Run(ctx, fmt.Sprintf("git -C %s show %s", shellQuote(src), shellQuote(sha+":"+path)))
	if rerr != nil || code != 0 {
		return unitPolicy{}, fmt.Errorf("the unit file %s at %s is unreadable: %s", path, sha, firstLine(stderr))
	}
	p, err := parseUnit(text)
	if err != nil {
		return unitPolicy{}, fmt.Errorf("%s at %s: %w", path, sha, err)
	}
	p.File, p.Commit = path, sha
	return p, nil
}

// loadUnitPolicy returns the run's policy, reading it once (var
// product_src, the run's work directory). An error refuses emulation.
func loadUnitPolicy(ctx context.Context, actx *tr.ActionContext, node runner, e opEnv) (unitPolicy, error) {
	var p unitPolicy
	if raw := actx.Vars[unitPolicyVar]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			return p, fmt.Errorf("unit policy state: %w", err)
		}
		return p, nil
	}
	p, err := readUnitPolicy(ctx, node, actx.Vars["product_src"], e.work)
	if err != nil {
		return p, fmt.Errorf("the runner refuses to emulate the holder's unit: %w", err)
	}
	b, _ := json.Marshal(p)
	actx.Vars[unitPolicyVar] = string(b)
	return p, nil
}

// unitNotEvidence is the header's sentence for a run that emulated the unit.
const unitNotEvidence = "This run is not evidence for the unit's own behaviour: the runner emulated the unit."

// unitHeader is the run header's entry for an emulated unit.
func unitHeader(p unitPolicy) map[string]any {
	return map[string]any{"emulated": true, "policy": p, "note": unitNotEvidence}
}
