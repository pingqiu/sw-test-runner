package v3block

// Frozen copies of the Go handlers that product/actions.json replaced, as
// they were before the move (feat/v3block-m2 at 2302aab). The equivalence
// tests (declared_equiv_test.go) run each against its declared action on the
// same fake answers and require identical records, outcomes, commands and
// runner state. They are test code only: nothing registers them.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

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

// v3OpSnapshotDeleteFrozen deletes a named snapshot (SnapshotRemoval.DeleteSnap):
// through the holder while it serves (the product cleans right after such a
// delete), store direct with `sw-block ops chunk snapshot-delete` when no
// holder runs (cleaned at the next periodic pass, the product's guide says).
// Param: name.
func v3OpSnapshotDeleteFrozen(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
	node, e, v, err := opBegin(actx, act, "v3_op_snapshot_delete")
	if err != nil {
		return nil, err
	}
	name := requireParam(act, actx, "name")
	if name == "" {
		return nil, errors.New("v3_op_snapshot_delete: name is required")
	}
	via := "store"
	if alive(ctx, node, v.PID, v.PIDStart) {
		via = "holder"
	}
	return record(actx, e, "snapshot_delete", v.Name, map[string]any{"name": name, "via": via}, func() (string, map[string]any) {
		if via == "holder" {
			body, _ := json.Marshal(map[string]string{"volumeID": v.Name, "name": name})
			out, _, code, err := node.Run(ctx, fmt.Sprintf("curl -s -m 60 -X POST -H 'Content-Type: application/json' --data %s -w '\\n%%{http_code}' %s",
				shellQuote(string(body)), shellQuote("http://"+v.Hosts[v.Cur].Status+"/chunk/snapshot/delete")))
			if err != nil || code != 0 {
				return fail(fmt.Sprintf("holder not answering (curl exit %d)", code))
			}
			i := strings.LastIndex(strings.TrimRight(out, "\n"), "\n")
			status := 1
			if strings.TrimSpace(out[i+1:]) == "200" {
				status = 0
			}
			return fromChunk(chunkSnapDelHolder, out[:max(i, 0)], status, nil)
		}
		stdout, _, code, err := node.Run(ctx, fmt.Sprintf("%s ops chunk snapshot-delete --volume %s --name %s --master %s --chunk-filer %s --chunk-path %s", shellQuote(e.cli()),
			shellQuote(v.Name), shellQuote(name), shellQuote(e.master), shellQuote(e.filer), shellQuote(e.chunkPath)))
		if err != nil {
			return fail(err.Error())
		}
		return fromChunk(chunkSnapDelStore, stdout, code, nil)
	})
}

// v3OpDeleteFrozen deletes the volume through the master's delete authority
// (MoveAdmission.DelMark, `ops chunk volume-delete`). After a stop the master
// admits the delete only once the holder's lease horizon plus the clock-skew
// margin has passed, and answers not_admitted until then; the CLI's own
// procedure is to rerun with the SAME request id once the cause is cleared.
// So one operation reruns it while the answer is not_admitted, within
// admit_seconds (default 120), and records every attempt. Its outcome is the
// last answer's. A step that declares expect=refused (a negative: the
// refusal is what it probes) is one call, never rerun: rerunning would wait
// out the refusal it tests.
func v3OpDeleteFrozen(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
	node, e, v, err := opBegin(actx, act, "v3_op_delete")
	if err != nil {
		return nil, err
	}
	bound := time.Duration(actions.ParseInt(optionalParam(act, "admit_seconds"), 120)) * time.Second
	req := "delete-" + v.Name
	return record(actx, e, "delete", v.Name, map[string]any{"layout_id": v.LayoutID, "request": req}, func() (string, map[string]any) {
		return e.deleteVolumeFrozen(ctx, node, v, req, bound)
	})
}

// deleteRetryEveryFrozen is the pause between two attempts of one delete.
var deleteRetryEveryFrozen = 3 * time.Second

