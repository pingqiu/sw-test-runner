package actions

import (
	"context"
	"testing"

	tr "github.com/pingqiu/sw-test-runner"
)

func TestTierChaosNeverFallsBackToUnwitnessedCleanupStrings(t *testing.T) {
	r := tr.NewRegistry()
	RegisterFaultActions(r)
	for _, name := range []string{"inject_partition", "inject_netem", "fill_disk", "clear_fault"} {
		h, err := r.Get(name)
		if err != nil {
			t.Fatal(err)
		}
		_, err = h.Execute(context.Background(), &tr.ActionContext{Vars: map[string]string{"__cleanup_fill_disk": "true"}}, tr.Action{Action: name, Node: "fake", Params: map[string]string{"target_ip": "127.0.0.1", "ports": "80", "dir": "/tmp"}})
		if err == nil {
			t.Fatalf("%s ran without owned witnessed plan", name)
		}
	}
}
