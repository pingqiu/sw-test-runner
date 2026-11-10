package seaweed

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"
)

var maintenanceObservationPeriod = time.Second

// The controller orders RPCs; their numeric clocks are never compared with a
// victim's clock. All eligibility bounds come from the task host and boot.
func (m *maintenanceRuntime) startObserver(ctx context.Context) (first, changed, done <-chan struct{}, stop context.CancelFunc, err error) {
	n := m.c.node(m.hosts["meta"])
	if err = n.ConnectContext(ctx); err != nil {
		return nil, nil, nil, nil, err
	}
	m.observerNode = n
	observe, cancel := context.WithCancel(ctx)
	firstCh, changedCh, doneCh := make(chan struct{}), make(chan struct{}, 1), make(chan struct{})
	go func() {
		defer close(doneCh)
		defer n.Close()
		initial := true
		for {
			started := time.Now()
			sample, e := m.sample(observe)
			m.mu.Lock()
			m.latestError = e
			m.latestReceived = time.Now()
			if e == nil {
				m.latest = sample
				for _, f := range m.faults {
					if !f.AfterFollowsGone && started.After(f.goneAt) {
						f.After = sample
						f.AfterFollowsGone = true
					}
				}
			} else {
				m.latest = maintenanceSample{}
			}
			raw := processJSON(m.faults)
			m.mu.Unlock()
			if e == nil {
				e = os.WriteFile(filepath.Join(m.c.Output, "maintenance-faults.json"), raw, 0600)
			}
			if e != nil && observe.Err() == nil {
				m.r.save("maintenance-observer-last-error.json", map[string]any{"error": e.Error(), "coverage": "NOT_COVERED"})
			}
			if initial {
				close(firstCh)
				initial = false
			}
			select {
			case changedCh <- struct{}{}:
			default:
			}
			timer := time.NewTimer(maintenanceObservationPeriod)
			select {
			case <-observe.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
	return firstCh, changedCh, doneCh, cancel, nil
}

func (m *maintenanceRuntime) observationsComplete() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, f := range m.faults {
		if !f.AfterFollowsGone {
			return false
		}
	}
	return true
}

func (m *maintenanceRuntime) finishObserver(ctx context.Context, changed, done <-chan struct{}, stop context.CancelFunc) error {
	for !m.observationsComplete() {
		select {
		case <-changed:
		case <-ctx.Done():
			stop()
			<-done
			return ctx.Err()
		case <-done:
			return errors.New("maintenance observer stopped before fault readback")
		}
	}
	stop()
	<-done
	return nil
}
