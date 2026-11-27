package declared

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"sync"
	"time"
)

// SimHost stands in for a host's shell in tests of detached commands: it
// answers the runner-io protocol of RunDetached by running each launched
// command on Inner (Run or RunRoot, as it was launched), and passes every
// other command to Inner as it is. Inner thus sees exactly the commands a
// direct run would issue.
//
// A launched command whose Inner call returns an error within LaunchWait
// fails the launch with that error (the command could not be run). One that
// has not answered by the kill is killed, unless Unkillable, when it is
// reported as surviving the kill.
type SimHost struct {
	Inner      Runner
	Unkillable bool
	LaunchWait time.Duration

	mu   sync.Mutex
	jobs map[string]*simJob
	pid  int
}

type simJob struct {
	done        chan struct{}
	out, errOut string
	code        int
	err         error
	pid         int
	killed      bool
}

func (h *SimHost) Run(ctx context.Context, cmd string) (string, string, int, error) {
	return h.do(ctx, cmd, false)
}

func (h *SimHost) RunRoot(ctx context.Context, cmd string) (string, string, int, error) {
	return h.do(ctx, cmd, true)
}

func (h *SimHost) do(ctx context.Context, cmd string, root bool) (string, string, int, error) {
	if !strings.HasPrefix(cmd, ": runner-io ") {
		if root {
			return h.Inner.RunRoot(ctx, cmd)
		}
		return h.Inner.Run(ctx, cmd)
	}
	f := strings.Fields(strings.SplitN(cmd, ";", 2)[0])
	if len(f) >= 3 && f[2] == "watch" {
		return h.watch(f[3:]), "", 0, nil
	}
	if len(f) < 4 {
		return "", "bad protocol command", 2, nil
	}
	verb, dir := f[2], f[3]
	h.mu.Lock()
	if h.jobs == nil {
		h.jobs = map[string]*simJob{}
	}
	j := h.jobs[dir]
	h.mu.Unlock()
	switch verb {
	case "launch":
		b, err := base64.StdEncoding.DecodeString(f[4])
		if err != nil {
			return "", "bad launch", 2, nil
		}
		h.mu.Lock()
		h.pid++
		j = &simJob{done: make(chan struct{}), pid: 40000 + h.pid}
		h.jobs[dir] = j
		h.mu.Unlock()
		inner := string(b)
		go func() {
			run := h.Inner.Run
			if root {
				run = h.Inner.RunRoot
			}
			j.out, j.errOut, j.code, j.err = run(context.WithoutCancel(ctx), inner)
			close(j.done)
		}()
		wait := h.LaunchWait
		if wait == 0 {
			wait = 50 * time.Millisecond
		}
		select {
		case <-j.done:
			if j.err != nil {
				return j.out, j.errOut, j.code, j.err
			}
		case <-time.After(wait):
		}
		return fmt.Sprintf("launched %d 777\n", j.pid), "", 0, nil
	case "poll":
		if j == nil {
			return "running\n", "", 0, nil
		}
		select {
		case <-j.done:
			if j.killed {
				return "running\n", "", 0, nil
			}
			return fmt.Sprintf("done\n%d\n%s\n%s\n", j.code, base64.StdEncoding.EncodeToString([]byte(j.out)), base64.StdEncoding.EncodeToString([]byte(j.errOut))), "", 0, nil
		default:
			return "running\n", "", 0, nil
		}
	case "kill":
		if j == nil {
			return "killed\n", "", 0, nil
		}
		select {
		case <-j.done:
			return "done\n", "", 0, nil
		default:
		}
		if h.Unkillable {
			return fmt.Sprintf("left %d 777\nkilled\n", j.pid+1), "", 0, nil
		}
		j.killed = true
		return "killed\n", "", 0, nil
	}
	return "", "bad protocol verb", 2, nil
}

// watch answers the abandoned-child watcher: a survivor (the job's command
// process, pid+1) is alive until its job finishes, then gone with the
// job's exit code as the waiter would have written it.
func (h *SimHost) watch(pids []string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var b strings.Builder
	for _, p := range pids {
		var job *simJob
		for _, j := range h.jobs {
			if fmt.Sprint(j.pid+1) == p {
				job = j
			}
		}
		if job == nil {
			fmt.Fprintf(&b, "gone %s - -\n", p)
			continue
		}
		select {
		case <-job.done:
			fmt.Fprintf(&b, "gone %s %d %s\n", p, job.code, p)
		default:
			fmt.Fprintf(&b, "alive %s\n", p)
		}
	}
	return b.String()
}
