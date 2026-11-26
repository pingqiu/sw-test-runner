package cli

import (
	tr "github.com/pingqiu/sw-test-runner"
	"testing"
)

func TestKnownZeroTopologyUsesRunnerSyntax(t *testing.T) {
	s, err := tr.ParseFile("../scenarios/public/known-zero-1tib-acceptance.yaml")
	if err != nil {
		t.Fatal(err)
	}
	s.Env["source_host"] = "192.0.2.1"
	s.Env["target_host"] = "192.0.2.2"
	s.Env["ssh_user"] = "tester"
	s.Env["ssh_key"] = "/operator/key"
	s.Env["source_host_key"] = "source-public-pin"
	s.Env["target_host_key"] = "target-public-pin"
	for _, name := range []string{"source", "target"} {
		n := s.Topology.Nodes[name]
		for key, pair := range map[string][2]string{"host": {n.Host, s.Env[name+"_host"]}, "user": {n.User, "tester"}, "key": {n.KeyFile, "/operator/key"}, "host_key": {n.HostKey, s.Env[name+"_host_key"]}} {
			if got := resolveTopologyValue(pair[0], s.Env); got != pair[1] {
				t.Errorf("%s %s=%q want %q", name, key, got, pair[1])
			}
		}
	}
}
