// Package accounting reports the frozen v3A load windows without retuning them.
package accounting

import (
	"errors"
	"math"
)

type Offer struct {
	Epoch   float64
	Skipped bool
}
type Write struct {
	ID                                              string
	Writer                                          int
	Bytes                                           uint64
	Admitted, Submitted, Completed, Received, ACKed *float64
}
type Read struct {
	Epoch     float64
	Worker    int
	Succeeded bool
}
type Stage struct {
	Count, Bytes              uint64
	PerSecond, BytesPerSecond float64
}
type Timing struct {
	Samples     uint64
	MeanSeconds float64
}
type Window struct {
	Bounds                                                          [2]float64
	Offered, Skipped                                                uint64
	Admitted, Submitted, Completed, ACKed, RCACKed                  Stage
	Reads                                                           [4]uint64
	Coverage, Telemetry                                             string
	SET, QueueAndPreparation, NativeWait, ReceiptAndChecks, Journal Timing
}
type Result struct {
	Coverage string
	Windows  []Window
}

// These baselines and windows are copied from reviewed grill.py b0e5cb99.
// They are deliberately not caller-configurable: failed windows are not retuned.
var policy = [...]struct {
	lo, hi                   float64
	acks, bytes, rc, rcBytes uint64
}{
	{300, 600, 150, 163577941, 112, 120586308},
	{600, 840, 120, 132120644, 90, 100663347},
}

func Evaluate(begin float64, offers []Offer, writes []Write, reads []Read) (Result, error) {
	finite := func(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
	if !finite(begin) {
		return Result{}, errors.New("invalid body clock")
	}
	for _, o := range offers {
		if !finite(o.Epoch) {
			return Result{}, errors.New("invalid offer clock")
		}
	}
	seen := map[string]bool{}
	complete := true
	for _, w := range writes {
		if w.ID == "" || seen[w.ID] || w.Writer < 0 || w.Writer > 3 {
			return Result{}, errors.New("invalid/duplicate write identity")
		}
		seen[w.ID] = true
		var previous *float64
		for _, at := range []*float64{w.Admitted, w.Submitted, w.Completed, w.Received, w.ACKed} {
			if at == nil {
				complete = false
				continue
			}
			if !finite(*at) || (previous != nil && *at < *previous) {
				return Result{}, errors.New("invalid/reordered write clock")
			}
			previous = at
		}
	}
	for _, r := range reads {
		if r.Worker < 0 || r.Worker > 3 || !finite(r.Epoch) {
			return Result{}, errors.New("invalid read evidence")
		}
	}
	result := Result{Coverage: "PASS"}
	for _, p := range policy {
		row := Window{Bounds: [2]float64{p.lo, p.hi}, Telemetry: "COMPLETE", Coverage: "PASS"}
		if !complete {
			row.Telemetry = "NOT_PROVEN"
		}
		inside := func(t float64) bool { return p.lo <= t-begin && t-begin < p.hi }
		overflow := false
		stage := func(s *Stage, at *float64, n uint64) {
			if at != nil && inside(*at) {
				if ^uint64(0)-s.Bytes < n {
					overflow = true
					return
				}
				s.Count++
				s.Bytes += n
			}
		}
		for _, o := range offers {
			if inside(o.Epoch) {
				row.Offered++
				if o.Skipped {
					row.Skipped++
				}
			}
		}
		for _, w := range writes {
			stage(&row.Admitted, w.Admitted, w.Bytes)
			stage(&row.Submitted, w.Submitted, w.Bytes)
			stage(&row.Completed, w.Completed, w.Bytes)
			stage(&row.ACKed, w.ACKed, w.Bytes)
			if w.Writer < 3 {
				stage(&row.RCACKed, w.ACKed, w.Bytes)
			}
			// Diagnostic latency belongs to the ACK's window, never backcharged.
			if w.ACKed != nil && inside(*w.ACKed) {
				measure := func(dst *Timing, a, b *float64) {
					if a != nil && b != nil {
						dst.Samples++
						dst.MeanSeconds += *b - *a
					}
				}
				measure(&row.SET, w.Admitted, w.ACKed)
				measure(&row.QueueAndPreparation, w.Admitted, w.Submitted)
				measure(&row.NativeWait, w.Submitted, w.Completed)
				measure(&row.ReceiptAndChecks, w.Completed, w.Received)
				measure(&row.Journal, w.Received, w.ACKed)
			}
		}
		if overflow {
			return Result{}, errors.New("byte counter overflow")
		}
		for _, r := range reads {
			if r.Succeeded && inside(r.Epoch) {
				row.Reads[r.Worker]++
			}
		}
		duration := p.hi - p.lo
		for _, timing := range []*Timing{&row.SET, &row.QueueAndPreparation, &row.NativeWait, &row.ReceiptAndChecks, &row.Journal} {
			if !finite(timing.MeanSeconds) {
				return Result{}, errors.New("timing accumulator overflow")
			}
			if timing.Samples > 0 {
				timing.MeanSeconds /= float64(timing.Samples)
			}
		}
		for _, s := range []*Stage{&row.Admitted, &row.Submitted, &row.Completed, &row.ACKed, &row.RCACKed} {
			s.PerSecond = float64(s.Count) / duration
			s.BytesPerSecond = float64(s.Bytes) / duration
		}
		if row.ACKed.Count < 10*p.acks || row.ACKed.Bytes < 10*p.bytes || row.RCACKed.Count < 10*p.rc || row.RCACKed.Bytes < 10*p.rcBytes {
			row.Coverage = "NOT_COVERED"
		}
		for i, n := range row.Reads {
			rate := 1.
			if i == 3 {
				rate = .5
			}
			if float64(n) < math.Ceil(duration*rate) {
				row.Coverage = "NOT_COVERED"
			}
		}
		if row.Coverage != "PASS" {
			result.Coverage = "NOT_COVERED"
		} else if row.Telemetry == "NOT_PROVEN" && result.Coverage == "PASS" {
			result.Coverage = "NOT_PROVEN"
		}
		result.Windows = append(result.Windows, row)
	}
	return result, nil
}
