package declared

import (
	"encoding/json"
	"testing"
)

// A condition is ONE field compared with ONE constant: equals or
// not_equals. Anything else does not load.
func TestConditionIsOneConstantComparison(t *testing.T) {
	for _, ok := range []string{`{"field": "volume.x", "equals": ""}`, `{"field": "volume.x", "not_equals": "true"}`} {
		var c Cond
		if err := json.Unmarshal([]byte(ok), &c); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for name, bad := range map[string]string{
		"both comparisons": `{"field": "volume.x", "equals": "a", "not_equals": "b"}`,
		"no comparison":    `{"field": "volume.x"}`,
		"no field":         `{"equals": "a"}`,
		"a template value": `{"field": "volume.x", "equals": "{{volume.y}}"}`,
		"another operator": `{"field": "volume.x", "above": "3"}`,
		"a conjunction":    `{"field": "volume.x", "equals": "a", "and": {"field": "y", "equals": "b"}}`,
	} {
		var c Cond
		if err := json.Unmarshal([]byte(bad), &c); err == nil {
			t.Errorf("%s: loaded", name)
		}
	}
}
