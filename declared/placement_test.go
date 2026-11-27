package declared

import "testing"

func TestPlaceByListMember(t *testing.T) {
	rows, err := ParsePlacements([]byte(`{"placement": [{"verb": "v", "by": "list_member", "field": "names", "arg": "name",
		"outcomes": {"present_before": "not_placed", "others_lost": "third", "present": "took_effect", "absent": "did_not"}, "cite": "docs/x.md:1"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	p := rows["v"]
	for name, c := range map[string]struct {
		before, after any
		want          string
		ok            bool
	}{
		"present":             {[]any{}, []any{"n"}, PlacedTookEffect, true},
		"absent":              {[]any{"a"}, []any{"a"}, PlacedDidNot, true},
		"others lost":         {[]any{"a"}, []any{"n"}, PlacedThird, true},
		"present before":      {[]any{"n"}, []any{"n"}, PlacedNotPlaced, true},
		"no list before":      {nil, []any{"n"}, PlacedNotPlaced, false},
		"no list after":       {[]any{}, "unreadable", PlacedNotPlaced, false},
		"not a list of names": {[]any{1}, []any{"n"}, PlacedNotPlaced, false},
	} {
		got, why, ok := p.Place("n", map[string]any{"names": c.before}, map[string]any{"names": c.after})
		if got != c.want || ok != c.ok || why == "" {
			t.Errorf("%s: %q %v (%s)", name, got, ok, why)
		}
	}
}
