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
	"regexp"
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
	Outcome         string `json:"outcome"` // refused | refused_effect | unknown
	MessageContains string `json:"message_contains,omitempty"`
	Cite            string `json:"cite"`
	Why             string `json:"why,omitempty"`
	// Effect is what a refused_effect call changed anyway, as a page states
	// it: the observation part, its value after, and the PAGE line.
	Effect *rowEffect `json:"effect,omitempty"`
	// PendingPin marks a refused_effect row whose effect matches a phrase
	// no product test pins yet: while any of its phrases is unpinned it
	// decodes as unknown (its effect kept in the table, the row listed in
	// product_gaps). Once the phrase's pinned_by is set it counts, with no
	// other edit.
	PendingPin bool `json:"pending_pin,omitempty"`
	// effective is the outcome the row decides with (set at load).
	effective string
}

type rowEffect struct {
	Part string         `json:"part"`
	To   map[string]any `json:"to"`
	Cite string         `json:"cite"`
}

func (e *rowEffect) record() map[string]any {
	return map[string]any{"part": e.Part, "to": e.To, "cite": e.Cite}
}

// pageGap is a failed answer kept unknown because its effect is stated only
// in code, not on a page: the rows (action, path, codes), the effect as the
// code shows it, the code line, the page read and why it does not state it.
type pageGap struct {
	Action string   `json:"action"`
	Path   string   `json:"path"`
	Codes  []string `json:"codes"`
	Effect struct {
		Part string         `json:"part"`
		To   map[string]any `json:"to"`
	} `json:"effect"`
	CodeCite string `json:"code_cite"`
	PageRead string `json:"page_read"`
	Why      string `json:"why"`
}

type outcomeTable struct {
	Product     string       `json:"product"`
	ReadAt      string       `json:"read_at"`
	About       string       `json:"about"`
	Rows        []outcomeRow `json:"rows"`
	PageGaps    []pageGap    `json:"page_gaps,omitempty"`
	Phrases     []phrase     `json:"phrases,omitempty"`
	ProductGaps []productGap `json:"product_gaps,omitempty"`
}

// productGap is a row kept unknown because the product does not pin the
// text its effect matches: the row, the phrase and the gap.
type productGap struct {
	Action string `json:"action"`
	Path   string `json:"path"`
	Code   string `json:"code"`
	Phrase string `json:"phrase"`
	Gap    string `json:"gap"`
}

// phrase is a match on PRODUCT TEXT the table relies on (an effect's
// *_contains, a row's message_contains): its source line and the product
// test that pins it, or null. Every phrase is a product gap until the
// product treats it as an interface; an effect may rely only on a pinned
// one.
type phrase struct {
	Phrase   string  `json:"phrase"`
	Source   string  `json:"source"`
	PinnedBy *string `json:"pinned_by"`
	Note     string  `json:"note,omitempty"`
}

