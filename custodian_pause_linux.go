//go:build linux

package testrunner

import (
	"context"
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"os"
	"strings"
	"time"
)

type custodyPause struct {
	deadline         time.Time
	stopped, resumed bool
	resumeErr        error
}

// A stable task enumeration is required; mixed/unknown states earn no witness.
func pauseTaskState(pid int) (string, error) {
	root := fmt.Sprintf("/proc/%d/task", pid)
	before, err := os.ReadDir(root)
	if err != nil {
		return "", err
	}
	if len(before) == 0 || len(before) > 2048 {
		return "", errors.New("task enumeration UNKNOWN")
	}
	stopped := 0
	for _, task := range before {
		raw, err := os.ReadFile(root + "/" + task.Name() + "/stat")
		if err != nil {
			return "", err
		}
		end := strings.LastIndexByte(string(raw), ')')
		if end < 0 {
			return "", errors.New("task stat malformed")
		}
		fields := strings.Fields(string(raw[end+1:]))
		if len(fields) < 20 {
			return "", errors.New("task stat short")
		}
		switch fields[0] {
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
	if !authorized || q.Identity != i || !q.PauseDeadline.After(time.Now()) || q.PauseDeadline.After(a.HardDeadline) {
		return w, errors.New("pause identity/deadline refused")
	}
	if q.Operation == "pause-bind" {
		if !p.deadline.IsZero() || !a.WorkDeadline.After(time.Now()) {
			return w, errors.New("pause already bound/work expired")
		}
	} else if p.deadline.IsZero() || !q.PauseDeadline.Equal(p.deadline) {
		return w, errors.New("pause grant not retained")
	}
	limit := q.PauseDeadline
	if until := time.Now().Add(5 * time.Second); limit.After(until) {
		limit = until
	}
	ctx, cancel := context.WithDeadline(context.Background(), limit)
	defer cancel()
	poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	if n, e := unix.Poll(poll, 0); e != nil || n != 0 {
		return w, errors.New("pause victim no longer live")
	}
	want := ""
	switch q.Operation {
	case "pause-bind":
		state, e := pauseTaskState(i.PID)
		if e != nil || state != "LIVE" {
			return w, errors.New("pause baseline not live")
		}
		p.deadline = q.PauseDeadline
		want = "LIVE"
	case "pause-stop":
		if p.stopped || p.resumed || !a.WorkDeadline.After(time.Now()) {
			return w, errors.New("STOP replay/work expired")
		}
		p.stopped = true
		err = unix.PidfdSendSignal(fd, unix.SIGSTOP, nil, 0)
		want = "STOPPED"
	case "pause-resume":
		if p.resumed {
			return w, errors.New("CONT replay")
		}
		p.resumed = true
		defer func() {
			if err != nil {
				p.resumeErr = err
			}
		}()
		err = unix.PidfdSendSignal(fd, unix.SIGCONT, nil, 0)
		want = "LIVE"
	case "pause-observe":
		if p.resumeErr != nil {
			return w, p.resumeErr
		}
	default:
		return w, errors.New("unknown pause operation")
	}
	if err != nil {
		return w, err
	}
	for {
		state, e := pauseTaskState(i.PID)
		if e == nil && (want == "" || state == want) {
			w.State = state
			w.ObservedAt = time.Now()
			return w, nil
		}
		select {
		case <-ctx.Done():
			return w, errors.Join(e, ctx.Err())
		case <-time.After(5 * time.Millisecond):
		}
	}
}
