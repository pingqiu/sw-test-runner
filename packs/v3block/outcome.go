package v3block

// The outcome of one operator-surface call: ok, refused or unknown. Each
// mapping decides from an exit code and the command's own answer, never from
// a log line. "refused" means nothing changed, so it is used only where the
// product states that; anything else that is not ok is unknown, for a read
// of the state to decide.
//
// Which failed answers are refused is a product fact, one row each with its
// citation, in product/outcomes.json (read at the product commit its header
// names; re-read the rows when the runner's product pin moves). This file
// holds only the mechanism: parse the answer, look up the row; no row means
// unknown.

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/pingqiu/sw-test-runner/declared"
)

// v3ctlAnswer is v3ctl's JSON answer.
type v3ctlAnswer struct {
	Outcome    string `json:"outcome"`
	Volume     string `json:"volume"`
	LayoutID   string `json:"layout_id"`
	StoreReady bool   `json:"store_ready"`
	CallNs     int64  `json:"call_ns"`
	Error      string `json:"error"`
}

// parseV3ctl maps v3ctl's exit code and answer to an outcome. The exit code
// and the answer must agree; anything else is unknown. The exits are v3ctl's
// own (v3ctl/main.go), not a product table.
func parseV3ctl(stdout string, code int, volume string) (v3ctlAnswer, string) {
	var a v3ctlAnswer
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &a); err != nil || a.Volume != volume {
		return a, "unknown"
	}
	want, known := map[string]int{"ok": 0, "refused": 1, "unknown": 3}[a.Outcome]
	if !known || want != code || (a.Outcome == "ok" && len(a.LayoutID) != 32) {
		return a, "unknown"
	}
	return a, a.Outcome
}

//go:embed product/outcomes.json
var outcomesJSON []byte

// outcomeRow is one row of product/outcomes.json.
type outcomeRow struct {
	Action          string `json:"action"`  // runner action, or "*"
	Path            string `json:"path"`    // product command or route, or "*"
	Code            string `json:"code"`    // ops chunk error code, or ops workspace exit
	Outcome         string `json:"outcome"` // refused | unknown
	MessageContains string `json:"message_contains,omitempty"`
	Cite            string `json:"cite"`
	Why             string `json:"why,omitempty"`
}

type outcomeTable struct {
	Product string       `json:"product"`
	ReadAt  string       `json:"read_at"`
	About   string       `json:"about"`
	Rows    []outcomeRow `json:"rows"`
}

// opPath names one call: the runner action and the product path it calls.
// The zero opPath is an action the table does not name: only "*" rows apply.
type opPath struct{ action, path string }

func (r outcomeRow) matches(p opPath, code string) bool {
	return r.Code == code && (r.Action == "*" || r.Action == p.action) && (r.Path == "*" || r.Path == p.path)
}

// loadOutcomes parses a table and checks it: every row has a code, a cite and
// a known outcome; no two rows share (action, path, code); and no code is
// refused for a call that another row keeps unknown.
func loadOutcomes(b []byte) (outcomeTable, error) {
	var t outcomeTable
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&t); err != nil {
		return t, fmt.Errorf("outcomes: %w", err)
	}
	if t.Product == "" || t.ReadAt == "" {
		return t, fmt.Errorf("outcomes: the header needs product and read_at")
	}
	seen := map[[3]string]bool{}
	for i, r := range t.Rows {
		at := fmt.Sprintf("outcomes row %d (%s %q %s)", i, r.Action, r.Path, r.Code)
		switch {
		case r.Action == "" || r.Path == "" || r.Code == "":
			return t, fmt.Errorf("%s: action, path and code are required", at)
		case strings.TrimSpace(r.Cite) == "":
			return t, fmt.Errorf("%s: no cite", at)
		case r.Outcome != "refused" && r.Outcome != "unknown":
			return t, fmt.Errorf("%s: outcome %q is not refused or unknown", at, r.Outcome)
		case r.MessageContains != "" && r.Outcome != "refused":
			return t, fmt.Errorf("%s: message_contains narrows a refusal only", at)
		}
		k := [3]string{r.Action, r.Path, r.Code}
		if seen[k] {
			return t, fmt.Errorf("%s: duplicate (action, path, code)", at)
		}
		seen[k] = true
	}
	for i, r := range t.Rows {
		if r.Outcome != "refused" {
			continue
		}
		for j, u := range t.Rows {
			if u.Outcome == "unknown" && u.Code == r.Code && overlap(r.Action, u.Action) && overlap(r.Path, u.Path) {
				return t, fmt.Errorf("outcomes rows %d and %d: %s %q %s is both refused and unknown", i, j, r.Action, r.Path, r.Code)
			}
		}
	}
	return t, nil
}

