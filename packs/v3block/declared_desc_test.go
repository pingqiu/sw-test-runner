package v3block

import (
	"sort"
	"strings"
	"testing"

	tr "github.com/pingqiu/sw-test-runner"
)

// Every typed operation the pack registers is either declared in
// product/actions.json or listed there as an exception with its reason;
// a declared action records the op id the model-to-code trace names
// (opIDs, operations.json); every error-body decoding names a path the
// outcome table has rows for; every variant cites the product.
func TestActionsDescription(t *testing.T) {
	r := tr.NewRegistry()
	RegisterPack(r)
	declaredNames := map[string]bool{}
	for _, a := range blockActions.File.Actions {
		declaredNames[a.Action] = true
	}
	excepted := map[string]bool{}
	for _, x := range blockActions.File.Exceptions {
		excepted[x.Action] = true
	}
	var missing []string
	for _, name := range r.List() {
		if strings.HasPrefix(name, "v3_op_") && name != "v3_op_spec" && !declaredNames[name] && !excepted[name] {
			missing = append(missing, name)
		}
	}
	for name := range declaredNames {
		if !r.Has(name) {
			missing = append(missing, name+" (declared, not registered)")
		}
	}
	for name := range excepted {
		if !r.Has(name) {
			missing = append(missing, name+" (an exception, not registered)")
		}
	}
	sort.Strings(missing)
	if len(missing) != 0 {
		t.Fatalf("neither declared nor an exception, or not registered: %v", missing)
	}

	ops := map[string]string{}
	for _, o := range genTable.Operations {
		ops[o.Action] = o.Op
	}
	paths := map[[2]string]bool{}
	for _, row := range outcomes.Rows {
		paths[[2]string{row.Action, row.Path}] = true
	}
	for _, a := range blockActions.File.Actions {
		if a.Action != "v3_op_"+a.Verb {
			t.Errorf("%s: verb %q does not name the action", a.Action, a.Verb)
		}
		if a.Op != opIDs[a.Verb] || (ops[a.Action] != "" && ops[a.Action] != a.Op) {
			t.Errorf("%s: op %q; opIDs %q, operations.json %q", a.Action, a.Op, opIDs[a.Verb], ops[a.Action])
		}
		for _, v := range a.Variants {
			if !strings.Contains(v.Cite, ".go:") {
				t.Errorf("%s: variant %q cites no source line: %q", a.Action, v.Name, v.Cite)
			}
			if v.Decode.Kind != "error_body" {
				continue
			}
			action := v.Decode.Action
			if action == "" {
				action = a.Verb
			}
			if !paths[[2]string{action, v.Decode.Path}] {
				t.Errorf("%s: outcomes.json has no row for (%s, %q)", a.Action, action, v.Decode.Path)
			}
		}
	}
}
