package v3block

// Equivalence of the declared sequence actions (v3_op_stop, v3_op_move)
// with the Go handlers they replaced (frozen_sequence_test.go), on the same
// scripted answers, compared as the process actions are
// (process_equiv_test.go). The waits whose bounds are the product's own
// (the holder's exit within 40 s, the ACK within 30 s) are ended by the
// operation's runner deadline instead, set short for both.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	tr "github.com/pingqiu/sw-test-runner"
)

func chunkBody(code, msg string) eqAnswer {
	b, _ := json.Marshal(map[string]any{"code": code, "message": msg, "retryable": false})
	return eqAnswer{out: string(b), code: 1}
}

// handOverRules is a good planned stop of v1 at epoch 3 (holder 4242), then
// a good move to b-v1, with the named rule's answers replaced.
func handOverRules(over map[string][]eqAnswer) []eqRule {
	base := []eqRule{
		{"delete_controller", []eqAnswer{{out: "deleted 1\n"}}},
		{"subsysnqn", []eqAnswer{{out: "end\n"}}},
		{"ops chunk detach", []eqAnswer{{out: `{"volumeID":"v1","headGen":7,"headEpoch":3,"cutLSN":3}`, errOut: "sealed HEAD generation: 7\n"}}},
		{"ops workspace revoke", []eqAnswer{{}}},
		{"/proc/4242", []eqAnswer{{out: "absent\n"}}},
		{"ops workspace show", []eqAnswer{{out: "lease state: revoking\nrecorded ACK: r1@3 HEAD generation 9\n"}}},
		{"ops workspace move", []eqAnswer{{out: "clean move issued: volume v1 request v1-move-3: r1@3 -> r1@4 on b-v1 (sealed HEAD gen 7, remote LSN 11)\nmore\n"}}},
	}
	for k, as := range over {
		for i := range base {
			if base[i].match == k {
				base[i].answers = as
			}
		}
	}
	return base
}

func heldVols() []*opVolume {
	vs := eqVolumes()
	vs[0].PID, vs[0].PIDStart, vs[0].Epoch = "4242", "100", 3
	vs[0].Extra = map[string]string{"intent": intentHeld}
	return vs
}

