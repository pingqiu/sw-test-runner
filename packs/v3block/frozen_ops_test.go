package v3block

// Frozen copies of the Go handlers that product/actions.json replaced, as
// they were before the move (feat/v3block-m2 at 2302aab). The equivalence
// tests (declared_equiv_test.go) run each against its declared action on the
// same fake answers and require identical records, outcomes, commands and
// runner state. They are test code only: nothing registers them.

import (
	"context"
	"errors"
	"fmt"

	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/actions"
)

// v3OpCreateFrozen: one timed CreateVolume call (VolumeCreate.MarkReady). Param:
// size_bytes (default 16 MiB).
func v3OpCreateFrozen(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
	node, e, v, err := opBegin(actx, act, "v3_op_create")
	if err != nil {
		return nil, err
	}
	size := actions.ParseInt(optionalParam(act, "size_bytes"), 16<<20)
	defer saveVolume(actx, v)
	return record(actx, e, "create", v.Name, map[string]any{"size_bytes": size}, func() (string, map[string]any) {
		stdout, stderr, code, err := node.Run(ctx, fmt.Sprintf("%s create --master %s --volume %s --size %d", shellQuote(e.v3ctl()), shellQuote(e.master), shellQuote(v.Name), size))
		if err != nil {
			return fail(err.Error())
		}
		a, r := parseV3ctl(stdout, code, v.Name)
		res := map[string]any{"call_ns": a.CallNs}
		if r != tr.OutcomeOK {
			res["detail"] = firstLine(a.Error + " " + stderr)
			return r, res
		}
		v.LayoutID, res["layout_id"] = a.LayoutID, a.LayoutID
		return r, res
	})
}

// v3OpSnapshotFrozen takes a named snapshot through the holder (SnapshotRemoval.Commit).
// Param: name.
func v3OpSnapshotFrozen(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
	node, e, v, err := opBegin(actx, act, "v3_op_snapshot")
	if err != nil {
		return nil, err
	}
	name := requireParam(act, actx, "name")
	if name == "" {
		return nil, errors.New("v3_op_snapshot: name is required")
	}
	return record(actx, e, "snapshot", v.Name, map[string]any{"name": name}, func() (string, map[string]any) {
		stdout, _, code, err := node.Run(ctx, fmt.Sprintf("%s ops chunk snapshot --volume %s --status-addr %s --name %s --request-id %s", shellQuote(e.cli()),
			shellQuote(v.Name), shellQuote(v.Hosts[v.Cur].Status), shellQuote(name), shellQuote(v.Name+"-"+name)))
		if err != nil {
			return fail(err.Error())
		}
		return fromChunk(chunkSnapshot, stdout, code, nil)
	})
}

// v3OpRestoreFrozen creates the target volume from a snapshot of this volume
// through the master (RestoreTarget.CreateHead). Params: snapshot, target_volume ("target" is the parser's iSCSI field).
func v3OpRestoreFrozen(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
	node, e, v, err := opBegin(actx, act, "v3_op_restore")
	if err != nil {
		return nil, err
	}
	snap, target := requireParam(act, actx, "snapshot"), requireParam(act, actx, "target_volume")
	if snap == "" || target == "" {
		return nil, errors.New("v3_op_restore: snapshot and target_volume are required")
	}
	t, err := loadVolume(actx, target)
	if err != nil {
		return nil, fmt.Errorf("v3_op_restore: %w", err)
	}
	defer saveVolume(actx, t)
	return record(actx, e, "restore", target, map[string]any{"source": v.Name, "snapshot": snap}, func() (string, map[string]any) {
		stdout, _, code, err := node.Run(ctx, fmt.Sprintf("%s ops chunk restore --source %s --snapshot %s --target %s --master %s --request-id %s", shellQuote(e.cli()),
			shellQuote(v.Name), shellQuote(snap), shellQuote(target), shellQuote(e.master), shellQuote("restore-"+target)))
		if err != nil {
			return fail(err.Error())
		}
		return fromChunk(chunkRestore, stdout, code, nil)
	})
}

// v3OpReleaseFrozen removes a deleted restore target from this volume's snapshot
// dependents (RestoreTarget.OpRelease). Param: target_volume.
func v3OpReleaseFrozen(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
	node, e, v, err := opBegin(actx, act, "v3_op_release")
	if err != nil {
		return nil, err
	}
	target := requireParam(act, actx, "target_volume")
	if target == "" {
		return nil, errors.New("v3_op_release: target_volume is required")
	}
	return record(actx, e, "release", v.Name, map[string]any{"target": target}, func() (string, map[string]any) {
		stdout, _, code, err := node.Run(ctx, fmt.Sprintf("%s ops chunk release --source %s --target %s --master %s --chunk-filer %s --chunk-path %s", shellQuote(e.cli()),
			shellQuote(v.Name), shellQuote(target), shellQuote(e.master), shellQuote(e.filer), shellQuote(e.chunkPath)))
		if err != nil {
			return fail(err.Error())
		}
		return fromChunk(chunkRelease, stdout, code, nil)
	})
}
