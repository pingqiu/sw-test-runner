package infra

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"runtime"
)

type CommandPipe struct {
	io.Reader
	io.WriteCloser
	CloseSession func() error
}

func (p *CommandPipe) Close() error { return p.CloseSession() }

// OpenCommand is an unprivileged, one-shot stdin/stdout session, not a daemon.
// A late session receives no input and is closed; it cannot authorize work.
func (n *Node) OpenCommand(ctx context.Context, argv ...string) (*CommandPipe, error) {
	command, err := ShellArgv(argv...)
	if err != nil {
		return nil, err
	}
	type result struct {
		pipe *CommandPipe
		err  error
	}
	done := make(chan result)
	go func() {
		var p *CommandPipe
		var err error
		if n.IsNative || n.IsLocal {
			c := exec.Command("bash", "-c", command)
			if n.IsLocal && runtime.GOOS == "windows" {
				c = exec.Command("wsl", "-e", "bash", "-c", command)
			}
			out, e := c.StdoutPipe()
			if e != nil {
				err = e
			} else {
				in, e := c.StdinPipe()
				if e != nil {
					err = e
				} else if err = c.Start(); err == nil {
					p = &CommandPipe{out, in, in.Close}
					go c.Wait()
				}
			}
		} else {
			n.mu.Lock()
			client := n.client
			n.mu.Unlock()
			if client == nil {
				err = fmt.Errorf("SSH not connected")
			} else {
				s, e := client.NewSession()
				if e != nil {
					err = e
				} else {
					out, e := s.StdoutPipe()
					if e != nil {
						err = e
					} else {
						in, e := s.StdinPipe()
						if e != nil {
							err = e
						} else if err = s.Start(command); err == nil {
							p = &CommandPipe{out, in, s.Close}
						}
					}
					if err != nil {
						_ = s.Close()
					} else {
						go s.Wait()
					}
				}
			}
		}
		select {
		case done <- result{p, err}:
		case <-ctx.Done():
			if p != nil {
				_ = p.Close()
			}
		}
	}()
	select {
	case r := <-done:
		return r.pipe, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
