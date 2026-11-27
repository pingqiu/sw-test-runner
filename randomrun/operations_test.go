package randomrun

import (
	"reflect"
	"strings"
	"testing"
)

const goodTable = `{"product": "p", "read_at": "p abc",
 "operations": [{"action": "a", "op": "M.A", "weight": 3, "cost": 2}, {"action": "b", "cost": 1}],
 "limits": [{"id": "x", "name": "a twice", "action": "a", "cite": "docs/p.md:1",
             "side_effect": "s", "side_effect_cite": "docs/p.md:2"}]}`

func TestParseOperations(t *testing.T) {
	tab, err := ParseOperations([]byte(goodTable))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(tab.DefaultWeights(), map[string]int{"a": 3}) {
		t.Fatalf("weights %v: an operation without a weight has no default weight", tab.DefaultWeights())
	}
	if !reflect.DeepEqual(tab.Costs(), map[string]float64{"a": 2, "b": 1}) {
		t.Fatalf("costs %v", tab.Costs())
	}
	if l, ok := tab.Limit("x"); !ok || l.Name != "a twice" {
		t.Fatalf("limit %v %v", l, ok)
	}
	// Red controls: each seeded defect in a table is refused.
	for name, edit := range map[string][2]string{
		"no read_at":            {`"read_at": "p abc",`, ``},
		"an unknown field":      {`"cost": 1}`, `"cost": 1, "wieght": 2}`},
		"a repeated action":     {`{"action": "b", "cost": 1}`, `{"action": "a", "cost": 1}`},
		"a zero cost":           {`"cost": 2}`, `"cost": 0}`},
		"a negative weight":     {`"weight": 3`, `"weight": -1`},
		"a limit without cite":  {`"cite": "docs/p.md:1",`, ``},
		"a limit on no action":  {`"action": "a", "cite"`, `"action": "c", "cite"`},
		"a side effect no cite": {`, "side_effect_cite": "docs/p.md:2"`, ``},
		"a limit name repeated": {`]}`, `, {"id": "y", "name": "a twice", "action": "a", "cite": "docs/p.md:3"}]}`},
		"a limit id repeated":   {`]}`, `, {"id": "x", "name": "b", "action": "a", "cite": "docs/p.md:3"}]}`},
	} {
		bad := strings.Replace(goodTable, edit[0], edit[1], 1)
		if bad == goodTable {
			t.Fatalf("%s: the edit did not apply", name)
		}
		if _, err := ParseOperations([]byte(bad)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
