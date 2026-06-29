// Command sw-test-runner is the kitchen-sink build that registers every
// product pack in this module: block, KV/S3, V3 seaweed-block, and RDMA lab
// gates.
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
	"github.com/pingqiu/sw-test-runner/packs/rdma"
	"github.com/pingqiu/sw-test-runner/packs/s3"
	"github.com/pingqiu/sw-test-runner/packs/v3block"
)

func main() {
	register := func(r *tr.Registry) {
		actions.RegisterCore(r)
		block.RegisterPack(r)   // V2 weed-block
		kv.RegisterPack(r)      // V2 KV
		s3.RegisterPack(r)      // S3 gateway
		rdma.RegisterPack(r)    // RDMA lab gates
		v3block.RegisterPack(r) // V3 seaweed-block
	}
	os.Exit(cli.Run(register, os.Args[1:]))
}