func seqCases(extra []eqCase) []eqCase {
	p := map[string]string{"volume": "v1"}
	c := func(name string, over map[string][]eqAnswer) eqCase {
		return eqCase{name: name, params: p, vols: heldVols(), rules: handOverRules(over)}
	}
	cases := []eqCase{
		c("ok", nil),
		c("disconnect fails", map[string][]eqAnswer{"delete_controller": {{errOut: "write error\nx", code: 4}}}),
		c("disconnect not confirmed", map[string][]eqAnswer{"subsysnqn": {{errOut: "boom", code: 3}}}),
		c("no controller to delete", map[string][]eqAnswer{"delete_controller": {{out: "deleted 0\n"}}}),
		c("disconnect answer odd", map[string][]eqAnswer{"delete_controller": {{out: "what\n"}}}),
		c("detach not run", map[string][]eqAnswer{"ops chunk detach": {{err: broken}}}),
		c("detach refused by the holder", map[string][]eqAnswer{"ops chunk detach": {chunkBody("not_attached", "no session")}}),
		c("detach without a body", map[string][]eqAnswer{"ops chunk detach": {{out: "panic", code: 2}}}),
		c("detach answer not JSON", map[string][]eqAnswer{"ops chunk detach": {{out: "sealed\nmore", errOut: "sealed HEAD generation: 7"}}}),
		c("detach answer another volume", map[string][]eqAnswer{"ops chunk detach": {{out: `{"volumeID":"v2","headGen":7,"headEpoch":3}`, errOut: "sealed HEAD generation: 7"}}}),
		c("detach answer another epoch", map[string][]eqAnswer{"ops chunk detach": {{out: `{"volumeID":"v1","headGen":7,"headEpoch":4}`, errOut: "sealed HEAD generation: 7"}}}),
		c("detach answer no generation", map[string][]eqAnswer{"ops chunk detach": {{out: `{"volumeID":"v1","headEpoch":3}`, errOut: "sealed HEAD generation: 0"}}}),
		c("detach answer a string generation", map[string][]eqAnswer{"ops chunk detach": {{out: `{"volumeID":"v1","headGen":"7","headEpoch":3}`, errOut: "sealed HEAD generation: 7"}}}),
		c("detach stderr another generation", map[string][]eqAnswer{"ops chunk detach": {{out: `{"volumeID":"v1","headGen":7,"headEpoch":3}`, errOut: "sealed HEAD generation: 8\nmore"}}}),
		c("detach keys in another case", map[string][]eqAnswer{"ops chunk detach": {{out: `{"VolumeId":"v1","HEADGEN":7,"headepoch":3}`, errOut: "sealed HEAD generation: 7"}}}),
		c("revoke refused", map[string][]eqAnswer{"ops workspace revoke": {{out: "lease\n", errOut: "not granted", code: 1}}}),
		c("revoke exit 5", map[string][]eqAnswer{"ops workspace revoke": {{errOut: "x", code: 5}}}),
		c("revoke exit 9", map[string][]eqAnswer{"ops workspace revoke": {{errOut: "y", code: 9}}}),
		c("revoke not run", map[string][]eqAnswer{"ops workspace revoke": {{err: broken}}}),
		c("ack on a later look", map[string][]eqAnswer{"ops workspace show": {{out: "recorded ACK: none\n"}, {out: "recorded ACK: r1@3 HEAD generation 9\r\n"}}}),
		c("ack of another epoch, then this one", map[string][]eqAnswer{"ops workspace show": {{out: "recorded ACK: r1@2 HEAD generation 9\n"}, {out: "recorded ACK: r1@3 HEAD generation 10\n"}}}),
		c("show fails, then answers", map[string][]eqAnswer{"ops workspace show": {{out: "recorded ACK: r1@3 HEAD generation 9\n", code: 1}, {out: "recorded ACK: r1@3 HEAD generation 9\n"}}}),
		{name: "expected refusal recorded", params: map[string]string{"volume": "v1", "expect": "refused"}, vols: heldVols(), rules: handOverRules(nil)},
	}
	cases = append(cases, extra...)
	for _, ce := range commonErrorCases(p, handOverRules(nil)) {
		ce.vols = heldVols()
		cases = append(cases, ce)
	}
	return cases
}

func TestDeclaredStopEquivalent(t *testing.T) {
	checkProcEquivalent(t, "v3_op_stop", v3OpStopFrozen, seqCases(nil))
}

func TestDeclaredMoveEquivalent(t *testing.T) {
	p := map[string]string{"volume": "v1"}
	c := func(name string, over map[string][]eqAnswer) eqCase {
		return eqCase{name: name, params: p, vols: heldVols(), rules: handOverRules(over)}
	}
	fromB := heldVols()
	fromB[0].Cur = 1
	extra := []eqCase{
		c("move not run", map[string][]eqAnswer{"ops workspace move": {{err: broken}}}),
		c("move exit 1", map[string][]eqAnswer{"ops workspace move": {{out: "refused\n", errOut: "not admitted", code: 1}}}),
		c("move exit 7", map[string][]eqAnswer{"ops workspace move": {{errOut: "odd", code: 7}}}),
		c("move answer another request", map[string][]eqAnswer{"ops workspace move": {{out: "clean move issued: volume v1 request v1-move-2: r1@3 -> r1@4 on b-v1 (sealed HEAD gen 7, remote LSN 11)\n"}}}),
		c("move answer another target", map[string][]eqAnswer{"ops workspace move": {{out: "clean move issued: volume v1 request v1-move-3: r1@3 -> r1@4 on a-v1 (sealed HEAD gen 7, remote LSN 11)\n"}}}),
		c("move answer another epoch", map[string][]eqAnswer{"ops workspace move": {{out: "clean move issued: volume v1 request v1-move-3: r1@2 -> r1@4 on b-v1 (sealed HEAD gen 7, remote LSN 11)\n"}}}),
		c("move answer another volume", map[string][]eqAnswer{"ops workspace move": {{out: "clean move issued: volume v9 request v1-move-3: r1@3 -> r1@4 on b-v1 (sealed HEAD gen 7, remote LSN 11)\n"}}}),
		c("move answer garbage", map[string][]eqAnswer{"ops workspace move": {{out: "  ok\n"}}}),
		c("move answer with a carriage return", map[string][]eqAnswer{"ops workspace move": {{out: "clean move issued: volume v1 request v1-move-3: r1@3 -> r1@4 on b-v1 (sealed HEAD gen 7, remote LSN 11)\r\n"}}}),
		{name: "from the second host", params: p, vols: fromB, rules: handOverRules(map[string][]eqAnswer{
			"ops workspace move": {{out: "clean move issued: volume v1 request v1-move-3: r1@3 -> r1@4 on a-v1 (sealed HEAD gen 7, remote LSN 11)\n"}}})},
	}
	checkProcEquivalent(t, "v3_op_move", v3OpMoveFrozen, seqCases(extra))
}

