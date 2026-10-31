package testrunner

import (
	"context"
	"errors"
	"maps"
)

// PackInputs is an immutable COPY of independently admitted inputs. No value of
// this type grants authority; owned dispatch still checks the live S0b scope.
type PackInputs struct {
	Scenario []byte
	Binding  RunBinding
	Jobs     map[string]JobPin
	Restarts map[string]GenerationRestartPlan
	Pauses   map[string]ProcessPausePlan `json:",omitempty"`
	Boots    map[string]string
	Scripts  map[string][]byte
	Files    map[string]map[string]string
}

func (c *ActionContext) boundPackContext() error {
	if c == nil || c.approved == nil || c.Safety == nil || c.Safety != c.approved.scope || c.Scenario != c.approved.scenario {
		return errors.New("no bound approved pack context")
	}
	return nil
}

// ApprovedPackInputs only succeeds inside this ApprovedRun's exact Engine
// context. A caller-created Safety scope, Vars or YAML flag is insufficient.
func (c *ActionContext) ApprovedPackInputs() (PackInputs, error) {
	if err := c.boundPackContext(); err != nil {
		return PackInputs{}, err
	}
	a := c.approved
	a.mu.Lock()
	defer a.mu.Unlock()
	p := PackInputs{Binding: a.request.Binding, Jobs: maps.Clone(a.runPolicy.Jobs), Restarts: maps.Clone(a.runPolicy.GenerationRestarts), Pauses: maps.Clone(a.runPolicy.ProcessPauses), Boots: map[string]string{}, Scripts: map[string][]byte{}}
	p.Scenario = append([]byte(nil), a.inputs.Scenario...)
	p.Files = map[string]map[string]string{}
	for node, r := range a.readbacks {
		p.Files[node] = maps.Clone(r.Files)
	}
	p.Binding.Hosts = append([]ApprovedHost(nil), p.Binding.Hosts...)
	for k, v := range p.Jobs {
		v.Argv = append([]string(nil), v.Argv...)
		p.Jobs[k] = v
	}
	for k, v := range a.readbacks {
		p.Boots[k] = v.BootID
	}
	for k, v := range a.inputs.Scripts {
		p.Scripts[k] = append([]byte(nil), v...)
	}
	return p, nil
}

// RunApprovedPackJob resolves an exact independently approved label. It never
// accepts a submitted executable, argv, backend, verifier or replacement scope.
func (c *ActionContext) RunApprovedPackJob(ctx context.Context, label string, background bool) (JobHandle, ExitInfo, error) {
	h, err := c.startApprovedPackJob(ctx, label, background)
	if err != nil || background {
		return h, ExitInfo{}, err
	}
	e, err := c.WaitApprovedPackTask(ctx, h)
	return h, e, err
}

// StartApprovedPackTask enrolls an ordinary bounded task without waiting. A pack
// may overlap read load with a restart, then wait on the TASK, never the victim.
func (c *ActionContext) StartApprovedPackTask(ctx context.Context, label string) (JobHandle, error) {
	h, err := c.startApprovedPackJob(ctx, label, false)
	c.FailApprovedPack(err)
	return h, err
}

func (c *ActionContext) startApprovedPackJob(ctx context.Context, label string, background bool) (JobHandle, error) {
	p, err := c.ApprovedPackInputs()
	if err != nil {
		return JobHandle{}, err
	}
	pin, ok := p.Jobs[label]
	if !ok {
		return JobHandle{}, errors.New("unknown approved pack job")
	}
	// Keep the action's tighter bound through enrollment and authorization;
	// cancellation of the original work envelope still fences this context.
	work, cancel := context.WithDeadline(ctx, c.Safety.admission.WorkDeadline)
	defer cancel()
	stop := context.AfterFunc(c.Safety.ctx, cancel)
	defer stop()
	if err := work.Err(); err != nil {
		return JobHandle{}, err
	}
	return c.Safety.start(work, JobSpec{Node: pin.Node, Executable: pin.Executable, SHA256: pin.SHA256, Argv: pin.Argv, retainExit: background, packLabel: label, CaptureOutput: !background}, false)
}

// WaitApprovedPackTask preserves exact owned stdout and terminal failure. It
// refuses background service handles: restart alone owns the victim's Wait.
func (c *ActionContext) WaitApprovedPackTask(ctx context.Context, h JobHandle) (e ExitInfo, err error) {
	defer func() { c.FailApprovedPack(err) }()
	if err = c.boundPackContext(); err != nil {
		return
	}
	j, unlock, err := c.Safety.lockJob(ctx, h)
	if err != nil {
		return e, err
	}
	valid := !j.spec.retainExit && j.spec.CaptureOutput && j.spec.packLabel != ""
	unlock()
	if !valid {
		return e, errors.New("task waiter refuses a service generation")
	}
	e, err = c.Safety.Wait(ctx, h)

	return e, err
}

// FailApprovedPack retains a pack admission/execution failure in the original
// S0b run even if submitted YAML requests ignore_error. It never grants work.
func (c *ActionContext) FailApprovedPack(err error) {
	if err != nil && c != nil && c.approved != nil && c.Safety == c.approved.scope {
		c.approved.revoke(err)
	}
}
