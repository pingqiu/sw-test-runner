package v3block

import (
	"context"
	"fmt"
	tr "github.com/pingqiu/sw-test-runner"
	"strings"
	"testing"
)

func TestR4StandaloneUsesOnlyLocalGuestsAndOffHostLedger(t *testing.T) {
	s, err := tr.ParseFile("../../scenarios/v3block/r4-node-loss.yaml")
	if err != nil {
		t.Fatal(err)
	}
	guests := map[string]int{}
	handler := func(_ context.Context, _ *tr.ActionContext, a tr.Action) (map[string]string, error) {
		cmd := a.Params["cmd"]
		if strings.Contains(cmd, "/bin/blockvolume ") && (strings.Contains(cmd, "--allow-external-nvme-bind") || !strings.Contains(cmd, "--nvme-listen 127.0.0.1:")) {
			return nil, fmt.Errorf("standalone target is not loopback-only")
		}
		if strings.Contains(cmd, "guest-io.py") {
			for _, op := range []string{"before", "stream-start", "stream-ended", "verify", "after-rejoin"} {
				if strings.HasSuffix(strings.TrimSpace(cmd), " "+op) {
					if a.Node != "controller" {
						return nil, fmt.Errorf("guest ACK ledger must be off-host")
					}
					guests[op]++
				}
			}
		}
		if strings.Contains(cmd, "auto-p2.py wait ") {
			return map[string]string{"value": "test-promotion-token"}, nil
		}
		return nil, nil
	}
	r := tr.NewRegistry()
	for _, name := range []string{"exec", "upload_path", "collect_path", "cloud_vm_power", "cloud_vm_power_restore"} {
		r.RegisterFunc(name, tr.TierCore, handler)
	}
	result := tr.NewEngine(r, nil).Run(context.Background(), s, &tr.ActionContext{Scenario: s, Vars: map[string]string{}, Log: func(string, ...interface{}) {}})
	if result.Status != tr.StatusPass {
		t.Fatal(result.Error)
	}
	for _, op := range []string{"before", "stream-start", "stream-ended", "verify", "after-rejoin"} {
		if guests[op] != 1 {
			t.Errorf("missing off-host coordinated guest step %s: %v", op, guests)
		}
	}
}