// The product's own waits, ended by the runner deadline in both: a holder
// that does not exit after the revoke, one with no identity recorded, and
// an ACK that never comes.
func TestDeclaredHandOverWaitsEquivalent(t *testing.T) {
	defer func(d time.Duration) { exceptionDeadlineOverride = d }(exceptionDeadlineOverride)
	exceptionDeadlineOverride = 700 * time.Millisecond
	p := map[string]string{"volume": "v1"}
	noID := heldVols()
	noID[0].PIDStart = ""
	cases := []eqCase{
		{name: "the holder keeps running", params: p, vols: heldVols(), rules: handOverRules(map[string][]eqAnswer{"/proc/4242": {{out: "start 100\n"}}})},
		{name: "the holder's state unreadable", params: p, vols: heldVols(), rules: handOverRules(map[string][]eqAnswer{"/proc/4242": {{errOut: "x", code: 3}}})},
		{name: "no start time recorded", params: p, vols: noID, rules: handOverRules(nil)},
		{name: "no ACK", params: p, vols: heldVols(), rules: handOverRules(map[string][]eqAnswer{"ops workspace show": {{out: "recorded ACK: none\n"}}})},
	}
	checkProcEquivalent(t, "v3_op_stop", v3OpStopFrozen, cases)
	checkProcEquivalent(t, "v3_op_move", v3OpMoveFrozen, cases)
}

// Red controls: the check catches a move that does not update the current
// host and a stop whose hand-over skips the disconnect record; the green
// twins pass.
func TestSequenceEquivalenceRedControls(t *testing.T) {
	ok := []eqCase{{name: "ok", params: map[string]string{"volume": "v1"}, vols: heldVols(), rules: handOverRules(nil)}}
	for name, c := range map[string]struct {
		action        string
		frozen, green tr.ActionHandlerFunc
	}{
		"a move that leaves the current host": {"v3_op_move", func(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
			out, err := v3OpMoveFrozen(ctx, actx, act)
			v, _ := loadVolume(actx, "v1")
			v.Cur = 0
			saveVolume(actx, v)
			return out, err
		}, v3OpMoveFrozen},
		"a stop that says ok on a missing ACK": {"v3_op_stop", func(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
			act.Params["expect"] = ""
			out, err := v3OpStopFrozen(ctx, actx, act)
			return map[string]string{"value": strings.ToUpper(out["value"])}, err
		}, v3OpStopFrozen},
	} {
		rep := &countReports{}
		checkProcEquivalentTo(t, rep, c.action, c.frozen, ok)
		if rep.n == 0 {
			t.Errorf("%s: not caught", name)
		}
		rep = &countReports{}
		checkProcEquivalentTo(t, rep, c.action, c.green, ok)
		if rep.n != 0 {
			t.Errorf("%s: green twin: %d reports", name, rep.n)
		}
	}
}
