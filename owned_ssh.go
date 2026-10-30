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
	EffectDeadline time.Time
	Sequence       uint64
	Error          string
	Identity       ProcessIdentity
	Exit           ExitInfo
	Kill           KillWitness
}

type custodyChannel struct {
	gate     sync.Mutex
	pipe     io.ReadWriteCloser
	sequence uint64
	replies  chan custodyReply
	poisoned bool
	release  func()
}

// SSHJobs uses an already-connected Node and an installed runner executable.
// The executable/SSH trust binding is an explicit deployment precondition.
type SSHJobs struct {
	mu     sync.Mutex
	Nodes  map[string]*infra.Node
	Runner string
	jobs   map[string]*custodyChannel
	exits  map[string]ExitInfo
	slots  chan struct{}
}

func (b *SSHJobs) Enroll(ctx context.Context, id string, a RunAdmission, s JobSpec) (_ ProcessIdentity, err error) {
	c := &custodyChannel{replies: make(chan custodyReply, 1)}
	b.mu.Lock()
	if b.jobs == nil {
		b.jobs = make(map[string]*custodyChannel)
		b.slots = make(chan struct{}, 8)
	}
	if b.jobs[id] != nil {
		b.mu.Unlock()
		return ProcessIdentity{}, errors.New("duplicate custody id")
	}
	b.jobs[id] = c
	b.mu.Unlock()
	defer func() {
		if errors.Is(err, ErrJobNotStarted) {
			b.mu.Lock()
			delete(b.jobs, id)
			b.mu.Unlock()
		}
	}()
	n := b.Nodes[s.Node]
	if n == nil {
		return ProcessIdentity{}, fmt.Errorf("%w: unknown custody node", ErrJobNotStarted)
	}
	select {
	case b.slots <- struct{}{}:
	case <-ctx.Done():
		return ProcessIdentity{}, errors.Join(ErrJobNotStarted, ctx.Err())
	}
	release := sync.OnceFunc(func() { <-b.slots })
	c.release = release
	p, err := n.OpenCommand(ctx, b.Runner, "owned-custodian")
	if err != nil {
		release()
		if errors.Is(err, infra.ErrCommandNotStarted) {
			err = errors.Join(ErrJobNotStarted, err)
		}
		return ProcessIdentity{}, err
	}
	c.pipe = p
	lifetime, cancel := context.WithDeadline(context.Background(), a.HardDeadline)
	context.AfterFunc(lifetime, func() { _ = p.Close() })
	go func() {
		defer release()
		defer cancel()
		defer close(c.replies)
		d := json.NewDecoder(p)
		for {
			var r custodyReply
			if d.Decode(&r) != nil {
				return
			}
			select {
			case c.replies <- r:
			case <-lifetime.Done():
				return
			}
		}
	}()
	r, err := c.call(ctx, custodyRequest{Operation: "enroll", Admission: a, Spec: s})
	if err == nil && !a.EffectDeadline.IsZero() && !r.EffectDeadline.Equal(a.EffectDeadline) {
		err = errors.New("custodian did not bind effect deadline; authorization refused")
	}
	return r.Identity, err
}

func (c *custodyChannel) call(ctx context.Context, q custodyRequest) (custodyReply, error) {
	c.gate.Lock()
	defer c.gate.Unlock()
	if err := ctx.Err(); err != nil {
		return custodyReply{}, err
	}
	if c.pipe == nil || c.poisoned {
		return custodyReply{}, errors.New("custody transport UNKNOWN")
	}
	c.sequence++
	q.Sequence = c.sequence
	written := make(chan error, 1)
	go func() { written <- json.NewEncoder(c.pipe).Encode(q) }()
	select {
	case err := <-written:
		if err != nil {
			c.poisoned = true
			_ = c.pipe.Close()
			return custodyReply{}, err
		}
	case <-ctx.Done():
		c.poisoned = true
		_ = c.pipe.Close()
		<-written
		return custodyReply{}, ctx.Err()
	}
	for {
		select {
		case r, ok := <-c.replies:
			if !ok {
				return custodyReply{}, errors.New("custody reply UNKNOWN")
			}
			if r.Sequence != q.Sequence {
				continue
			}
			if r.Error != "" {
				return r, errors.New(r.Error)
			}
			return r, nil
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
	if err == nil && op == "settle" && !r.Exit.Settled {
		err = errors.New("custody settlement UNKNOWN")
	}
	if err == nil && (op == "settle" || r.Exit.Settled) {
		_ = c.pipe.Close()
		c.release()
		b.mu.Lock()
		if b.exits == nil {
			b.exits = map[string]ExitInfo{}
		}
		b.exits[id] = r.Exit
		delete(b.jobs, id)
		b.mu.Unlock()
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
		b.mu.Lock()
		exit, terminal := b.exits[id]
		b.mu.Unlock()
		if terminal {
			return exit, nil
		}
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
