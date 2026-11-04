//go:build linux

package testrunner

import (
	"errors"
	"testing"

	"golang.org/x/sys/unix"
)

// EINTR from the zero-timeout liveness poll is retried, never reported as a
// liveness answer; a real error and a ready pidfd are still returned as is.
func TestPidfdReadyRetriesEINTR(t *testing.T) {
	defer func(f func([]unix.PollFd, int) (int, error)) { pollFds = f }(pollFds)
	for name, tc := range map[string]struct {
		answers []error
		ready   int
		wantN   int
		wantErr error
		calls   int
	}{
		"eintr twice then none ready": {answers: []error{unix.EINTR, unix.EINTR, nil}, calls: 3},
		"eintr then ready":            {answers: []error{unix.EINTR, nil}, ready: 1, wantN: 1, calls: 2},
		"real error":                  {answers: []error{unix.EBADF}, wantN: -1, wantErr: unix.EBADF, calls: 1},
	} {
		calls := 0
		pollFds = func(fds []unix.PollFd, timeout int) (int, error) {
			if timeout != 0 || len(fds) != 1 || fds[0].Events != unix.POLLIN {
				t.Fatalf("%s: poll(%v, %d)", name, fds, timeout)
			}
			e := tc.answers[calls]
			calls++
			if e != nil {
				return -1, e
			}
			return tc.ready, nil
		}
		n, err := pidfdReady(7)
		if n != tc.wantN || !errors.Is(err, tc.wantErr) || err == nil && tc.wantErr != nil || calls != tc.calls {
			t.Fatalf("%s: n=%d err=%v calls=%d", name, n, err, calls)
		}
	}
}
