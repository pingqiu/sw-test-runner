// Command sweedrdma is the SeaweedFS RDMA lab test runner.
package main

import (
	"os"

	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/actions"
	"github.com/pingqiu/sw-test-runner/cli"
	"github.com/pingqiu/sw-test-runner/packs/rdma"
)

func main() {
	register := func(r *tr.Registry) {
		actions.RegisterCore(r)
		rdma.RegisterPack(r)
	}
	os.Exit(cli.Run(register, os.Args[1:]))
}
