package seaweed

import "errors"

// The frozen first-encode uniform layout binds every needle to its data span.
// Only data0 plus all four parity holders are absent in the degraded cell.
func ecAssessUnderQuorum(b ecReadBody, route string, s ecSnapshot, degraded bool) error {
	l := s.Layout
	if b.RunID != s.Manifest.RunID || b.Control != "" || len(l) != 3 || (l[0] != 3 && l[0] != 4) || l[2] <= 0 || l[1] != ((l[2]+10*1048576-1)/(10*1048576))*1048576 || len(s.Intervals) != len(s.Manifest.Objects) {
		return errors.New("EC under-quorum layout/scope invalid")
	}
	targets := 0
	for i, o := range s.Manifest.Objects {
		span := s.Intervals[o.Key]
		if len(span) != 2 || span[0] <= 0 || span[1] < int64(o.Length) || span[0] > l[2]-span[1] {
			return errors.New("EC needle interval invalid")
		}
		target := span[0] < l[1]
		if target {
			targets++
		}
		if i >= len(b.Outcomes) { // Still validate the frozen cohort before rejecting an incomplete read.
			continue
		}
		r := b.Outcomes[i]
		if r.Route != route || r.Key != o.Key || r.Op != i || r.Kind != "s3_get" || r.Original != nil || !(r.Started >= 0 && r.Ended >= r.Started && r.Ended-r.Started <= 30) {
			return errors.New("EC under-quorum read binding/deadline invalid")
		}
		if r.Status == 200 && (r.Length != o.Length || r.SHA256 != o.SHA256) {
			return errors.New("P1 EC product: unplanted 200 with wrong bytes")
		}
		if !degraded {
			continue
		}
		if target {
			if r.Exact || !((r.Status >= 500 && r.Status <= 599) || (r.Status == 0 && r.Error != "")) {
				return errors.New("EC unavailable target did not fail closed")
			}
		} else if r.Status != 200 || !r.Exact || r.Error != "" {
			return errors.New("EC recoverable control not exact")
		}
	}
	if targets == 0 || targets == len(s.Manifest.Objects) {
		return errors.New("NOT_COVERED: need unavailable targets and recoverable controls")
	}
	if !degraded {
		return ecAssessRead(b, route, s.Manifest, false)
	}
	if b.Wrong != 0 || len(b.Outcomes) != len(s.Manifest.Objects) {
		return errors.New("EC under-quorum terminal incomplete")
	}
	return nil
}
