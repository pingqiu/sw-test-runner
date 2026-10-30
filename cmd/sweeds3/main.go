// Command sweeds3 is the SeaweedFS S3 gateway product test runner.
// It registers core actions plus the S3 product pack only.
//
// Operator entry:
//
//	sweeds3 list
//	sweeds3 validate scenarios/s3-smoke-chain.yaml
//	sweeds3 run scenarios/s3-smoke-chain.yaml
//
// Same pattern as cmd/swblock — swap the product pack import. See
// docs/cross-product-testops-standard.md (§6 onboarding a new product).
package main

import (
	"os"

	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/actions"
	"github.com/pingqiu/sw-test-runner/cli"
	"github.com/pingqiu/sw-test-runner/packs/s3"
)

func main() {
	register := func(r *tr.Registry) {
		actions.RegisterCore(r)
		s3.RegisterPack(r)
	}
	os.Exit(cli.Run(register, os.Args[1:]))
}
