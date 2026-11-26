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
}
