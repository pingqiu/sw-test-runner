package v3block

// Frozen copies of the hand-over Go handlers that product/actions.json's
// sequence actions replaced (v3_op_stop, v3_op_move), as they were before
// the move (feat/v3block-m2 at 4fb5f3d), with frozen copies of every helper
// the move changed or removed (the guest disconnect, the detach answer, the
// hand-over itself). The workspace command, the outcome table and the
// recovery bookkeeping are called live. The equivalence tests
// (sequence_equiv_test.go) run each against its declared action. Test code
// only: nothing registers them.

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/declared"
)

func nvmeConfirmGoneFrozen(ctx context.Context, node runner, nqn string) string {
	end := time.Now().Add(5 * time.Second)
	for {
		state, _, why := nvmeScanFrozen(ctx, node, nqn)
		switch {
		case state == "absent":
			return ""
		case state == "unreadable":
			return "nvme disconnect: cannot confirm: " + why
		case time.Now().After(end):
			return "nvme disconnect: a controller of " + nqn + " remains"
		}
		if !pause(ctx, 250*time.Millisecond) {
			return "nvme disconnect: a controller of " + nqn + " remains at the runner deadline"
		}
	}
}

func nvmeDisconnectCountFrozen(ctx context.Context, node runner, nqn string) (int, string) {
	cmd := fmt.Sprintf(`n=0; for d in /sys/class/nvme/nvme*; do [ -e "$d" ] || continue; q=$(cat "$d/subsysnqn") || exit 3; [ "$q" = %s ] || continue; echo 1 > "$d/delete_controller" || exit 4; n=$((n+1)); done; echo "deleted $n"`, shellQuote(nqn))
	stdout, stderr, code, err := node.RunRoot(ctx, cmd)
	if err != nil || code != 0 {
		return -1, fmt.Sprintf("nvme disconnect: exit %d: %s", code, firstLine(stderr))
	}
	n := -1
	if f := strings.Fields(stdout); len(f) == 2 && f[0] == "deleted" {
		n, _ = strconv.Atoi(f[1])
	}
	why := nvmeConfirmGoneFrozen(ctx, node, nqn)
	return n, why
}

