package testrunner

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// A filesystem read can outlive cancellation (for example, an unavailable
// shared mount). Caller refusal is bounded; the read remains accounted until
// it terminates. It can only capture data, never dispatch a job after timeout.
func (a *ApprovedRun) readBounded(parent context.Context, limit time.Duration, read func() error) error {
	ctx, cancel := context.WithTimeout(parent, limit)
	defer cancel()
	a.mu.Lock()
	if a.closing {
		a.mu.Unlock()
		return errors.New("authority admission closed")
	}
	a.readers++
	a.mu.Unlock()
	done := make(chan error, 1)
	go func() {
		var err error
		defer func() {
			if p := recover(); p != nil {
				err = fmt.Errorf("authority capture panic: %v", p)
			}
			a.mu.Lock()
			a.readers--
			close(a.readerChanged)
			a.readerChanged = make(chan struct{})
			a.mu.Unlock()
			done <- err
		}()
		err = read()
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return fmt.Errorf("authority capture not proven within bound: %w", ctx.Err())
	}
}

func (a *ApprovedRun) authority(ctx context.Context, limit time.Duration) error {
	return a.readBounded(ctx, limit, a.checkWork)
}

func (a *ApprovedRun) startWatch() {
	ctx, stop := context.WithCancel(a.scope.ctx)
	a.watchStop = stop
	a.watchDone = make(chan struct{})
	go func() {
		defer close(a.watchDone)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := a.authority(ctx, 2*time.Second); err != nil {
					if ctx.Err() == nil {
						a.revoke(err)
					}
					return
				}
			}
		}
	}()
}
