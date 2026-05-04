package actions

import tr "github.com/pingqiu/sw-test-runner"

// RegisterCore registers product-agnostic core actions:
// exec, sleep, assert_*, print, grep_log, fsck, fault injection, benchmarking, cleanup, results, recovery.
func RegisterCore(r *tr.Registry) {
	RegisterSystemActions(r)
	RegisterFaultActions(r)
	RegisterBenchActions(r)
	RegisterCleanupActions(r)
	RegisterResultActions(r)
	RegisterRecoveryActions(r)
}