func overlap(a, b string) bool { return a == "*" || b == "*" || a == b }

// lookup decides a failed answer: refused only when a matching refused row
// holds for the message and no matching row says unknown.
func (t outcomeTable) lookup(p opPath, code, message string) string {
	refused := false
	for _, r := range t.Rows {
		if !r.matches(p, code) {
			continue
		}
		if r.Outcome != "refused" {
			return "unknown"
		}
		if r.MessageContains == "" || strings.Contains(message, r.MessageContains) {
			refused = true
		}
	}
	if refused {
		return "refused"
	}
	return "unknown"
}

// outcomes is the embedded table. A table that does not load stops the pack:
// a bad table must not decide outcomes.
var outcomes = func() outcomeTable {
	t, err := loadOutcomes(outcomesJSON)
	if err != nil {
		panic(err)
	}
	return t
}()

// The calls whose `sw-block ops chunk` answers (or holder route answers)
// chunkOutcome maps, as the runner issues them.
var (
	chunkDetach        = opPath{"detach", "ops chunk detach"}                     // holder POST /chunk/detach, inside the planned stop
	chunkSnapshot      = opPath{"snapshot", "ops chunk snapshot"}                 // holder POST /chunk/snapshot
	chunkSnapDelHolder = opPath{"snapshot_delete", "POST /chunk/snapshot/delete"} // holder route
	chunkSnapDelStore  = opPath{"snapshot_delete", "ops chunk snapshot-delete"}   // store direct
	chunkRestore       = opPath{"restore", "ops chunk restore"}                   // master CreateVolume
	chunkRelease       = opPath{"release", "ops chunk release"}                   // store direct
	chunkVolumeDelete  = opPath{"delete", "ops chunk volume-delete"}
)

// chunkOutcome maps one call's `sw-block ops chunk` exit (or holder route
// answer) and its JSON error body to an outcome: exit 0 ok; an error code the
// table refuses for this call, refused; every other answer, outcome_unknown
// included whatever its retryable flag says, unknown.
func chunkOutcome(op opPath, stdout string, code int) (string, string) {
	return chunkOutcomeIn(outcomes, op, stdout, code)
}

func chunkOutcomeIn(t outcomeTable, op opPath, stdout string, code int) (string, string) {
	return declared.DecodeErrorBody(func(action, path, code, message string) string {
		return t.lookup(opPath{action, path}, code, message)
	}, op.action, op.path, stdout, code)
}

// The runner actions that issue `sw-block ops workspace` commands, and the
// commands. Both actions run the planned hand-over (handOver in ops.go)
// before any workspace command.
const (
	wsStop      = "stop" // revoke, inside the planned stop (handOver)
	wsMove      = "move" // revoke inside handOver, then move --clean
	wsRevoke    = "ops workspace revoke"
	wsMoveClean = "ops workspace move --clean"
)

// wsOutcome maps one `sw-block ops workspace` exit of a runner action: 0 ok;
// an exit the table refuses for that action and command, refused; anything
// else unknown.
func wsOutcome(action, path string, code int) string {
	return wsOutcomeIn(outcomes, action, path, code)
}

func wsOutcomeIn(t outcomeTable, action, path string, code int) string {
	if code == 0 {
		return "ok"
	}
	return t.lookup(opPath{action, path}, strconv.Itoa(code), "")
}
