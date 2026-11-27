package v3block

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	tr "github.com/pingqiu/sw-test-runner"
)

const effectHeader = `{"product": "block", "read_at": "x", "about": "a", "phrases": [
  {"phrase": "pinned text", "source": "a.go:1", "pinned_by": "a_test.go:9"},
  {"phrase": "loose text", "source": "a.go:2", "pinned_by": null}], "rows": [`

const effectRow = `{"action": "delete", "path": "ops chunk volume-delete", "code": "reserved_here", "outcome": "refused_effect",
  "cite": "x.go:1", "why": "w", "effect": {"part": "workspace", "to": {"exit_nonzero": true}, "cite": "docs/operator-guide.md:193-196"}}`

// A refused_effect row loads only with its effect and a PAGE citation of
// it; the decision carries the effect, and the executor's lookup hands it
// over as the completion's result.effect.
func TestRefusedEffectRows(t *testing.T) {
	tab, err := loadOutcomes([]byte(effectHeader + effectRow + `]}`))
	if err != nil {
		t.Fatal(err)
	}
	p := opPath{"delete", "ops chunk volume-delete"}
	out, eff := tab.decide(p, "reserved_here", "m")
	if out != tr.OutcomeRefusedEffect || eff == nil || eff.Part != "workspace" {
		t.Fatalf("%s %+v", out, eff)
	}
	got, rec := tab.lookupFunc()("delete", "ops chunk volume-delete", "reserved_here", "m")
	want := map[string]any{"part": "workspace", "to": map[string]any{"exit_nonzero": true}, "cite": "docs/operator-guide.md:193-196"}
	if got != tr.OutcomeRefusedEffect || !reflect.DeepEqual(rec, want) {
		t.Fatalf("lookup: %s %v", got, rec)
	}
	if o, d := chunkOutcomeIn(tab, p, `{"code":"reserved_here","message":"m"}`, 1); o != tr.OutcomeRefusedEffect || d != "reserved_here: m" {
		t.Fatalf("chunkOutcome: %s %q", o, d)
	}
	// Red controls: each seeded defect is refused at load.
	for name, bad := range map[string][2]string{
		"no effect":                {`, "effect": {"part": "workspace", "to": {"exit_nonzero": true}, "cite": "docs/operator-guide.md:193-196"}`, ``},
		"an effect cited in code":  {`"cite": "docs/operator-guide.md:193-196"`, `"cite": "core/host/master/workspace_service.go:32-33"`},
		"an effect without a cite": {`, "cite": "docs/operator-guide.md:193-196"`, ``},
		"an effect without a part": {`"part": "workspace", `, ``},
		"an effect without to":     {`"to": {"exit_nonzero": true}, `, ``},
		"an effect on a refusal":   {`"outcome": "refused_effect"`, `"outcome": "refused"`},
		"an effect on an unknown":  {`"outcome": "refused_effect"`, `"outcome": "unknown"`},
		"another outcome":          {`"outcome": "refused_effect"`, `"outcome": "refused_maybe"`},
	} {
		if !strings.Contains(effectRow, bad[0]) {
			t.Fatalf("%s: seed not in the row", name)
		}
		if _, err := loadOutcomes([]byte(effectHeader + strings.Replace(effectRow, bad[0], bad[1], 1) + `]}`)); err == nil {
			t.Errorf("%s: loaded", name)
		}
	}
	// A refused_effect row and a refused or unknown row for the same call conflict.
	for _, other := range []string{"refused", "unknown"} {
		r2 := `, {"action": "*", "path": "*", "code": "reserved_here", "outcome": "` + other + `", "cite": "y.go:1"}`
		if _, err := loadOutcomes([]byte(effectHeader + effectRow + r2 + `]}`)); err == nil {
			t.Errorf("refused_effect and %s for one call: loaded", other)
		}
	}
}

// page_gaps name only codes their own row keeps unknown, with a code line.
func TestPageGaps(t *testing.T) {
	if len(outcomes.PageGaps) == 0 {
		t.Fatal("the embedded table lists no page gap")
	}
	gap := func(codes, codeCite string) string {
		return effectHeader + `{"action": "delete", "path": "ops chunk volume-delete", "code": "c1", "outcome": "unknown", "cite": "x.go:1"},
		  {"action": "delete", "path": "ops chunk volume-delete", "code": "c2", "outcome": "refused", "cite": "x.go:2"}],
		  "page_gaps": [{"action": "delete", "path": "ops chunk volume-delete", "codes": [` + codes + `],
		    "effect": {"part": "workspace", "to": {"exit_nonzero": true}}, "code_cite": "` + codeCite + `", "page_read": "docs/x.md:1", "why": "w"}]}`
	}
	if _, err := loadOutcomes([]byte(gap(`"c1"`, "a.go:1"))); err != nil {
		t.Fatalf("green twin: %v", err)
	}
	for name, b := range map[string]string{
		"a refused code":     gap(`"c2"`, "a.go:1"),
		"a code with no row": gap(`"c3"`, "a.go:1"),
		"no code line":       gap(`"c1"`, "docs/x.md:1"),
	} {
		if _, err := loadOutcomes([]byte(b)); err == nil {
			t.Errorf("%s: loaded", name)
		}
	}
}

