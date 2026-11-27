package v3block

// The block product's declared actions (product/actions.json), run by the
// generic executor (package declared). This file supplies only what the
// executor asks a product for: the node, the subject (the runner's record
// of one volume, opVolume) and the outcome table. The actions that stay Go
// handlers are listed in actions.json under "exceptions", each with why.

import (
	"context"
	_ "embed"
	"fmt"
	"strconv"

	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/actions"
	"github.com/pingqiu/sw-test-runner/declared"
)

//go:embed product/actions.json
var actionsJSON []byte

// opNode finds the node a step names; a test replaces it with a fake.
var opNode = func(actx *tr.ActionContext, name string) (runner, error) {
	n, err := actions.GetNode(actx, name)
	if err != nil {
		return nil, err
	}
	return n, nil
}

// volumeSubject is one volume's record as a declared action's subject.
type volumeSubject struct {
	actx  *tr.ActionContext
	v     *opVolume
	alive *bool // the holder liveness check, read once
}

func (s *volumeSubject) Field(ctx context.Context, node declared.Runner, f string) (string, error) {
	v := s.v
	switch f {
	case "name":
		return v.Name, nil
	case "nqn":
		return v.NQN, nil
	case "layout_id":
		return v.LayoutID, nil
	case "status_addr":
		if v.Cur < 0 || v.Cur >= len(v.Hosts) {
			return "", fmt.Errorf("volume %q: no current host %d", v.Name, v.Cur)
		}
		return v.Hosts[v.Cur].Status, nil
	case "holder_alive":
		if s.alive == nil {
			a := alive(ctx, node, v.PID, v.PIDStart)
			s.alive = &a
		}
		return strconv.FormatBool(*s.alive), nil
	}
	return "", fmt.Errorf("volume field %q", f)
}

func (s *volumeSubject) Set(f, val string) error {
	if f != "layout_id" {
		return fmt.Errorf("volume field %q is not settable", f)
	}
	s.v.LayoutID = val
	return nil
}

func (s *volumeSubject) Save() { saveVolume(s.actx, s.v) }

// blockActions is the executor of product/actions.json. A description that
// does not load stops the pack, as a bad outcome table does.
var blockActions = func() *declared.Executor {
	f, err := declared.Parse(actionsJSON)
	if err != nil {
		panic(err)
	}
	return &declared.Executor{
		File: f,
		Node: func(actx *tr.ActionContext, name string) (declared.Runner, error) { return opNode(actx, name) },
		Subject: func(actx *tr.ActionContext, name string) (declared.Subject, error) {
			v, err := loadVolume(actx, name)
			if err != nil {
				return nil, err
			}
			return &volumeSubject{actx: actx, v: v}, nil
		},
		Lookup: func(action, path, code, message string) string {
			return outcomes.lookup(opPath{action, path}, code, message)
		},
	}
}()
