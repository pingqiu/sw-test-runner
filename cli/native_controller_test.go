package cli

import (
	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/infra"
	"testing"
)

func TestNativeControllerNodeNeedsNoSSH(t *testing.T) {
	s := &tr.Scenario{Topology: tr.Topology{Nodes: map[string]tr.NodeSpec{"controller": {IsNative: true}}}}
	a, err := setupActionContext(s, func(string, ...interface{}) {})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Nodes["controller"].Close()
	n, ok := a.Nodes["controller"].(*infra.Node)
	if !ok || !n.IsNative || n.IsLocal {
		t.Fatalf("wrong controller node: %+v", a.Nodes["controller"])
	}
}
