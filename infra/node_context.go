package infra

import (
	"context"
	"fmt"
	"net"
	"time"

	"golang.org/x/crypto/ssh"
)

// ConnectContext is the bounded cloud connection. Closing its underlying socket
// cancels handshake, channel-open and upload I/O without taking Node.mu.
// Existing Connect and the on-prem strict runtime remain unchanged.
func (n *Node) ConnectContext(ctx context.Context) error {
	deadline, bounded := ctx.Deadline()
	if !bounded {
		return fmt.Errorf("SSH connection requires a deadline")
	}
	config, err := n.sshConfig()
	if err != nil {
		return err
	}
	socket, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", n.sshAddress())
	if err != nil {
		return err
	}
	if err = socket.SetDeadline(deadline); err != nil {
		socket.Close()
		return err
	}
	stop := context.AfterFunc(ctx, func() { socket.Close() })
	conn, channels, requests, err := ssh.NewClientConn(socket, n.sshAddress(), config)
	if err != nil {
		stop()
		socket.Close()
		return fmt.Errorf("SSH handshake: %w", err)
	}
	client := ssh.NewClient(conn, channels, requests)
	n.mu.Lock()
	n.client = client
	n.mu.Unlock()
	go func() { client.Wait(); stop() }()
	return nil
}
