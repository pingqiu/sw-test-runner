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
	"sort"
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
	case "written":
		// Every lba a write was attempted on, in numeric order.
		var out []uint64
		for k := range v.Written {
			n, _ := strconv.ParseUint(k, 10, 64)
			out = append(out, n)
		}
		sort.Slice(out, func(a, b int) bool { return out[a] < out[b] })
		parts := make([]string, len(out))
		for i, n := range out {
			parts[i] = strconv.FormatUint(n, 10)
		}
		return strings.Join(parts, " "), nil
	}
	return "", fmt.Errorf("volume field %q", f)
}

func (s *volumeSubject) Set(f, val string) error {
	switch f {
	case "layout_id":
		s.v.LayoutID = val
	case "written":
		// A write attempted on lba val (set before its invoke).
		if s.v.Written == nil {
			s.v.Written = map[string]bool{}
		}
		s.v.Written[val] = true
	default:
		return fmt.Errorf("volume field %q is not settable", f)
	}
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
		Lookup: outcomes.lookupFunc(),
		OpDeadline: func(d time.Duration) time.Duration {
			if exceptionDeadlineOverride > 0 {
				return exceptionDeadlineOverride
			}
			return d
		},
	}
}()

func init() {
	// The run values: the guest connect the run selected (its label and its
	// kernel options), from the description's guest section (set here: the
	// guest configuration is read from the executor's description).
	blockActions.Values = func(actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
		c, err := selectConnect(opParam(act, actx, guest.Connect.SelectVar))
		if err != nil {
			return nil, err
		}
		return map[string]string{"guest_connect": c.Label, "guest_connect_options": c.Options}, nil
	}
}

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
			if d, ok := a.RunnerDeadline(params); ok {
				if exceptionDeadlineOverride > 0 {
					return exceptionDeadlineOverride, nil
				}
				return d, nil
			}
			var d time.Duration
			for _, v := range a.Variants {
				d = max(d, time.Duration(v.RunnerDeadlineSeconds)*time.Second)
			}
			for _, st := range a.Steps {
				d += time.Duration(st.RunnerDeadlineSeconds) * time.Second
			}
			return d, nil
		}
	}
	return 0, fmt.Errorf("%s: product/actions.json declares no runner deadline for it", name)
}

// guestConfig is the description's guest configuration: runner settings for
// the kernel NVMe/TCP host the runner connects, not product facts.
type guestConfig struct {
	About   string `json:"about"`
	Connect struct {
		About     string   `json:"about"`
		Options   []string `json:"options"`
		SelectVar string   `json:"select_var"`
		Default   struct {
			Name     string `json:"name"`
			Evidence bool   `json:"evidence"`
			Cite     string `json:"cite"`
		} `json:"default"`
		Variants []struct {
			Name     string          `json:"name"`
			Options  json.RawMessage `json:"options"`
			Evidence bool            `json:"evidence"`
			Source   string          `json:"source"`
			Why      string          `json:"why"`
		} `json:"variants"`
	} `json:"connect"`
	// variants are the declared variants' options, read and checked at load.
	variants map[string]map[string]int
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
	if g.Connect.SelectVar == "" || g.Connect.Default.Name == "" || !g.Connect.Default.Evidence {
		panic("v3block: product/actions.json guest: connect.select_var and an evidence default connect are required")
	}
	g.variants = map[string]map[string]int{}
	for _, v := range g.Connect.Variants {
		opts, err := variantOptions(g.Connect.Options, v.Options)
		switch {
		case err != nil:
			panic("v3block: guest connect variant " + v.Name + ": " + err.Error())
		case v.Name == "" || v.Name == g.Connect.Default.Name || g.variants[v.Name] != nil:
			panic("v3block: guest connect variant " + v.Name + ": a variant needs its own name")
		case v.Evidence || v.Source == "" || len(opts) == 0:
			panic("v3block: guest connect variant " + v.Name + ": a variant is never evidence, names its source and sets options")
		}
		g.variants[v.Name] = opts
	}
	return g
}()

// exceptionDeadlineOverride replaces every operation-level runner deadline:
// a Go handler's and a declared step kind's (tests).
var exceptionDeadlineOverride time.Duration

// selectConnect resolves the run's connect by name: "" or the default's
// name is the product's plain connect (the only one whose results are
// release evidence); a declared variant's name gives its options, in the
// declared order, labelled as a variant. Any other name is refused.
func selectConnect(name string) (guestConnect, error) {
	c := guest.Connect
	if name == "" || name == c.Default.Name {
		return guestConnect{Name: c.Default.Name, Label: c.Default.Name + " (the product's connect, kernel defaults)"}, nil
	}
	set, ok := guest.variants[name]
	if !ok {
		return guestConnect{}, fmt.Errorf("%s %q is not a declared guest connect", c.SelectVar, name)
	}
	var opts, parts []string
	for _, o := range c.Options {
		if n, ok := set[o]; ok {
			opts = append(opts, fmt.Sprintf(",%s=%d", o, n))
			parts = append(parts, fmt.Sprintf("%s=%d", o, n))
		}
	}
	return guestConnect{Name: name, Options: strings.Join(opts, ""), Label: "guest connect variant: " + strings.Join(parts, ",")}, nil
}

// variantOptions reads a variant's options object into the kernel's option
// names: a name may be written with - or _; a name not among options, a
// value that is not an integer, or a name given twice (in either spelling)
// is refused.
func variantOptions(options []string, raw json.RawMessage) (map[string]int, error) {
	known := map[string]bool{}
	for _, o := range options {
		known[o] = true
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if t, err := d.Token(); err != nil || t != json.Delim('{') {
		return nil, fmt.Errorf("options is not an object")
	}
	set := map[string]int{}
	for d.More() {
		t, err := d.Token()
		if err != nil {
			return nil, err
		}
		k := strings.ReplaceAll(t.(string), "-", "_")
		var v any
		if err := d.Decode(&v); err != nil {
			return nil, err
		}
		num, isNum := v.(json.Number)
		n, err := strconv.Atoi(string(num))
		switch {
		case !known[k]:
			return nil, fmt.Errorf("unknown connect option %q (known: %s)", t, strings.Join(options, ", "))
		case !isNum || err != nil:
			return nil, fmt.Errorf("connect option %q: %v is not an integer", t, v)
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