// Product text: an effect may match only a listed phrase that a product test
// pins; a row's message_contains must be listed (pinned or not).
func TestEffectPhrasesArePinned(t *testing.T) {
	row := func(to string) string {
		return `{"action": "delete", "path": "ops chunk volume-delete", "code": "reserved_here", "outcome": "refused_effect",
		  "cite": "x.go:1", "effect": {"part": "workspace", "to": {"exit": 1, ` + to + `}, "cite": "docs/operator-guide.md:193-196"}}`
	}
	if _, err := loadOutcomes([]byte(effectHeader + row(`"stderr_contains": "pinned text"`) + `]}`)); err != nil {
		t.Fatalf("green twin: %v", err)
	}
	for name, b := range map[string]string{
		"an unpinned phrase":        effectHeader + row(`"stderr_contains": "loose text"`) + `]}`,
		"an unlisted phrase":        effectHeader + row(`"text_contains": "other text"`) + `]}`,
		"a phrase that is no text":  effectHeader + row(`"stderr_contains": 7`) + `]}`,
		"an unlisted message":       effectHeader + `{"action": "a", "path": "p", "code": "c", "outcome": "refused", "cite": "x.go:1", "message_contains": "other"}]}`,
		"a pin that is no test":     strings.Replace(effectHeader, `"a_test.go:9"`, `"docs/x.md:1"`, 1) + `]}`,
		"a phrase without a source": strings.Replace(effectHeader, `"source": "a.go:2"`, `"source": ""`, 1) + `]}`,
	} {
		if _, err := loadOutcomes([]byte(b)); err == nil {
			t.Errorf("%s: loaded", name)
		}
	}
	// A listed message_contains loads whether pinned or not.
	if _, err := loadOutcomes([]byte(effectHeader + `{"action": "a", "path": "p", "code": "c", "outcome": "refused", "cite": "x.go:1", "message_contains": "loose text"}]}`)); err != nil {
		t.Fatalf("listed message: %v", err)
	}
	// The embedded table: every effect phrase is pinned.
	for _, r := range outcomes.Rows {
		if r.Effect == nil || r.PendingPin {
			continue
		}
		for k, v := range r.Effect.To {
			if strings.HasSuffix(k, "_contains") {
				found := false
				for _, ph := range outcomes.Phrases {
					found = found || (ph.Phrase == v && ph.PinnedBy != nil)
				}
				if !found {
					t.Errorf("%s %s: %v is not a pinned phrase", r.Action, r.Code, v)
				}
			}
		}
	}
}

// A refused_effect row on an unpinned phrase: marked pending_pin it decodes
// unknown (and must be listed in product_gaps); without the mark the table
// is refused. Pinning the phrase, and nothing else, makes it count.
func TestPendingPin(t *testing.T) {
	table := func(pinned, pending, listed bool) string {
		pin := "null"
		if pinned {
			pin = `"a_test.go:9"`
		}
		mark := ""
		if pending {
			mark = `"pending_pin": true, `
		}
		gaps := ""
		if listed {
			gaps = `, "product_gaps": [{"action": "delete", "path": "ops chunk volume-delete", "code": "reserved_here", "phrase": "the text", "gap": "phrase not pinned by a product test"}]`
		}
		return `{"product": "block", "read_at": "x", "about": "a",
		  "phrases": [{"phrase": "the text", "source": "a.go:1", "pinned_by": ` + pin + `}],
		  "rows": [{"action": "delete", "path": "ops chunk volume-delete", "code": "reserved_here", "outcome": "refused_effect", ` + mark + `
		    "cite": "x.go:1", "effect": {"part": "workspace", "to": {"exit": 1, "stderr_contains": "the text"}, "cite": "docs/operator-guide.md:193-196"}}]` + gaps + `}`
	}
	p := opPath{"delete", "ops chunk volume-delete"}
	decide := func(b string) (string, error) {
		tab, err := loadOutcomes([]byte(b))
		if err != nil {
			return "", err
		}
		out, eff := tab.decide(p, "reserved_here", "")
		if (out == tr.OutcomeRefusedEffect) != (eff != nil) {
			return "", fmt.Errorf("outcome %s with effect %v", out, eff)
		}
		return out, nil
	}
	if out, err := decide(table(false, true, true)); err != nil || out != tr.OutcomeUnknown {
		t.Fatalf("unpinned, pending: %s %v; want unknown", out, err)
	}
	if out, err := decide(table(true, true, true)); err != nil || out != tr.OutcomeRefusedEffect {
		t.Fatalf("pinned (only pinned_by flipped): %s %v; want refused_effect", out, err)
	}
	if out, err := decide(table(true, false, false)); err != nil || out != tr.OutcomeRefusedEffect {
		t.Fatalf("pinned, unmarked: %s %v; want refused_effect", out, err)
	}
	// Red controls.
	if _, err := decide(table(false, false, false)); err == nil {
		t.Error("an unpinned phrase without pending_pin loaded")
	}
	if _, err := decide(table(false, true, false)); err == nil {
		t.Error("a pending row missing from product_gaps loaded")
	}
	if _, err := loadOutcomes([]byte(`{"product": "block", "read_at": "x", "about": "a", "rows": [{"action": "a", "path": "p", "code": "c", "outcome": "refused", "pending_pin": true, "cite": "x.go:1"}]}`)); err == nil {
		t.Error("pending_pin on a plain refusal loaded")
	}
	// The embedded table: the delete row is pending and decodes unknown.
	if out, eff := outcomes.decide(chunkVolumeDelete, "snapshot_has_dependents", ""); out != tr.OutcomeUnknown || eff != nil {
		t.Fatalf("embedded delete row: %s %v", out, eff)
	}
}
