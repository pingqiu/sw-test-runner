package v3block

// The block product's declared actions (product/actions.json), run by the
// generic executor (package declared). This file supplies only what the
// executor asks a product for: the node, the subject (the runner's record
// of one volume, opVolume) and the outcome table. The actions that stay Go
// handlers are listed in actions.json under "exceptions", each with why.

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

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

// boundedNode bounds every command of a Go handler: a command runs at most
// until its context ends (bound when the context has none), and the runner
// stops waiting then (declared.Call). No command of an exception can hold
// the run.
type boundedNode struct {
	inner runner
	bound time.Duration
}

func (b boundedNode) Run(ctx context.Context, cmd string) (string, string, int, error) {
	return b.call(ctx, b.inner.Run, cmd)
}

func (b boundedNode) RunRoot(ctx context.Context, cmd string) (string, string, int, error) {
	return b.call(ctx, b.inner.RunRoot, cmd)
}

func (b boundedNode) call(ctx context.Context, run func(context.Context, string) (string, string, int, error), cmd string) (string, string, int, error) {
	if _, has := ctx.Deadline(); !has {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, b.bound)
		defer cancel()
	}
	return declared.Call(ctx, run, cmd)
}

// runnerDeadline is the runner deadline of the Go handler name
// (actions.json exceptions). A handler the description gives no deadline
// does not run. (The frozen copies of replaced handlers take their declared
// action's longest variant deadline.)
func runnerDeadline(name string, params map[string]string) (time.Duration, error) {
	if d, ok := blockActions.File.ExceptionDeadline(name, params); ok {
		if exceptionDeadlineOverride > 0 {
			return exceptionDeadlineOverride, nil
		}
		return d, nil
	}
	for _, a := range blockActions.File.Actions {
		if a.Action == name {
			var d time.Duration
			for _, v := range a.Variants {
				d = max(d, time.Duration(v.RunnerDeadlineSeconds)*time.Second)
			}
			return d, nil
		}
	}
	return 0, fmt.Errorf("%s: product/actions.json declares no runner deadline for it", name)
}

// guestConfig is the description's guest configuration: runner settings for
// the kernel NVMe/TCP host the runner connects, not product facts.
type guestConfig struct {
	About                 string `json:"about"`
	DeviceIODeadlineSecs  int    `json:"device_io_runner_deadline_seconds"`
	DeviceIODeadlineAbout string `json:"device_io_runner_deadline_about"`
	Connect               struct {
		About     string   `json:"about"`
		Options   []string `json:"options"`
		SelectVar string   `json:"select_var"`
		Default   struct {
			Name     string `json:"name"`
			Evidence bool   `json:"evidence"`
			Cite     string `json:"cite"`
		} `json:"default"`
		ValuesSeen []struct {
			Value  string `json:"value"`
			Source string `json:"source"`
		} `json:"values_seen"`
	} `json:"connect"`
}

// guestConnect is the connect a run uses: the extra fabrics options
// (",key=value..." in the declared order; "" for the product's plain
// connect) and the label the history records.
type guestConnect struct {
	Name, Options, Label string
}

var guest = func() guestConfig {
	var g guestConfig
	if err := strictJSON(blockActions.File.Guest, &g); err != nil {
		panic("v3block: product/actions.json guest: " + err.Error())
	}
	if g.DeviceIODeadlineSecs <= 0 || g.Connect.SelectVar == "" || g.Connect.Default.Name == "" || !g.Connect.Default.Evidence {
		panic("v3block: product/actions.json guest: device_io_runner_deadline_seconds, connect.select_var and an evidence default connect are required")
	}
	for _, v := range g.Connect.ValuesSeen {
		if _, err := parseConnect(g.Connect.Options, v.Value); err != nil || v.Source == "" {
			panic(fmt.Sprintf("v3block: guest connect value %q: %v (and a source is required)", v.Value, err))
		}
	}
	return g
}()

// exceptionDeadlineOverride replaces every Go handler's runner deadline
// (tests).
var exceptionDeadlineOverride time.Duration

// deviceDeadlineOverride replaces the device I/O runner deadline (tests).
var deviceDeadlineOverride time.Duration

func deviceDeadline() time.Duration {
	if deviceDeadlineOverride > 0 {
		return deviceDeadlineOverride
	}
	return time.Duration(guest.DeviceIODeadlineSecs) * time.Second
}

// selectConnect resolves the run's connect from its guest_connect value:
// empty is the product's plain connect (the only one whose results are
// release evidence); otherwise the declared options it sets, in the
// declared order, labelled as a variant.
func selectConnect(value string) (guestConnect, error) {
	c := guest.Connect
	set, err := parseConnect(c.Options, value)
	if err != nil {
		return guestConnect{}, fmt.Errorf("%s %q: %w", c.SelectVar, value, err)
	}
	if len(set) == 0 {
		return guestConnect{Name: c.Default.Name, Label: c.Default.Name + " (the product's connect, kernel defaults)"}, nil
	}
	var opts, parts []string
	for _, o := range c.Options {
		if n, ok := set[o]; ok {
			opts = append(opts, fmt.Sprintf(",%s=%d", o, n))
			parts = append(parts, fmt.Sprintf("%s=%d", o, n))
		}
	}
	return guestConnect{Name: "variant", Options: strings.Join(opts, ""), Label: "guest connect variant: " + strings.Join(parts, ",")}, nil
}

// parseConnect reads "name=value" pairs (space or comma separated; a name
// with - or _) into kernel option names; a name not among options, a value
// that is not an integer, or a name given twice is refused.
func parseConnect(options []string, value string) (map[string]int, error) {
	known := map[string]bool{}
	for _, o := range options {
		known[o] = true
	}
	set := map[string]int{}
	for _, kv := range strings.FieldsFunc(value, func(r rune) bool { return r == ' ' || r == ',' || r == '\t' }) {
		k, v, ok := strings.Cut(kv, "=")
		k = strings.ReplaceAll(strings.TrimLeft(k, "-"), "-", "_")
		n, err := strconv.Atoi(v)
		switch {
		case !ok || err != nil:
			return nil, fmt.Errorf("%q is not name=integer", kv)
		case !known[k]:
			return nil, fmt.Errorf("unknown connect option %q (known: %s)", k, strings.Join(options, ", "))
		}
		if _, dup := set[k]; dup {
			return nil, fmt.Errorf("connect option %q given twice", k)
		}
		set[k] = n
	}
	return set, nil
}

func strictJSON(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	return d.Decode(v)
}