func parseDetachFrozen(stdout, stderr, vol string, epoch uint64) (uint64, error) {
	var a struct {
		VolumeID  string `json:"volumeID"`
		HeadGen   uint64 `json:"headGen"`
		HeadEpoch uint64 `json:"headEpoch"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &a); err != nil {
		return 0, fmt.Errorf("detach answer is not JSON: %q", firstLine(stdout))
	}
	if a.VolumeID != vol || a.HeadGen == 0 || a.HeadEpoch != epoch {
		return 0, fmt.Errorf("detach answer %+v; want %s at epoch %d with a generation", a, vol, epoch)
	}
	if strings.TrimSpace(stderr) != fmt.Sprintf("sealed HEAD generation: %d", a.HeadGen) {
		return 0, fmt.Errorf("detach stderr %q does not state generation %d", firstLine(stderr), a.HeadGen)
	}
	return a.HeadGen, nil
}

func (e opEnv) disconnectFrozen(ctx context.Context, node runner, v *opVolume) string {
	n, why := nvmeDisconnectCountFrozen(ctx, node, v.NQN)
	if ref, ok := declared.OpOf(ctx); ok {
		var controllers any
		if n >= 0 {
			controllers = n
		}
		res := map[string]any{"nqn": v.NQN, "controllers": controllers, "ok": why == ""}
		if why != "" {
			res["detail"] = why
		}
		_ = ref.History.Observe(tr.HistoryObserve, tr.HistoryRecord{Client: "runner", Verb: "disconnect", Volume: v.Name, Result: res})
	}
	return why
}

func (e opEnv) handOverFrozen(ctx context.Context, node runner, v *opVolume, op string) (map[string]any, string) {
	res := map[string]any{"epoch": v.Epoch}
	if why := e.disconnectFrozen(ctx, node, v); why != "" {
		return res, why
	}
	stdout, stderr, code, err := node.Run(ctx, fmt.Sprintf("%s ops chunk detach --plain --volume %s --status-addr %s --timeout 120s", shellQuote(e.cli()), shellQuote(v.Name), shellQuote(v.Hosts[v.Cur].Status)))
	if err != nil {
		return res, err.Error()
	}
	if r, d := chunkOutcome(chunkDetach, stdout, code); r != tr.OutcomeOK {
		res["detach"] = r
		return res, "detach: " + d
	}
	gen, perr := parseDetachFrozen(stdout, stderr, v.Name, v.Epoch)
	if perr != nil {
		return res, perr.Error()
	}
	res["sealed_gen"] = gen
	out, errOut, code, err := e.workspace(ctx, node, v.Name, "revoke", "--replica", "r1", "--epoch", strconv.FormatUint(v.Epoch, 10), "--sealed-gen", strconv.FormatUint(gen, 10))
	if err != nil || wsOutcome(op, wsRevoke, code) != tr.OutcomeOK {
		res["revoke_exit"] = code
		return res, "revoke: " + firstLine(out+" "+errOut)
	}
	if why := waitGoneFrozen(ctx, node, v.PID, v.PIDStart, 40*time.Second); why != "" {
		return res, "the holder did not exit by itself after the revoke: " + why
	}
	v.PID, v.PIDStart = "", ""
	ack := regexp.MustCompile(fmt.Sprintf(`(?m)^recorded ACK: r1@%d HEAD generation (\d+)\r?$`, v.Epoch))
	for end := time.Now().Add(30 * time.Second); time.Now().Before(end); {
		out, _, c, e2 := e.workspace(ctx, node, v.Name, "show")
		if m := ack.FindStringSubmatch(out); e2 == nil && c == 0 && m != nil {
			res["ack_gen"] = m[1]
			return res, ""
		}
		if !pause(ctx, 500*time.Millisecond) {
			break
		}
	}
	return res, fmt.Sprintf("no recorded ACK of r1@%d", v.Epoch)
}

func v3OpStopFrozen(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
	node, e, v, err := opBegin(actx, act, "v3_op_stop")
	if err != nil {
		return nil, err
	}
	defer saveVolume(actx, v)
	return lifecycle(v, intentNotHeld)(recordBounded(ctx, actx, e, "stop", v.Name, map[string]any{"server": v.Hosts[v.Cur].Server, "epoch": v.Epoch}, func(ctx context.Context) (string, map[string]any) {
		res, why := e.handOverFrozen(ctx, node, v, wsStop)
		if why != "" {
			res["detail"] = why
			return tr.OutcomeUnknown, res
		}
		return tr.OutcomeOK, res
	}))
}

var reMovedToFrozen = regexp.MustCompile(`^clean move issued: volume (\S+) request (\S+): r1@(\d+) -> r1@(\d+) on (\S+) \(sealed HEAD gen (\d+), remote LSN (\d+)\)\r?$`)

func v3OpMoveFrozen(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
	node, e, v, err := opBegin(actx, act, "v3_op_move")
	if err != nil {
		return nil, err
	}
	to := 1 - v.Cur
	dst := v.Hosts[to]
	req := v.Name + "-move-" + strconv.FormatUint(v.Epoch, 10)
	defer saveVolume(actx, v)
	return lifecycle(v, intentNotHeld)(recordBounded(ctx, actx, e, "move", v.Name, map[string]any{"from": v.Hosts[v.Cur].Server, "to": dst.Server, "epoch": v.Epoch, "request": req}, func(ctx context.Context) (string, map[string]any) {
		res, why := e.handOverFrozen(ctx, node, v, wsMove)
		if why != "" {
			res["detail"] = "stop: " + why
			return tr.OutcomeUnknown, res
		}
		out, errOut, code, err := e.workspace(ctx, node, v.Name, "move", "--clean", "--replica", "r1", "--epoch", strconv.FormatUint(v.Epoch, 10),
			"--target-host", dst.Server, "--target-data", dst.Data, "--target-ctrl", dst.Ctrl, "--request-id", req)
		if err != nil {
			return fail(err.Error())
		}
		res["exit"] = code
		if r := wsOutcome(wsMove, wsMoveClean, code); r != tr.OutcomeOK {
			res["detail"] = firstLine(out + " " + errOut)
			return r, res
		}
		first, _, _ := strings.Cut(out, "\n")
		m := reMovedToFrozen.FindStringSubmatch(first)
		if m == nil || m[1] != v.Name || m[2] != req || m[3] != strconv.FormatUint(v.Epoch, 10) || m[5] != dst.Server {
			res["detail"] = "the move answer does not name this move: " + firstLine(out)
			return tr.OutcomeUnknown, res
		}
		res["new_epoch"], res["head_gen"], res["remote_lsn"] = m[4], m[6], m[7]
		v.Cur = to
		return tr.OutcomeOK, res
	}))
}
