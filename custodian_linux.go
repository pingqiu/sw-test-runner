//go:build linux

package testrunner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// OwnedEntrypoint runs only in a fresh, single-job runner process. It never
// shares the controller's children. The exec gate is a pipe, not a ready file.
func OwnedEntrypoint(args []string) (bool, error) {
	if len(args) == 0 {
		return false, nil
	}
	switch args[0] {
	case "owned-exec":
		var gate [1]byte
		if len(args) < 2 {
			return true, errors.New("missing owned executable")
		}
		if _, err := io.ReadFull(os.NewFile(4, "authorize"), gate[:]); err != nil || gate[0] != 1 {
			return true, errors.New("not authorized")
		}
		return true, syscall.Exec("/proc/self/fd/3", args[1:], os.Environ())
	case "owned-custodian":
		return true, serveCustody(os.Stdin, os.Stdout)
	}
	return false, nil
}

func procFields(pid int) ([]string, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return nil, err
	}
	i := strings.LastIndexByte(string(b), ')')
	if i < 0 {
		return nil, errors.New("invalid proc stat")
	}
	f := strings.Fields(string(b[i+1:]))
	if len(f) < 20 {
		return nil, errors.New("short proc stat")
	}
	return f, nil
}

// A direct child cannot be recycled while it is ours and unreaped. pidfds
// prevent signal-by-PID races; subreaping brings escaped grandchildren here.
func settleChildren(ctx context.Context, leader int, status *unix.WaitStatus, exited *bool) error {
	for {
		for {
			var ws unix.WaitStatus
			pid, err := unix.Wait4(-1, &ws, unix.WNOHANG, nil)
			if err == unix.ECHILD {
				return nil
			}
			if err != nil && err != unix.EINTR {
				return err
			}
			if pid <= 0 {
				break
			}
			if pid == leader {
				*status, *exited = ws, true
			}
		}
		entries, err := os.ReadDir("/proc")
		if err != nil {
			return err
		}
		for _, entry := range entries {
			pid, err := strconv.Atoi(entry.Name())
			if err != nil {
				continue
			}
			f, err := procFields(pid)
			if err != nil || f[1] != strconv.Itoa(os.Getpid()) {
				continue
			}
			fd, err := unix.PidfdOpen(pid, 0)
			if err != nil {
				if err == unix.ESRCH {
					continue
				}
				return err
			}
			err = unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0)
			_ = unix.Close(fd)
			if err != nil && err != unix.ESRCH {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func serveCustody(input io.Reader, output io.Writer) (result error) {
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		return err
	}
	requests := make(chan custodyRequest)
	closed := make(chan struct{})
	defer close(closed)
	go func() {
		defer close(requests)
		d := json.NewDecoder(input)
		for {
			var q custodyRequest
			if d.Decode(&q) != nil {
				return
			}
			select {
			case requests <- q:
			case <-closed:
				return
			}
		}
	}()
	var first custodyRequest
	select {
	case q, ok := <-requests:
		if !ok {
			return io.EOF
		}
		first = q
	case <-time.After(10 * time.Second):
		return errors.New("no enrollment")
	}
	a, s := first.Admission, first.Spec
	if first.Operation != "enroll" || !a.WorkDeadline.After(time.Now()) || a.CleanupReserve <= 0 || a.CleanupReserve > time.Minute || a.WorkDeadline.Add(a.CleanupReserve).After(a.HardDeadline) {
		return errors.New("invalid custody envelope")
	}
	file, err := os.Open(s.Executable)
	if err != nil {
		return err
	}
	defer file.Close()
	gate, authorize, err := os.Pipe()
	if err != nil {
		return err
	}
	defer gate.Close()
	defer authorize.Close()
	out, err := os.OpenFile(s.StdoutPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer out.Close()
	errout, err := os.OpenFile(s.StderrPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer errout.Close()
	self, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(self, append([]string{"owned-exec", s.Executable}, s.Argv...)...)
	cmd.ExtraFiles, cmd.Stdout, cmd.Stderr = []*os.File{file, gate}, out, errout
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	pid := cmd.Process.Pid
	var status unix.WaitStatus
	exited, settled := false, false
	defer func() {
		if !settled {
			ctx, cancel := context.WithDeadline(context.Background(), a.HardDeadline)
			defer cancel()
			result = errors.Join(result, settleChildren(ctx, pid, &status, &exited))
		}
		_ = cmd.Process.Release()
	}()
	pidfd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return err
	}
	defer unix.Close(pidfd)
	f, err := procFields(pid)
	if err != nil {
		return err
	}
	start, err := strconv.ParseUint(f[19], 10, 64)
	if err != nil {
		return err
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return err
	}
	identity := ProcessIdentity{Node: s.Node, RunID: a.RunID, BootID: strings.TrimSpace(string(boot)), PID: pid, PGID: pid, StartTime: start, ExecutableSHA256: s.SHA256}
	enc := json.NewEncoder(output)
	if err := enc.Encode(custodyReply{Sequence: first.Sequence, Identity: identity}); err != nil {
		return err
	}
	timer := time.NewTimer(time.Until(a.HardDeadline))
	defer timer.Stop()
	authorized := false
	for {
		select {
		case <-timer.C:
			return errors.New("hard deadline")
		case q, ok := <-requests:
			if !ok {
				return errors.New("controller disconnected")
			}
			r := custodyReply{Sequence: q.Sequence}
			var callErr error
			switch q.Operation {
			case "authorize":
				if authorized || q.Identity != identity || !a.WorkDeadline.After(time.Now()) {
					callErr = errors.New("authorization refused")
				} else {
					_, callErr = authorize.Write([]byte{1})
					authorized = callErr == nil
				}
			case "kill":
				var info unix.Siginfo
				callErr = unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOHANG|unix.WNOWAIT, nil)
				if callErr == nil && info.Signo != 0 {
					callErr = errors.New("victim not alive")
				}
				if callErr == nil {
					callErr = unix.PidfdSendSignal(pidfd, unix.SIGKILL, nil, 0)
				}
				if callErr == nil {
					r.Kill = KillWitness{Identity: identity, AliveBefore: true, Signal: "KILL", AppliedAt: time.Now()}
				}
			case "status", "settle":
				if !exited {
					var ws unix.WaitStatus
					got, e := unix.Wait4(pid, &ws, unix.WNOHANG, nil)
					callErr = e
					if got == pid {
						status, exited = ws, true
					}
				}
				if callErr == nil && (exited || q.Operation == "settle") {
					ctx, cancel := context.WithDeadline(context.Background(), a.HardDeadline)
					callErr = settleChildren(ctx, pid, &status, &exited)
					cancel()
					settled = callErr == nil
					if settled {
						r.Exit = ExitInfo{Identity: identity, Code: status.ExitStatus(), StdoutPath: s.StdoutPath, StderrPath: s.StderrPath, Settled: true}
						if status.Signaled() {
							r.Exit.Signal = int(status.Signal())
						}
					}
				}
			default:
				callErr = errors.New("unknown custody command")
			}
			if callErr != nil {
				r.Error = callErr.Error()
			}
			if err := enc.Encode(r); err != nil {
				return err
			}
			if settled {
				return callErr
			}
		}
	}
}
