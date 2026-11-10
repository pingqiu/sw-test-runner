package seaweed

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"
)

var maintenanceAdmissionMaximum = 30 * time.Second
var errMaintenanceNoAdmission = errors.New("maintenance has no eligible in-flight copy")

// Original C1 maxima: mark10 + kill60 + down60 + mark10 + start60 +
// identity30 + ready45 + final stamp10. Admission never starts a new phase.
const maintenanceC1Tail = 285 * time.Second

func (m *maintenanceRuntime) admitCalibrationBudget(ctx context.Context, gate time.Duration) error {
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) < gate+maintenanceC1Tail {
		return errors.New("maintenance admission cannot fit the original C1 tail")
	}
	// Leave the independent final readback's entire maximum before work ends;
	// maintenanceBudget also keeps the separate 300s cleanup reserve intact.
	return m.budget.admit(time.Now(), time.Until(deadline)+240*time.Second)
}

func (m *maintenanceRuntime) eligibleAdmission(s maintenanceSample) bool {
	i, p := s.Identity, s.Progress
	if i == nil || !s.Alive || s.Result != nil || i.PID <= 0 || i.Start == "" || i.Boot == "" || i.Boot != s.Boot || i.Task != m.c.MaintenanceTask || i.RunID != m.d.RunID || i.ConfigSHA != m.base["config_sha256"] || !validMaintenanceBound(maintenanceBound{s.Before, s.After}) || p == nil {
		return false
	}
	victim := endpoint(m.hosts["vol1"], "volume_http")
	return p.State == "in_flight" && p.VID == m.subject.VID && p.Operation != "" && p.Bytes > 0 && p.Begin > 0 && p.Begin <= s.Before && p.Source != "" && p.Target != "" && (victim == p.Source || victim == p.Target)
}

func (m *maintenanceRuntime) awaitCopyAdmission(ctx context.Context, changed <-chan struct{}) error {
	gate, cancel := context.WithTimeout(ctx, maintenanceAdmissionMaximum)
	defer cancel()
	var previous maintenanceSample
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-gate.Done():
			return fmt.Errorf("%w: bounded signal wait expired", errMaintenanceNoAdmission)
		case <-m.done:
			_, err := m.taskSnapshot()
			if err != nil {
				return err
			}
			return fmt.Errorf("%w: task completed before admission", errMaintenanceNoAdmission)
		case <-changed:
			m.mu.Lock()
			s, received, err := m.latest, m.latestReceived, m.latestError
			m.mu.Unlock()
			if err != nil {
				return fmt.Errorf("maintenance admission observation unreadable: %w", err)
			}
			if time.Since(received) > 2*time.Second || !m.eligibleAdmission(s) {
				previous = maintenanceSample{}
				continue
			}
			p, q := previous.Progress, s.Progress
			if p != nil && previous.Identity != nil && *previous.Identity == *s.Identity && previous.After <= s.Before && previous.Before < s.Before && p.Operation == q.Operation && p.Begin == q.Begin && p.Source == q.Source && p.Target == q.Target && q.Bytes > p.Bytes {
				select {
				case <-m.done:
					return fmt.Errorf("%w: task completed at admission", errMaintenanceNoAdmission)
				default:
				}
				if err := m.admitCalibrationBudget(ctx, 0); err != nil {
					return err
				}
				return m.r.save("maintenance-admission.json", map[string]any{"before": previous, "admitted": s, "phase_deadline": deadlineString(ctx), "coverage": "NOT_GRANTED"})
			}
			previous = s
		}
	}
}

func deadlineString(ctx context.Context) string {
	d, _ := ctx.Deadline()
	return d.UTC().Format(time.RFC3339Nano)
}

// No fault means NO error excusal or synthetic fault markers. This validates
// the load only; it cannot qualify fault coverage, which remains zero.
func checkMaintenanceUnfaultedOps(ops []chaosOp, _ chaosWindows) error {
	if len(ops) == 0 {
		return errors.New("unfaulted maintenance load is empty")
	}
	acked := map[string]bool{}
	for i, op := range ops {
		if op.Op != i || !op.Exact || op.Started <= 0 || math.IsNaN(op.Started) || math.IsNaN(op.Ended) || math.IsInf(op.Started, 0) || math.IsInf(op.Ended, 0) || op.Ended < op.Started || i > 0 && op.Started < ops[i-1].Ended {
			return fmt.Errorf("unfaulted maintenance load op %d is not exact/ordered", i)
		}
		acked[op.Kind] = true
	}
	if !acked["kv_set"] || !acked["s3_put"] {
		return errors.New("unfaulted maintenance load lacks KV/S3 ACKs")
	}
	return nil
}
