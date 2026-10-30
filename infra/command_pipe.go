package infra

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"runtime"
	"sync"

	"golang.org/x/crypto/ssh"
)

type CommandPipe struct {
	io.Reader
	io.WriteCloser
	CloseSession func() error
}

// ErrCommandNotStarted proves failure before any process/SSH exec dispatch.
// A lost exec reply is not this error, even when OpenCommand returns no pipe.
var ErrCommandNotStarted = errors.New("command NOT_STARTED")

func commandNotStarted(err error) error { return errors.Join(ErrCommandNotStarted, err) }

func (p *CommandPipe) Close() error { return p.CloseSession() }

// OpenCommand owns a separate connection, never a session on Node.client.
// Closing it interrupts blocked channel writes without affecting other jobs.
// ctx bounds creation only; the caller owns the returned pipe's lifetime.
func (n *Node) OpenCommand(ctx context.Context, argv ...string) (*CommandPipe, error) {
	command, err := ShellArgv(argv...)
	if err != nil {
		return nil, commandNotStarted(err)
	}
	if err := ctx.Err(); err != nil {
		return nil, commandNotStarted(err)
	}
	if n.IsNative || n.IsLocal {
		c := exec.Command("bash", "-c", command)
		if n.IsLocal && runtime.GOOS == "windows" {
			c = exec.Command("wsl", "-e", "bash", "-c", command)
		}
		in, writer, err := os.Pipe()
		if err != nil {
			return nil, commandNotStarted(err)
		}
		defer in.Close()
		reader, out, err := os.Pipe()
		if err != nil {
			writer.Close()
			return nil, commandNotStarted(err)
		}
		defer out.Close()
		c.Stdin, c.Stdout = in, out
		var once sync.Once
		closePipe := func() error {
			once.Do(func() { _ = writer.Close(); _ = reader.Close() })
			return nil
		}
		if err := c.Start(); err != nil {
			_ = closePipe()
			return nil, commandNotStarted(err)
		}
		// Our pipe descriptors survive Wait until the decoder consumes EOF.
		go c.Wait()
		p := &CommandPipe{reader, writer, closePipe}
		if err := ctx.Err(); err != nil {
			_ = p.Close()
			return nil, err
		}
		return p, nil
	}
	config, err := n.sshConfig()
	if err != nil {
		return nil, commandNotStarted(err)
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", n.sshAddress())
	if err != nil {
		return nil, commandNotStarted(err)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	type result struct {
		pipe *CommandPipe
		err  error
	}
	done := make(chan result, 1)
	go func() {
		cc, channels, requests, err := ssh.NewClientConn(conn, n.sshAddress(), config)
		if err != nil {
			done <- result{err: commandNotStarted(err)}
			return
		}
		client := ssh.NewClient(cc, channels, requests)
		s, err := client.NewSession()
		if err != nil {
			done <- result{err: commandNotStarted(err)}
			return
		}
		out, err := s.StdoutPipe()
		if err != nil {
			done <- result{err: commandNotStarted(err)}
			return
		}
		in, err := s.StdinPipe()
		if err != nil {
			done <- result{err: commandNotStarted(err)}
			return
		}
		err = s.Start(command)
		if err != nil {
			done <- result{err: err}
			return
		}
		go s.Wait()
		done <- result{pipe: &CommandPipe{out, in, conn.Close}}
	}()
	select {
	case r := <-done:
		if !stop() || ctx.Err() != nil {
			_ = conn.Close()
			return nil, errors.Join(ctx.Err(), r.err)
		}
		if r.err != nil {
			_ = conn.Close()
		}
		return r.pipe, r.err
	case <-ctx.Done():
		_ = conn.Close()
		stop()
		r := <-done // Socket closure settles handshake/channel creation too.
		return nil, errors.Join(ctx.Err(), r.err)
	}
}
