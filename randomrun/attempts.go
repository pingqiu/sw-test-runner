package randomrun

// The driver's rerun: an action whose description declares one (an operator
// page tells the operator to run the same command again for an answer) is
// RE-ISSUED by the driver, never looped inside the action. Each attempt is
// its own handler call and its own operation in the history (an invoke and a
// complete with "attempt": n). Only a REFUSED attempt is re-issued, never
// an unknown, and the subject is then observed twice: right after it (the
// "after" of attempt n) and right before attempt n+1, after the pause (its
// "before"). The checkers judge every refused attempt as they judge any
// refusal, and the two observations are a baseline pair across the pause.
// The last attempt's outcome is the call's.

import (
	"context"
	"fmt"
	"time"

	tr "github.com/pingqiu/sw-test-runner"
)

// VerbRerunGuard is the observe record the driver writes when the
// wall-clock guard (not the count) ended a request's attempts: {action,
// attempt, elapsed_ms}, on the subject. The checkers take the limit that
// request probed as not judged (C5 UNKNOWN).
const VerbRerunGuard = "rerun_guard"

// Attempts runs one call act of handler h on subject: one attempt, or, when
// rr declares a rerun of act.Action (RequestArg not ""), attempt 1 and,
// while rr.Rerun says so, attempt n+1 after its pause, each with
// AttemptParam set. observe records the subject's observation; it runs
// right after a refused attempt that is re-issued and again right before
// the next attempt (none: nothing is observed).
// When the guard ends the attempts, a VerbRerunGuard record says so. It returns the last
// attempt's output and the number of attempts made (0 for an action with no
// declared rerun). A step that sets AttemptParam itself is a runner error:
// only the driver numbers attempts. A context ended during a pause returns
// the last attempt's output, as an attempt that was not made.
func Attempts(ctx context.Context, actx *tr.ActionContext, h tr.ActionHandlerFunc, act tr.Action, rr Rerunner, subject string, observe func() error) (map[string]string, int, error) {
	if _, set := act.Params[AttemptParam]; set {
		return nil, 0, fmt.Errorf("%s: %s is the driver's to set, not a step's", act.Action, AttemptParam)
	}
	if rr == nil || rr.RequestArg(act.Action) == "" {
		out, err := h(ctx, actx, act)
		return out, 0, err
	}
	start := time.Now()
	for n := 1; ; n++ {
		p := make(map[string]string, len(act.Params)+1)
		for k, v := range act.Params {
			p[k] = v
		}
		p[AttemptParam] = fmt.Sprint(n)
		call := act
		call.Params = p
		out, err := h(ctx, actx, call)
		if err != nil {
			return nil, n, err
		}
		elapsed := time.Since(start)
		again, pause, why := rr.Rerun(act.Action, act.Params, n, elapsed, out)
		if !again {
			if why == "guard" {
				actx.Log("  %s: the wall-clock guard ended the reruns after attempt %d", act.Action, n)
				if err := actx.History.Observe(tr.HistoryObserve, tr.HistoryRecord{Client: "gen", Verb: VerbRerunGuard, Volume: subject,
					Result: map[string]any{"action": act.Action, "attempt": n, "elapsed_ms": elapsed.Milliseconds()}}); err != nil {
					return nil, n, err
				}
			}
			return out, n, nil
		}
		// Only a refused attempt is re-issued (Rerun). Observe right after
		// it (its "after") and again right before the next attempt, after
		// the pause (the next one's "before"): the two are a baseline pair,
		// so work the product does in the background during the pause shows
		// as a baseline difference, blamed on neither attempt.
		if observe != nil {
			if err := observe(); err != nil {
				return nil, n, err
			}
		}
		select {
		case <-ctx.Done():
			return out, n, nil
		case <-time.After(pause):
		}
		if observe != nil {
			if err := observe(); err != nil {
				return nil, n, err
			}
		}
	}
}
