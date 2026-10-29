package testrunner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/pingqiu/sw-test-runner/infra"
)

type custodyRequest struct {
	Sequence  uint64
	Operation string
	Admission RunAdmission
	Spec      JobSpec
	Identity  ProcessIdentity
}

type custodyReply struct {
	Sequence uint64
	Error    string
	Identity ProcessIdentity
	Exit     ExitInfo
	Kill     KillWitness
}

type custodyChannel struct {
	gate     sync.Mutex
	pipe     io.ReadWriteCloser
	sequence uint64
	replies  chan custodyReply
	failed   chan struct{}
}

// SSHJobs uses an already-connected Node and an installed runner executable.
// The executable/SSH trust binding is an explicit deployment precondition.
type SSHJobs struct {
	mu     sync.Mutex
	Nodes  map[string]*infra.Node
	Runner string
	jobs   map[string]*custodyChannel
}

func (b *SSHJobs) Enroll(ctx context.Context, id string, a RunAdmission, s JobSpec) (ProcessIdentity, error) {
	c := &custodyChannel{replies: make(chan custodyReply, 1), failed: make(chan struct{})}
	b.mu.Lock()
	if b.jobs == nil {
		b.jobs = make(map[string]*custodyChannel)
	}
	if b.jobs[id] != nil {
		b.mu.Unlock()
		return ProcessIdentity{}, errors.New("duplicate custody id")
	}
	b.jobs[id] = c
	b.mu.Unlock()
	n := b.Nodes[s.Node]
	if n == nil {
		return ProcessIdentity{}, errors.New("unknown custody node")
	}
	p, err := n.OpenCommand(ctx, b.Runner, "owned-custodian")
	if err != nil {
		return ProcessIdentity{}, err
	}
	c.pipe = p
	go func() {
		defer close(c.failed)
		d := json.NewDecoder(p)
		for {
			var r custodyReply
			if d.Decode(&r) != nil {
				return
			}
			select {
			case c.replies <- r:
			case <-ctx.Done():
				return
			}
		}
	}()
	r, err := c.call(ctx, custodyRequest{Operation: "enroll", Admission: a, Spec: s})
	return r.Identity, err
}

func (c *custodyChannel) call(ctx context.Context, q custodyRequest) (custodyReply, error) {
	c.gate.Lock()
	defer c.gate.Unlock()
	if c.pipe == nil {
		return custodyReply{}, errors.New("custody transport UNKNOWN")
	}
	c.sequence++
	q.Sequence = c.sequence
	written := make(chan error, 1)
	go func() { written <- json.NewEncoder(c.pipe).Encode(q) }()
	select {
	case err := <-written:
		if err != nil {
			return custodyReply{}, err
		}
	case <-ctx.Done():
		return custodyReply{}, ctx.Err()
	}
	for {
		select {
		case r := <-c.replies:
			if r.Sequence != q.Sequence {
				continue
			}
			if r.Error != "" {
				return r, errors.New(r.Error)
			}
			return r, nil
		case <-c.failed:
			return custodyReply{}, errors.New("custody reply UNKNOWN")
		case <-ctx.Done():
			return custodyReply{}, ctx.Err()
		}
	}
}

func (b *SSHJobs) command(ctx context.Context, id, op string, identity ProcessIdentity) (custodyReply, error) {
	b.mu.Lock()
	c := b.jobs[id]
	b.mu.Unlock()
	if c == nil {
		return custodyReply{}, fmt.Errorf("unknown custody id %s", id)
	}
	r, err := c.call(ctx, custodyRequest{Operation: op, Identity: identity})
	if err == nil && (op == "settle" || r.Exit.Settled) {
		_ = c.pipe.Close()
	}
	return r, err
}

func (b *SSHJobs) Authorize(ctx context.Context, id string, i ProcessIdentity) error {
	_, err := b.command(ctx, id, "authorize", i)
	return err
}
func (b *SSHJobs) Settle(ctx context.Context, id string) error {
	_, err := b.command(ctx, id, "settle", ProcessIdentity{})
	return err
}
func (b *SSHJobs) Kill(ctx context.Context, id string) (KillWitness, error) {
	r, err := b.command(ctx, id, "kill", ProcessIdentity{})
	return r.Kill, err
}
func (b *SSHJobs) Wait(ctx context.Context, id string) (ExitInfo, error) {
	for {
		r, err := b.command(ctx, id, "status", ProcessIdentity{})
		if err != nil || r.Exit.Settled {
			return r.Exit, err
		}
		t := time.NewTimer(20 * time.Millisecond)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return ExitInfo{}, ctx.Err()
		}
	}
}