// pageLine is a citation of a product page line: docs/<page>.md:<line>.
var pageLine = regexp.MustCompile(`^docs/[A-Za-z0-9._/-]+\.md:[0-9]+(-[0-9]+)?\b`)

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
		case r.Outcome != "refused" && r.Outcome != "refused_effect" && r.Outcome != "unknown":
			return t, fmt.Errorf("%s: outcome %q is not refused, refused_effect or unknown", at, r.Outcome)
		case r.MessageContains != "" && r.Outcome == "unknown":
			return t, fmt.Errorf("%s: message_contains narrows a refusal only", at)
		case (r.Effect != nil) != (r.Outcome == "refused_effect"):
			return t, fmt.Errorf("%s: an effect exactly when the outcome is refused_effect", at)
		case r.Effect != nil && (r.Effect.Part == "" || len(r.Effect.To) == 0):
			return t, fmt.Errorf("%s: the effect needs its part and to", at)
		case r.Effect != nil && !pageLine.MatchString(r.Effect.Cite):
			return t, fmt.Errorf("%s: the effect's cite %q is not a page line (docs/<page>.md:<line>): an effect only in code stays unknown and goes to page_gaps", at, r.Effect.Cite)
		}
		k := [3]string{r.Action, r.Path, r.Code}
		if seen[k] {
			return t, fmt.Errorf("%s: duplicate (action, path, code)", at)
		}
		seen[k] = true
	}
	for i, r := range t.Rows {
		if r.Outcome == "unknown" {
			continue
		}
		for j, u := range t.Rows {
			if i != j && u.Outcome != r.Outcome && u.Code == r.Code && overlap(r.Action, u.Action) && overlap(r.Path, u.Path) {
				return t, fmt.Errorf("outcomes rows %d and %d: %s %q %s is both %s and %s", i, j, r.Action, r.Path, r.Code, r.Outcome, u.Outcome)
			}
		}
	}
	phrases := map[string]phrase{}
	for i, ph := range t.Phrases {
		at := fmt.Sprintf("outcomes phrase %d (%q)", i, ph.Phrase)
		switch {
		case ph.Phrase == "" || !strings.Contains(ph.Source, ".go:"):
			return t, fmt.Errorf("%s: the phrase and its source line are required", at)
		case ph.PinnedBy != nil && !strings.Contains(*ph.PinnedBy, "_test.go:"):
			return t, fmt.Errorf("%s: pinned_by %q is not a product test line (or null)", at, *ph.PinnedBy)
		}
		if _, dup := phrases[ph.Phrase]; dup {
			return t, fmt.Errorf("%s: listed twice", at)
		}
		phrases[ph.Phrase] = ph
	}
	for i, r := range t.Rows {
		at := fmt.Sprintf("outcomes row %d (%s %q %s)", i, r.Action, r.Path, r.Code)
		if r.MessageContains != "" {
			if _, ok := phrases[r.MessageContains]; !ok {
				return t, fmt.Errorf("%s: message_contains %q is product text not listed in phrases", at, r.MessageContains)
			}
		}
		t.Rows[i].effective = r.Outcome
		if r.PendingPin && r.Outcome != "refused_effect" {
			return t, fmt.Errorf("%s: pending_pin marks a refused_effect row only", at)
		}
		if r.Effect == nil {
			continue
		}
		unpinned := ""
		for k, v := range r.Effect.To {
			text, isText := v.(string)
			if !strings.HasSuffix(k, "_contains") {
				continue
			}
			ph, ok := phrases[text]
			switch {
			case !isText || text == "":
				return t, fmt.Errorf("%s: effect %s is not a phrase", at, k)
			case !ok:
				return t, fmt.Errorf("%s: effect %s %q is product text not listed in phrases", at, k, text)
			case ph.PinnedBy == nil:
				unpinned = text
			}
		}
		if unpinned == "" {
			continue
		}
		if !r.PendingPin {
			return t, fmt.Errorf("%s: the effect relies on %q, which no product test pins (pinned_by null): mark the row pending_pin, so it decodes unknown until the pin exists", at, unpinned)
		}
		listed := false
		for _, g := range t.ProductGaps {
			listed = listed || (g.Action == r.Action && g.Path == r.Path && g.Code == r.Code && g.Phrase == unpinned)
		}
		if !listed {
			return t, fmt.Errorf("%s: pending on the unpinned phrase %q, but not listed in product_gaps", at, unpinned)
		}
		t.Rows[i].effective = "unknown"
	}
	for i, g := range t.ProductGaps {
		if _, ok := phrases[g.Phrase]; !ok || g.Action == "" || g.Path == "" || g.Code == "" || strings.TrimSpace(g.Gap) == "" {
			return t, fmt.Errorf("outcomes product gap %d: action, path, code, a listed phrase and the gap are required", i)
		}
	}
	for i, g := range t.PageGaps {
		at := fmt.Sprintf("outcomes page gap %d (%s %q)", i, g.Action, g.Path)
		if g.Action == "" || g.Path == "" || len(g.Codes) == 0 || g.Effect.Part == "" || len(g.Effect.To) == 0 ||
			!strings.Contains(g.CodeCite, ".go:") || strings.TrimSpace(g.PageRead) == "" || strings.TrimSpace(g.Why) == "" {
			return t, fmt.Errorf("%s: action, path, codes, effect, a code line, the page read and why are required", at)
		}
		for _, c := range g.Codes {
			if got, _ := t.decide(opPath{g.Action, g.Path}, c, ""); got != "unknown" || !t.hasRow(g.Action, g.Path, c, "unknown") {
				return t, fmt.Errorf("%s: code %s is not kept unknown by its own row", at, c)
			}
		}
	}
	return t, nil
}

func overlap(a, b string) bool { return a == "*" || b == "*" || a == b }

// lookup decides a failed answer (decide without the effect).
func (t outcomeTable) lookup(p opPath, code, message string) string {
	out, _ := t.decide(p, code, message)
	return out
}

// decide decides a failed answer: refused (or refused_effect, with its
// effect) only when a matching row of that outcome holds for the message
// and no matching row says unknown.
func (t outcomeTable) decide(p opPath, code, message string) (string, *rowEffect) {
	var hit *outcomeRow
	for i, r := range t.Rows {
		if !r.matches(p, code) {
			continue
		}
		if r.decides() == "unknown" {
			return "unknown", nil
		}
		if hit == nil && (r.MessageContains == "" || strings.Contains(message, r.MessageContains)) {
			hit = &t.Rows[i]
		}
	}
	if hit != nil {
		if hit.decides() != "refused_effect" {
			return hit.decides(), nil
		}
		return hit.decides(), hit.Effect
	}
	return "unknown", nil
}

// decides is the outcome the row decides with: its outcome, or unknown
// while it is pending a product pin (set at load; a table built in code
// without loading decides with its outcomes).
func (r outcomeRow) decides() string {
	if r.effective != "" {
		return r.effective
	}
	return r.Outcome
}

func (t outcomeTable) hasRow(action, path, code, outcome string) bool {
	for _, r := range t.Rows {
		if r.Action == action && r.Path == path && r.Code == code && r.Outcome == outcome {
			return true
		}
	}
	return false
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
	out, detail, _ := declared.DecodeErrorBody(t.lookupFunc(), op.action, op.path, stdout, code)
	return out, detail
}

// lookupFunc is the table as the executor asks for it.
func (t outcomeTable) lookupFunc() declared.LookupFunc {
	return func(action, path, code, message string) (string, map[string]any) {
		out, effect := t.decide(opPath{action, path}, code, message)
		if effect == nil {
			return out, nil
		}
		return out, effect.record()
	}
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
