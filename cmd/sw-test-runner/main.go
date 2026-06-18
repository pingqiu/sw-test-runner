// Command sw-test-runner is the kitchen-sink build that registers every
// product pack in this module: V2 weed-block, V2 KV, and V3 seaweed-block.
//
// Per-product binaries (cmd/swblock, cmd/weedblock, etc.) provide narrower
// builds that link only the pack(s) for one product. Both styles share the
// same cli.Run dispatcher and scenario YAML format.
package main

import (
	"os"

	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/actions"
	"github.com/pingqiu/sw-test-runner/cli"
	"github.com/pingqiu/sw-test-runner/packs/block"
	"github.com/pingqiu/sw-test-runner/packs/kv"
	"github.com/pingqiu/sw-test-runner/packs/v3block"
)

func main() {
	register := func(r *tr.Registry) {
		actions.RegisterCore(r)
		block.RegisterPack(r)   // V2 weed-block
		kv.RegisterPack(r)      // V2 KV
		v3block.RegisterPack(r) // V3 seaweed-block
	}
	os.Exit(cli.Run(register, os.Args[1:]))
}