// deleteVolumeFrozen runs `ops chunk volume-delete` once, and again while the
// answer is not_admitted within bound, unless the step declares
// expect=refused. Its outcome is the last answer's; every attempt is listed.
func (e opEnv) deleteVolumeFrozen(ctx context.Context, node runner, v *opVolume, req string, bound time.Duration) (string, map[string]any) {
	end := time.Now().Add(bound)
	var attempts []string
	for {
		stdout, _, code, err := node.Run(ctx, fmt.Sprintf("%s ops chunk volume-delete --volume %s --master %s --chunk-filer %s --chunk-path %s --request-id %s", shellQuote(e.cli()),
			shellQuote(v.Name), shellQuote(e.master), shellQuote(e.filer), shellQuote(e.chunkPath), shellQuote(req)))
		if err != nil {
			return tr.OutcomeUnknown, map[string]any{"detail": err.Error(), "attempts": attempts}
		}
		r, res := fromChunk(chunkVolumeDelete, stdout, code, nil)
		d, _ := res["detail"].(string)
		attempts = append(attempts, fmt.Sprintf("%s %s", r, firstLine(d)))
		res["attempts"] = attempts
		if r == tr.OutcomeOK || e.expect == tr.OutcomeRefused || !strings.HasPrefix(d, "not_admitted:") || time.Now().After(end) {
			return r, res
		}
		select {
		case <-ctx.Done():
			return r, res
		case <-time.After(deleteRetryEveryFrozen):
		}
	}
}

// record is the unbounded record() the frozen handlers called.
func record(actx *tr.ActionContext, e opEnv, verb, vol string, args map[string]any, do func() (string, map[string]any)) (map[string]string, error) {
	if e.expect != "" {
		if args == nil {
			args = map[string]any{}
		}
		args["expect"] = e.expect
	}
	seq, err := actx.History.Invoke(tr.HistoryRecord{Client: e.client, Op: opIDs[verb], Verb: verb, Volume: vol, Args: args})
	if err != nil {
		return nil, err
	}
	outcome, result := do()
	if err := actx.History.Complete(seq, outcome, result); err != nil {
		return nil, err
	}
	actx.Log("  v3_op_%s(%s): %s", verb, vol, outcome)
	return map[string]string{"value": outcome}, nil
}

// v3OpObserveFrozen records what the product shows of the volume: HEAD from `ops
// chunk show` (or its error code), the authority line from `ops workspace
// show`, and every file under the volume's directory in the filer. An
// observation is a fact for the checkers, never a verdict.
func v3OpObserveFrozen(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
	node, e, v, err := opBegin(actx, act, "v3_op_observe")
	if err != nil {
		return nil, err
	}
	// An observation has no outcome; its runner deadline ends its commands.
	ctx, cancel := context.WithTimeout(ctx, e.deadline)
	defer cancel()
	res := map[string]any{}
	stdout, _, code, err := node.Run(ctx, fmt.Sprintf("%s ops chunk show --volume %s --master %s --chunk-filer %s --chunk-path %s", shellQuote(e.cli()),
		shellQuote(v.Name), shellQuote(e.master), shellQuote(e.filer), shellQuote(e.chunkPath)))
	observeChunkShow(res, stdout, code, err)
	if out, _, c, e2 := e.workspace(ctx, node, v.Name, "show"); e2 == nil {
		res["workspace"] = map[string]any{"exit": c, "text": firstLines(out, 12)}
	}
	// The volume's directories in the filer, whatever its layout: a name that
	// never existed is observed as having none.
	if dirs, why := listDirs(ctx, node, e.filer, e.chunkPath, v.Name+"-"); why != "" {
		res["dirs_error"] = why
	} else {
		res["dirs"] = dirs
	}
	if v.LayoutID != "" {
		files, why := listFiles(ctx, node, e.filer, e.chunkPath, v.Name+"-"+v.LayoutID[:8])
		if why != "" {
			res["listing_error"] = why
		} else {
			res["files"] = files
		}
	}
	return map[string]string{"value": "observed"}, actx.History.Observe(tr.HistoryObserve, tr.HistoryRecord{Client: e.client, Volume: v.Name, Verb: "observe", Result: res})
}
