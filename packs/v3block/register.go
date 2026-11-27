// Package v3block is the V3 SeaweedFS block storage product pack for sw-test-runner.
//
// It registers V3-specific actions (start_blockmaster/blockvolume/blockcsi,
// cluster-spec apply, primary wait, status read, anti-authority-leak assert)
// on top of the product-agnostic runner core.
//
// V3 daemons live in the seaweed_block module (github.com/seaweedfs/seaweed-block).
// This pack does NOT import V3 daemon types — actions take binary paths from
// scenario YAML, spawn processes, and parse stdout/HTTP/log output. This keeps
// the V2 testrunner module dependency surface minimal.
//
// Action prefix: every V3 action is named v3_* to avoid name collision with
// V2 packs/block/ actions. Per the audit at
// sw-block/design/test/v3-phase-15-v2-testrunner-plugin-audit.md §4,
// V3 must NOT register actions with V2-shaped authority semantics
// (no promote, no demote, no set_lease, no role assignment).
package v3block

import (
	tr "github.com/pingqiu/sw-test-runner"
)

// RegisterPack registers all V3-specific actions on the registry.
// Core actions (exec, sleep, assert_*, bench, k8s, iscsi client, fault) are
// NOT registered here — they are registered by actions.RegisterCore() and
// other tier-agnostic packs.
func RegisterPack(r *tr.Registry) {
	// TierDevOps — daemon lifecycle (analogous to V2 start_weed_master)
	r.RegisterFunc("v3_start_blockmaster", tr.TierDevOps, v3StartBlockmaster)
	r.RegisterFunc("v3_start_blockvolume", tr.TierDevOps, v3StartBlockvolume)
	r.RegisterFunc("v3_start_blockcsi", tr.TierDevOps, v3StartBlockcsi)
	r.RegisterFunc("v3_apply_cluster_spec", tr.TierDevOps, v3ApplyClusterSpec)

	// TierBlock — V3-shaped status / wait / structural assertions
	r.RegisterFunc("v3_status", tr.TierBlock, v3Status)
	r.RegisterFunc("v3_wait_primary", tr.TierBlock, v3WaitPrimary)
	r.RegisterFunc("v3_assert_no_authority_leak", tr.TierBlock, v3AssertNoAuthorityLeak)

	// TierBlock — typed operations over a volume set (volset.go), each
	// through the operator surface, outcome ok / refused / unknown.
	r.RegisterFunc("v3_vs_spec", tr.TierDevOps, v3VsSpec)
	r.RegisterFunc("v3_vs_create", tr.TierBlock, v3VsCreate)
	r.RegisterFunc("v3_vs_attach", tr.TierBlock, v3VsAttach)
	r.RegisterFunc("v3_vs_write", tr.TierBlock, v3VsWrite)
	r.RegisterFunc("v3_vs_verify", tr.TierBlock, v3VsVerify)
	r.RegisterFunc("v3_vs_snapshot", tr.TierBlock, v3VsSnapshot)
	r.RegisterFunc("v3_vs_snapshot_delete", tr.TierBlock, v3VsSnapshotDelete)
	r.RegisterFunc("v3_vs_objects", tr.TierBlock, v3VsObjects)
	r.RegisterFunc("v3_vs_restart", tr.TierBlock, v3VsRestart)
	r.RegisterFunc("v3_vs_serving", tr.TierBlock, v3VsServing)
	r.RegisterFunc("v3_vs_move", tr.TierBlock, v3VsMove)
	r.RegisterFunc("v3_vs_host", tr.TierBlock, v3VsHost)
	r.RegisterFunc("v3_vs_stop", tr.TierBlock, v3VsStop)
	// Typed operations (ops.go): one operator operation on one volume, each
	// recording invoke and complete in the run's history; they decide nothing.
	r.RegisterFunc("v3_op_spec", tr.TierDevOps, v3OpSpec)
	r.RegisterFunc("v3_history_end", tr.TierBlock, v3HistoryEnd)
	// The M1 random run (gen.go, genrun.go): proposals through the same typed operations.
	r.RegisterFunc("v3_gen_run", tr.TierBlock, v3GenRun)
	r.RegisterFunc("v3_gen_replay", tr.TierBlock, v3GenReplay) // genreplay.go: re-execute a recorded run, compare outcomes
	// The tier-1 declared workload loop (looprun.go, product/loops.json): no model.
	r.RegisterFunc("v3_loop_run", tr.TierBlock, v3LoopRun)
	r.RegisterFunc("v3_loop_replay", tr.TierBlock, v3LoopReplay)
	// One run's ports, work directory and NQN prefix (runplan.go), for concurrent runs.
	r.RegisterFunc("v3_run_plan", tr.TierDevOps, v3RunPlan)
	for name, f := range map[string]tr.ActionHandlerFunc{
		"v3_op_create": blockActions.Handler("v3_op_create"), "v3_op_attach": blockActions.Handler("v3_op_attach"), "v3_op_write": blockActions.Handler("v3_op_write"), "v3_op_read": blockActions.Handler("v3_op_read"),
		"v3_op_snapshot": blockActions.Handler("v3_op_snapshot"), "v3_op_snapshot_delete": blockActions.Handler("v3_op_snapshot_delete"), "v3_op_restore": blockActions.Handler("v3_op_restore"),
		"v3_op_release": blockActions.Handler("v3_op_release"), "v3_op_stop": blockActions.Handler("v3_op_stop"), "v3_op_move": blockActions.Handler("v3_op_move"), "v3_op_delete": blockActions.Handler("v3_op_delete"),
		"v3_op_observe": blockActions.Handler("v3_op_observe"), "v3_op_flush": blockActions.Handler("v3_op_flush"),
		// Process faults (faults.go): kill and restart by recorded identity, and the healthy observation.
		"v3_op_kill": blockActions.Handler("v3_op_kill"), "v3_op_restart": blockActions.Handler("v3_op_restart"), "v3_op_restart_master": v3OpRestartMaster, "v3_op_healthy": v3OpHealthy,
	} {
		r.RegisterFunc(name, tr.TierBlock, f)
	}
}
