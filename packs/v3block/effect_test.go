package v3block

import (
	"reflect"
	"strings"
	"testing"

	tr "github.com/pingqiu/sw-test-runner"
)

const effectHeader = `{"product": "block", "read_at": "x", "about": "a", "rows": [`

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
