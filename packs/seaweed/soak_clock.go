package seaweed

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"path"
	"strings"

	tr "github.com/pingqiu/sw-test-runner"
)

// clockSample is one paired M01/M02 clock capture around an existing M02 job
// (decision 3 r6 section 2): T1 and T4 from M01 CLOCK_MONOTONIC, entry and exit
// from the job's own first and last statements, so job residence leaves the
// bracket. The assessor judges it; a sample only records what was observed.
type clockSample struct {
	BootID     string  `json:"boot_id"`
	RunID      string  `json:"run_id"`
	T1         float64 `json:"t1"`
	T4         float64 `json:"t4"`
	EntryEpoch float64 `json:"entry_epoch"`
	ExitEpoch  float64 `json:"exit_epoch"`
	EntryMono  float64 `json:"entry_mono"`
	ExitMono   float64 `json:"exit_mono"`
}

// clockPublish is publishEvidence, for the M02 evidence and clock files; the CPU fixtures have no M01 run root.
var clockPublish = publishEvidence

// clockEvidence builds a sample from the job's strict one-row output.
func clockEvidence(row []byte, runID string, t1, t4 float64) ([]byte, error) {
	var got struct {
		Clock *clockSample `json:"clock"`
	}
	if oneStrictValue(row) != nil || json.NewDecoder(bytes.NewReader(row)).Decode(&got) != nil || got.Clock == nil {
		return nil, errors.New("soak job row has no clock capture")
	}
	s := *got.Clock
	s.RunID, s.T1, s.T4 = runID, t1, t4
	for _, v := range []float64{t1, t4, s.EntryEpoch, s.ExitEpoch, s.EntryMono, s.ExitMono} {
		if math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 {
			return nil, errors.New("soak clock capture is not finite and positive")
		}
	}
	if s.BootID == "" {
		return nil, errors.New("soak clock capture has no boot id")
	}
	return json.Marshal(s)
}

func publishClock(root, name, runID string, row []byte, t1, t4 float64) error {
	body, err := clockEvidence(row, runID, t1, t4)
	if err != nil {
		return err
	}
	return clockPublish(path.Join(root, name), body)
}

// authorizeT1 is the soak sample's t1 (plan r2 step 2): the controller clock immediately before this job's
// underlying Authorize, after its final successful readback, bound to the job's settled exit identity.
var authorizeT1 = func(a *tr.ActionContext, h tr.JobHandle, e tr.ExitInfo) (float64, error) {
	return a.ApprovedAuthorizeMono(h, e)
}

// publishSoakClock publishes the sample the assessor reads, with t1 at authorization, and beside it the same
// exchange's job-envelope sample (t1 before the whole job call, as through r8b). Both come from one exchange, so
// the authorize sample's interval lies inside the envelope's by construction; the envelope is kept as the
// pre-registered old/new comparison, never clipped or substituted.
func publishSoakClock(root, name, runID string, row []byte, envelopeT1, authT1, t4 float64) error {
	if !(envelopeT1 <= authT1 && authT1 < t4) {
		return errors.New("authorize stamp outside the job envelope")
	}
	if err := publishClock(root, strings.TrimSuffix(name, ".json")+"-envelope.json", runID, row, envelopeT1, t4); err != nil {
		return err
	}
	return publishClock(root, name, runID, row, authT1, t4)
}
