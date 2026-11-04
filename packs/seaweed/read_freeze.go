package seaweed

import (
	"errors"
)

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
