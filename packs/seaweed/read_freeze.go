package seaweed

import (
	"context"
	"encoding/json"
	"errors"
	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/inventory"
	"time"
)

// Freeze shares the admitted read-only seed/load/oracle; HTTP controls are deferred.
func (r *runtimePack) freeze(ctx context.Context, a *tr.ActionContext, p Plan, d *inventory.Document, in tr.PackInputs, s *session, load tr.JobHandle) (map[string]string, error) {
	x := p.Restart
	victim := s.jobs[in.Pause.Victim]
	if _, err := a.PauseApprovedPackGeneration(ctx, x.Episode, victim); err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(20 * time.Second):
	}
	receipt, err := a.ResumeApprovedPackGeneration(ctx, x.Episode, victim)
	if err != nil {
		return nil, err
	}
	e, err := a.WaitApprovedPackTask(ctx, load)
	if err != nil {
		return nil, err
	}
	var c SmokeConfig
	_ = json.Unmarshal([]byte(in.Jobs[x.DC].Argv[3]), &c)
	observed, err := CheckReadTaskOutput(e, c, "load", "dc", ReadWindow{receipt.PreStopMono})
	if err != nil {
		return nil, err
	}
	if err = checkFreezeExposure(observed, freezeWindow{receipt.PreStopMono, receipt.StoppedMono, receipt.PreContMono, receipt.ResumedMono}); err != nil {
		return nil, err
	}
	if _, err = readTask(ctx, a, in, x.RecoverDC, "recovery", "dc"); err != nil {
		return nil, err
	}
	return admittedResult("completed", "OK", "inspect STOP/CONT, DC overlap/window/counters and fresh DC recovery; HTTP DEFERRED", "seaweed_read_restart", d), nil
}

// freezeWindow holds one client-clock marker set: P before STOP, S after the
// stopped witness, C before CONT and R after the LIVE witness.
type freezeWindow struct{ PreStop, Stopped, PreCont, Resumed float64 }

// checkFreezeExposure requires a >=20 s hold, one GET spanning [S,C] and one
// GET starting after R. The lab and cloud cases share it unchanged.
func checkFreezeExposure(observed map[string]any, w freezeWindow) error {
	covered, continued := false, false
	for _, value := range observed["outcomes"].([]any) {
		row := value.(map[string]any)
		covered = covered || row["started"].(float64) <= w.PreCont && row["ended"].(float64) >= w.Stopped
		continued = continued || row["started"].(float64) > w.Resumed
	}
	if w.Stopped <= w.PreStop || w.PreCont-w.Stopped < 20 || !covered || !continued {
		return errors.New("freeze NOT_COVERED: missing hold/overlap/post-LIVE GET")
	}
	return nil
}
