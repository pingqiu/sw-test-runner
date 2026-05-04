// Command weedblock is the V2 weed-block product test runner.
// It registers core actions plus the V2 block + KV product packs.
//
// Operator entry:
//
//	weedblock list
//	weedblock validate scenarios/internal/recovery-baseline-failover.yaml
//	weedblock run scenarios/internal/recovery-baseline-failover.yaml
//
// Use this binary against a V2 hardware lab where the weed binary is
// pre-installed at /opt/work/weed and the V2 master is reachable.
package main

import (
	"os"

	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/actions"
	"github.com/pingqiu/sw-test-runner/cli"
	"github.com/pingqiu/sw-test-runner/packs/block"
	"github.com/pingqiu/sw-test-runner/packs/kv"
)

func main() {
	register := func(r *tr.Registry) {
		actions.RegisterCore(r)
		block.RegisterPack(r)
		kv.RegisterPack(r)
	}
	os.Exit(cli.Run(register, os.Args[1:]))
}
