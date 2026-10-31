//go:build linux

package testrunner

import (
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

type custodyPause struct {
	deadline  time.Time
	resumed   bool
	resumeErr error
}

// Complete task enumeration: mixed, traced, dead or changing sets are UNKNOWN.
func pauseTaskState(pid int) (string, error) {
	return pauseTaskStateAt(fmt.Sprintf("/proc/%d/task", pid))
}

func pauseTaskStateAt(root string) (string, error) {
	before, err := os.ReadDir(root)
	if err != nil || len(before) == 0 || len(before) > 2048 {
		return "", errors.New("task enumeration UNKNOWN")
	}
	stopped := 0
	for _, task := range before {
		f, e := procFieldsAt(root + "/" + task.Name() + "/stat")
		if e != nil {
			return "", e
		}
		switch f[0] {
		case "T":
			stopped++
		case "R", "S", "D", "I":
		default:
			return "", errors.New("task not live/untraced")
		}
	}
	after, err := os.ReadDir(root)
	if err != nil || len(before) != len(after) {
		return "", errors.New("task set changed")
	}
	for n := range before {
		if before[n].Name() != after[n].Name() {
			return "", errors.New("task set changed")
		}
	}
	if stopped == len(before) {
		return "STOPPED", nil
	}
	if stopped == 0 {
		return "LIVE", nil
	}
	return "", errors.New("mixed task stop state")
}

func (p *custodyPause) command(q custodyRequest, i ProcessIdentity, a RunAdmission, authorized bool, fd int) (w ProcessPauseWitness, err error) {
	w.Identity = i
	limit := q.Admission.EffectDeadline
	if !authorized || q.Identity != i || !limit.After(time.Now()) || limit.After(q.PauseDeadline) || q.PauseDeadline.After(a.HardDeadline) {
		return w, errors.New("pause identity/deadline refused")
	}
	poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	if n, e := unix.Poll(poll, 0); e != nil || n != 0 {
		return w, errors.New("pause victim no longer live")
	}
	want := "LIVE"
	switch q.Operation {
	case "pause-stop":
		if !p.deadline.IsZero() || !a.WorkDeadline.After(time.Now()) {
			return w, errors.New("STOP replay/work expired")
		}
		if state, e := pauseTaskState(i.PID); e != nil || state != "LIVE" || !limit.After(time.Now()) || !a.WorkDeadline.After(time.Now()) {
			return w, errors.New("pause baseline not live")
		}
		// Retain the exact resume grant BEFORE the first possible STOP effect.
		p.deadline = q.PauseDeadline
		err = unix.PidfdSendSignal(fd, unix.SIGSTOP, nil, 0)
		want = "STOPPED"
	case "pause-resume", "pause-observe":
		if p.deadline.IsZero() || !q.PauseDeadline.Equal(p.deadline) || p.resumeErr != nil {
			return w, errors.New("missing/failed retained resume grant")
		}
		if q.Operation == "pause-resume" {
			if p.resumed || !limit.After(time.Now()) {
				return w, errors.New("CONT replay")
			}
			p.resumed = true
			defer func() { p.resumeErr = err }()
			err = unix.PidfdSendSignal(fd, unix.SIGCONT, nil, 0)
		} else if !p.resumed {
			return w, errors.New("CONT not proven")
		}
	default:
		return w, errors.New("unknown pause operation")
	}
	if err != nil {
		return w, err
	}
	if pollBound := time.Now().Add(5 * time.Second); pollBound.Before(limit) {
		limit = pollBound
	}
	for time.Now().Before(limit) {
		state, e := pauseTaskState(i.PID)
		if e == nil && state == want {
			w.State, w.ObservedAt = state, time.Now()
			return w, nil
		}
		time.Sleep(5 * time.Millisecond)
	}
	return w, errors.New("pause state witness deadline")
}
