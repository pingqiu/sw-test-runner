// Package declared runs a product's DECLARED actions: an action is a row of
// the product's description (actions.json), not a Go function. A row names
// the node it runs on, the command as a template over the step's parameters,
// the run's configuration and the subject's fields, a timeout, how the answer
// is decoded into ok / refused / unknown (through the product's outcome
// table), and the result fields it records. One executor runs every row and
// writes the history records: the invocation, then the completion.
//
// A product supplies three things through Executor, none of them a decision
// about an outcome: how to find the node, how to read and update a subject
// (the runner's own record of one volume, say), and its outcome table.
// This package imports no product pack (the import guard checks it).
package declared

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	tr "github.com/pingqiu/sw-test-runner"
)

// Runner runs one shell command on a node: stdout, stderr, the exit code,
// and an error when the command could not be run at all.
type Runner interface {
	Run(ctx context.Context, cmd string) (string, string, int, error)
	RunRoot(ctx context.Context, cmd string) (string, string, int, error)
}

// Subject is the runner's own record of the thing an action works on. Field
// may run a command on the node (a liveness check), so a field is read only
// when a template or a variant's condition names it.
type Subject interface {
	Field(ctx context.Context, node Runner, name string) (string, error)
	Set(field, value string) error
	Save()
}

// LookupFunc decides a failed answer from the product's outcome table:
// refused, refused_effect (with the documented effect: part, to, cite) or
// unknown, for (action, path, code, message).
type LookupFunc func(action, path, code, message string) (string, map[string]any)

// File is a product's actions description.
type File struct {
	Product string `json:"product"`
	ReadAt  string `json:"read_at"`
	About   string `json:"about,omitempty"`
	Config  Config `json:"config"`
	// RunnerIODir is where a detached command keeps its output and exit
	// code on the node (a template over config.<key>).
	RunnerIODir string      `json:"runner_io_dir"`
	Actions     []Action    `json:"actions"`
	Exceptions  []Exception `json:"exceptions"`
	// RunnerSetup names the registered actions that are runner setup, not
	// product actions, and why.
	RunnerSetup []Setup `json:"runner_setup,omitempty"`
	// Guest is the product's guest configuration, read by the product's
	// own code; the executor does not interpret it.
	Guest json.RawMessage `json:"guest,omitempty"`
}

// Config is the configuration every declared action shares, each key read
// from the step's params, else from the run's vars (a var naming other vars
// is expanded). No key may stay unresolved.
type Config struct {
	Keys []ConfigKey `json:"keys"`
	// Client is the key whose value is the history's client name.
	Client string `json:"client"`
}

// ConfigKey is one shared configuration key.
type ConfigKey struct {
	Name     string `json:"name"`
	Required bool   `json:"required,omitempty"`
	Default  string `json:"default,omitempty"`
}

// Exception names an action that stays a Go handler, why, and its runner
// deadline: the RUNNER's bound on one operation, never a product bound. At
// the deadline the operation is recorded unknown and the run moves on. With
// PlusParam the deadline grows by that int param's seconds (a wait the step
// may set).
type Exception struct {
	Action string `json:"action"`
	Why    string `json:"why"`
	// DeclarableWith names the generic executor feature that would let the
	// action be declared.
	DeclarableWith        string     `json:"declarable_with"`
	RunnerDeadlineSeconds int        `json:"runner_deadline_seconds"`
	PlusParam             *PlusParam `json:"plus_param,omitempty"`
}

// Setup is a runner setup action and why it is not a product action.
type Setup struct {
	Action string `json:"action"`
	Why    string `json:"why"`
}

// PlusParam is an int step param added to a deadline (Default when absent
// or not an int).
type PlusParam struct {
	Name    string `json:"name"`
	Default int    `json:"default"`
}

// ExceptionDeadline is the runner deadline of the Go handler name for a
// step with params; false when the description lists no such exception.
func (f *File) ExceptionDeadline(name string, params map[string]string) (time.Duration, bool) {
	for _, x := range f.Exceptions {
		if x.Action != name {
			continue
		}
		d := time.Duration(x.RunnerDeadlineSeconds) * time.Second
		if p := x.PlusParam; p != nil {
			n, err := strconv.Atoi(params[p.Name])
			if err != nil {
				n = p.Default
			}
			d += time.Duration(max(n, 0)) * time.Second
		}
		return d, true
	}
	return 0, false
}

// Action is one declared action.
type Action struct {
	Action string `json:"action"` // the registered name
	Verb   string `json:"verb"`   // the history record's verb
	Op     string `json:"op"`     // the history record's Module.Action id
	// RunsOn is where the command runs: "step_node", the step's node (the
	// first node when the step names none).
	RunsOn string  `json:"runs_on"`
	AsRoot bool    `json:"as_root,omitempty"`
	Params []Param `json:"params"`
	Record Record  `json:"record"`
	// Variants are tried in order; the first whose condition holds (or that
	// has none) runs. The chosen variant's name is {{variant}}.
	Variants []Variant `json:"variants"`
	Retry    *Retry    `json:"retry,omitempty"`
	// OnOKSet updates a subject's field after an ok answer:
	// "<subject param>.<field>" -> template.
	OnOKSet map[string]string `json:"on_ok_set,omitempty"`
	// Kind "observe": an observation of Steps (observe.go), not an
	// operation; it has no op and no outcome.
	Kind  string `json:"kind,omitempty"`
	Steps []Step `json:"steps,omitempty"`
	// Derived values, {{derived.<name>}} (observe.go).
	Derived map[string]Derived `json:"derived,omitempty"`
	Note    string             `json:"note,omitempty"`
}

// Param is one parameter of the step. Kind "subject" loads the subject it
// names (its fields are then {{<param>.<field>}}); "int" parses an integer
// (a bad or absent value is the default); otherwise a string.
type Param struct {
	Name     string `json:"name"`
	Kind     string `json:"kind,omitempty"`
	Vars     bool   `json:"vars,omitempty"` // absent from the params, read the run's var of that name
	Required bool   `json:"required,omitempty"`
	Default  string `json:"default,omitempty"`
}

// Record is what the invocation records: the subject's name and the args.
type Record struct {
	Volume string           `json:"volume"`
	Args   map[string]Value `json:"args"`
}

// Value is a template; with Type "int" it is recorded as a number.
type Value struct {
	Template string
	Type     string
}

func (v *Value) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		return json.Unmarshal(b, &v.Template)
	}
	var o struct {
		Template string `json:"template"`
		Type     string `json:"type"`
	}
	if err := strict(b, &o); err != nil {
		return err
	}
	if o.Type != "int" {
		return fmt.Errorf("value type %q (want int)", o.Type)
	}
	v.Template, v.Type = o.Template, o.Type
	return nil
}

// Word is one word of a command: a template, or {"json": {...}}, an object
// of templates written as one JSON word. Every word is shell quoted.
type Word struct {
	Text string
	JSON map[string]string
}

func (w *Word) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		return json.Unmarshal(b, &w.Text)
	}
	var o struct {
		JSON map[string]string `json:"json"`
	}
	if err := strict(b, &o); err != nil {
		return err
	}
	if len(o.JSON) == 0 {
		return errors.New("a command word is a string or {\"json\": {...}}")
	}
	w.JSON = o.JSON
	return nil
}

// Cond holds when the field's value equals Equals.
type Cond struct {
	Field  string `json:"field"`
	Equals string `json:"equals"`
	// NotEquals, when set, holds when the field's value differs from it.
	NotEquals *string `json:"not_equals,omitempty"`
}

// Variant is one way to run the action: its command and its decoding.
type Variant struct {
	Name    string `json:"name,omitempty"`
	When    *Cond  `json:"when,omitempty"`
	Command []Word `json:"command"`
	// RunnerDeadlineSeconds is the runner's bound on the command, not a
	// product bound: at it the command's session is killed (what survives
	// is abandoned and recorded) and the operation is unknown.
	RunnerDeadlineSeconds int    `json:"runner_deadline_seconds"`
	Decode                Decode `json:"decode"`
	// Cite is where the product states the command and its flags.
	Cite string `json:"cite"`
}

// Decode turns the command's answer into an outcome and result fields.
//
// Kind "error_body": exit 0 is ok; otherwise stdout is the product's JSON
// error body {code, message} and the outcome table decides (action, path,
// code, message); no body is unknown. The detail is "code: message".
// Answer "http_status_last_line": stdout's last line is an HTTP status (200
// counts as exit 0, any other as 1) and the body precedes it; a command
// that fails or exits non-zero is TransportFailure (template over {{exit}}).
//
// Kind "self_reported": the command reports its own outcome in a JSON
// answer (Fields: name -> string | int | bool, decoded as encoding/json
// decodes a struct). The answer counts only if it decodes, every Match
// field equals its template, OutcomeField is one of Exits and the exit code
// is that outcome's, and on ok every OKLength field has that length.
// Anything else is unknown. Result fields are recorded always, ResultOK
// fields only on ok; on any other outcome Detail (template over
// {{answer.<field>}} and {{stderr}}, first line) is the detail.
type Decode struct {
	Kind             string            `json:"kind"`
	Action           string            `json:"action,omitempty"` // outcome table action; default the verb
	Path             string            `json:"path,omitempty"`
	Answer           string            `json:"answer,omitempty"`
	TransportFailure string            `json:"transport_failure,omitempty"`
	Fields           map[string]string `json:"fields,omitempty"`
	OutcomeField     string            `json:"outcome_field,omitempty"`
	Exits            map[string]int    `json:"exits,omitempty"`
	Match            map[string]string `json:"match,omitempty"`
	OKLength         map[string]int    `json:"ok_length,omitempty"`
	Result           map[string]string `json:"result,omitempty"`
	ResultOK         map[string]string `json:"result_ok,omitempty"`
	Detail           string            `json:"detail,omitempty"`
}

// Retry reruns the same command while the answer's error code is WhileCode,
// within the int param WithinParam (seconds), pausing EverySeconds between
// two attempts. A step that declares expect=refused is one call. Every
// attempt is listed in the result under Attempts; the outcome is the last.
type Retry struct {
	WhileCode    string `json:"while_code"`
	WithinParam  string `json:"within_param"`
	EverySeconds int    `json:"every_seconds"`
	Attempts     string `json:"attempts"`
	// ExpectedRefusal, when set, lets a step that declares expect=refused
	// rerun too: when its bool param is true, while the answer is WhileCode
	// AND its message contains WhileMessageContains (a refusal that is not
	// the one the step probes). The first other answer is the one decoded.
	ExpectedRefusal *ExpectedRefusalRetry `json:"expected_refusal,omitempty"`
}

// ExpectedRefusalRetry names the bool param that turns it on and the
// product text of the refusal it reruns through.
type ExpectedRefusalRetry struct {
	Param                string `json:"param"`
	WhileMessageContains string `json:"while_message_contains"`
	Why                  string `json:"why,omitempty"`
}

func strict(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	return d.Decode(v)
}

// ref is a template reference: {{name}}, or {{name|q}} for the value
// quoted as Go's %q quotes a string.
var ref = regexp.MustCompile(`\{\{\s*([A-Za-z_][\w.]*(?:\|q)?)\s*\}\}`)

func names(t string) []string {
	var out []string
	for _, m := range ref.FindAllStringSubmatch(t, -1) {
		out = append(out, strings.TrimSuffix(m[1], "|q"))
	}
	return out
}

// Parse reads and checks a description: every action has a name, verb, op,
// node, at least one variant, and each variant a command, a cite, a timeout
// and a known decoding; every template names only what is defined where it
// is used; no action is both declared and an exception.
func Parse(b []byte) (*File, error) {
	var f File
	if err := strict(b, &f); err != nil {
		return nil, fmt.Errorf("actions: %w", err)
	}
	if f.Product == "" || f.ReadAt == "" {
		return nil, errors.New("actions: product and read_at are required")
	}
	keys := map[string]bool{}
	for _, k := range f.Config.Keys {
		keys[k.Name] = true
	}
	if f.Config.Client != "" && !keys[f.Config.Client] {
		return nil, fmt.Errorf("actions: client key %q is not a config key", f.Config.Client)
	}
	seen := map[string]bool{}
	for i := range f.Actions {
		a := &f.Actions[i]
		if err := a.check(keys); err != nil {
			return nil, fmt.Errorf("actions: %s: %w", a.Action, err)
		}
		if seen[a.Action] {
			return nil, fmt.Errorf("actions: %s declared twice", a.Action)
		}
		seen[a.Action] = true
	}
	if len(f.Actions) > 0 {
		for _, n := range names(f.RunnerIODir) {
			if !strings.HasPrefix(n, "config.") || !keys[strings.TrimPrefix(n, "config.")] {
				return nil, fmt.Errorf("actions: runner_io_dir names %q, not a config key", n)
			}
		}
		if f.RunnerIODir == "" {
			return nil, errors.New("actions: runner_io_dir is required")
		}
	}
	for _, x := range f.Exceptions {
		if x.Action == "" || strings.TrimSpace(x.Why) == "" {
			return nil, fmt.Errorf("actions: exception %q needs its action and why", x.Action)
		}
		if x.RunnerDeadlineSeconds <= 0 {
			return nil, fmt.Errorf("actions: exception %s has no runner_deadline_seconds", x.Action)
		}
		if strings.TrimSpace(x.DeclarableWith) == "" {
			return nil, fmt.Errorf("actions: exception %s does not name the executor feature that would declare it", x.Action)
		}
		if x.PlusParam != nil && x.PlusParam.Name == "" {
			return nil, fmt.Errorf("actions: exception %s: plus_param needs a name", x.Action)
		}
		if seen[x.Action] {
			return nil, fmt.Errorf("actions: %s is both declared and an exception", x.Action)
		}
		seen[x.Action] = true
	}
	for _, x := range f.RunnerSetup {
		if x.Action == "" || strings.TrimSpace(x.Why) == "" {
			return nil, fmt.Errorf("actions: runner setup %q needs its action and why", x.Action)
		}
		if seen[x.Action] {
			return nil, fmt.Errorf("actions: %s is listed twice", x.Action)
		}
		seen[x.Action] = true
	}
	return &f, nil
}

func (a *Action) check(keys map[string]bool) error {
	if a.Action == "" || a.Verb == "" || (a.Op == "") != (a.Kind == "observe") {
		return errors.New("action and verb are required, and an op exactly when it is not an observation")
	}
	if a.RunsOn != "step_node" {
		return fmt.Errorf("runs_on %q (want step_node)", a.RunsOn)
	}
	if a.Kind != "" && a.Kind != "observe" {
		return fmt.Errorf("kind %q", a.Kind)
	}
	if len(a.Variants) == 0 && a.Kind != "observe" {
		return errors.New("no variant")
	}
	plain, subjects, ints := map[string]bool{"variant": true}, map[string]bool{}, map[string]bool{}
	for k := range keys {
		plain["config."+k] = true
	}
	for k := range a.Derived {
		plain["derived."+k] = true
	}
	for _, p := range a.Params {
		switch p.Kind {
		case "subject":
			subjects[p.Name] = true
		case "bool":
			if p.Default != "" && p.Default != "true" && p.Default != "false" {
				return fmt.Errorf("param %s: default %q is not a bool", p.Name, p.Default)
			}
		case "int":
			ints[p.Name] = true
			if p.Default != "" {
				if _, err := strconv.Atoi(p.Default); err != nil {
					return fmt.Errorf("param %s: default %q is not an int", p.Name, p.Default)
				}
			}
		case "":
		default:
			return fmt.Errorf("param %s: kind %q", p.Name, p.Kind)
		}
		plain[p.Name] = true
	}
	defined := func(extra map[string]bool) func(string) error {
		return func(t string) error {
			for _, n := range names(t) {
				if plain[n] || extra[n] {
					continue
				}
				if i := strings.IndexByte(n, '.'); i > 0 && subjects[n[:i]] {
					continue
				}
				return fmt.Errorf("template %q: %q is not defined here", t, n)
			}
			return nil
		}
	}
	base := defined(nil)
	if err := base(a.Record.Volume); err != nil {
		return err
	}
	if a.Kind == "observe" {
		return a.checkObserve(defined)
	}
	if len(a.Derived) != 0 {
		return errors.New("derived values serve observations only")
	}
	for _, v := range a.Record.Args {
		if err := base(v.Template); err != nil {
			return err
		}
	}
	for i, v := range a.Variants {
		at := fmt.Sprintf("variant %d", i)
		if v.When == nil && i != len(a.Variants)-1 {
			return fmt.Errorf("%s: only the last variant may have no condition", at)
		}
		if v.When != nil {
			if err := base("{{" + v.When.Field + "}}"); err != nil {
				return fmt.Errorf("%s: %w", at, err)
			}
		}
		if len(v.Command) == 0 || strings.TrimSpace(v.Cite) == "" || v.RunnerDeadlineSeconds <= 0 {
			return fmt.Errorf("%s: command, cite and a positive runner_deadline_seconds are required", at)
		}
		for _, w := range v.Command {
			for _, t := range append([]string{w.Text}, mapValues(w.JSON)...) {
				if err := base(t); err != nil {
					return fmt.Errorf("%s: %w", at, err)
				}
			}
		}
		d := v.Decode
		switch d.Kind {
		case "error_body":
			if d.Path == "" {
				return fmt.Errorf("%s: error_body needs the path the outcome table names", at)
			}
			switch d.Answer {
			case "":
			case "http_status_last_line":
				if err := defined(map[string]bool{"exit": true})(d.TransportFailure); err != nil || d.TransportFailure == "" {
					return fmt.Errorf("%s: transport_failure: %v", at, err)
				}
			default:
				return fmt.Errorf("%s: answer %q", at, d.Answer)
			}
		case "self_reported":
			if _, ok := d.Exits[tr.OutcomeOK]; !ok || d.Fields[d.OutcomeField] != "string" {
				return fmt.Errorf("%s: self_reported needs a string outcome_field and the ok exit", at)
			}
			for o := range d.Exits {
				if o != tr.OutcomeOK && o != tr.OutcomeRefused && o != tr.OutcomeUnknown {
					return fmt.Errorf("%s: exit for outcome %q", at, o)
				}
			}
			answer := map[string]bool{"stderr": true}
			for f, typ := range d.Fields {
				if typ != "string" && typ != "int" && typ != "bool" {
					return fmt.Errorf("%s: field %s type %q", at, f, typ)
				}
				answer["answer."+f] = true
			}
			for _, m := range []map[string]string{d.Result, d.ResultOK} {
				for _, f := range m {
					if d.Fields[f] == "" {
						return fmt.Errorf("%s: result from %q, not an answer field", at, f)
					}
				}
			}
			for f, t := range d.Match {
				if d.Fields[f] != "string" {
					return fmt.Errorf("%s: match on %q, not a string answer field", at, f)
				}
				if err := base(t); err != nil {
					return fmt.Errorf("%s: %w", at, err)
				}
			}
			for f := range d.OKLength {
				if d.Fields[f] != "string" {
					return fmt.Errorf("%s: ok_length on %q, not a string field", at, f)
				}
			}
			if err := defined(answer)(d.Detail); err != nil {
				return fmt.Errorf("%s: %w", at, err)
			}
			for k, t := range a.OnOKSet {
				if i := strings.IndexByte(k, '.'); i <= 0 || !subjects[k[:i]] {
					return fmt.Errorf("on_ok_set %q: not <subject>.<field>", k)
				}
				if err := defined(answer)(t); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("%s: decode kind %q", at, d.Kind)
		}
		if d.Kind != "self_reported" && len(a.OnOKSet) > 0 {
			return errors.New("on_ok_set reads the answer: only with a self_reported decoding")
		}
	}
	if r := a.Retry; r != nil {
		if r.WhileCode == "" || !ints[r.WithinParam] || r.EverySeconds <= 0 || r.Attempts == "" {
			return errors.New("retry needs while_code, an int within_param, every_seconds and attempts")
		}
		if er := r.ExpectedRefusal; er != nil {
			isBool := false
			for _, p := range a.Params {
				isBool = isBool || (p.Name == er.Param && p.Kind == "bool")
			}
			if !isBool || er.WhileMessageContains == "" {
				return errors.New("retry.expected_refusal needs a bool param and while_message_contains")
			}
		}
		for _, v := range a.Variants {
			if v.Decode.Kind != "error_body" {
				return errors.New("retry reruns on an error body's code")
			}
		}
	}
	return nil
}

func mapValues(m map[string]string) []string {
	var out []string
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

// Executor runs the declared actions of one description.
type Executor struct {
	File *File
	// Node finds the node a step names ("" the first).
	Node func(actx *tr.ActionContext, name string) (Runner, error)
	// Subject loads the subject a step names.
	Subject func(actx *tr.ActionContext, name string) (Subject, error)
	// Lookup is the product's outcome table.
	Lookup LookupFunc
	// Pause, when set, replaces a retry's declared pause (tests).
	Pause func(declared time.Duration) time.Duration
	// Deadline, when set, replaces a variant's declared runner deadline
	// (tests).
	Deadline func(declared time.Duration) time.Duration
	// Poll is how often a detached command is polled (default 200ms).
	Poll time.Duration
}

// Handlers returns one handler per declared action, by its name.
func (x *Executor) Handlers() map[string]tr.ActionHandlerFunc {
	out := map[string]tr.ActionHandlerFunc{}
	for i := range x.File.Actions {
		out[x.File.Actions[i].Action] = x.Handler(x.File.Actions[i].Action)
	}
	return out
}

// Handler returns the handler of the declared action name; it panics when
// the description does not declare it (a registration error).
func (x *Executor) Handler(name string) tr.ActionHandlerFunc {
	for i := range x.File.Actions {
		if a := &x.File.Actions[i]; a.Action == name {
			return func(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
				return x.exec(ctx, actx, act, a)
			}
		}
	}
	panic("declared: no action " + name)
}

// scope holds what templates may name for one step.
type scope struct {
	ctx      context.Context
	node     Runner
	vals     map[string]string
	subjects map[string]Subject
	derived  map[string]Derived
}

func (s *scope) get(n string) (string, error) {
	if q, ok := strings.CutSuffix(n, "|q"); ok {
		v, err := s.get(q)
		return strconv.Quote(v), err
	}
	if v, ok := s.vals[n]; ok {
		return v, nil
	}
	if d, ok := strings.CutPrefix(n, "derived."); ok && s.derived != nil {
		if spec, ok := s.derived[d]; ok {
			v, err := spec.compute(s)
			if err != nil {
				return "", derivedError{err.Error()}
			}
			return v, nil
		}
	}
	if i := strings.IndexByte(n, '.'); i > 0 {
		if sub, ok := s.subjects[n[:i]]; ok {
			return sub.Field(s.ctx, s.node, n[i+1:])
		}
	}
	return "", fmt.Errorf("template: %q is not defined", n)
}

// expand substitutes every {{name}} once; a substituted value is never
// expanded again.
func (s *scope) expand(t string) (string, error) {
	var b strings.Builder
	last := 0
	for _, m := range ref.FindAllStringSubmatchIndex(t, -1) {
		v, err := s.get(t[m[2]:m[3]])
		if err != nil {
			return "", err
		}
		b.WriteString(t[last:m[0]])
		b.WriteString(v)
		last = m[1]
	}
	b.WriteString(t[last:])
	return b.String(), nil
}

// command builds the shell command: every word expanded, then shell quoted.
func (s *scope) command(words []Word) (string, error) {
	out := make([]string, len(words))
	for i, w := range words {
		if w.JSON != nil {
			obj := map[string]string{}
			for k, t := range w.JSON {
				v, err := s.expand(t)
				if err != nil {
					return "", err
				}
				obj[k] = v
			}
			b, _ := json.Marshal(obj)
			out[i] = ShellQuote(string(b))
			continue
		}
		v, err := s.expand(w.Text)
		if err != nil {
			return "", err
		}
		out[i] = ShellQuote(v)
	}
	return strings.Join(out, " "), nil
}

var varRef = regexp.MustCompile(`\{\{\s*(\w+)\s*\}\}`)

// configValue is a param, else a var; a var's value may name other vars,
// expanded here (bounded; an unknown name stays).
func configValue(act tr.Action, actx *tr.ActionContext, k string) string {
	v := act.Params[k]
	if v == "" {
		v = actx.Vars[k]
	}
	for i := 0; i < 4 && strings.Contains(v, "{{"); i++ {
		v = varRef.ReplaceAllStringFunc(v, func(m string) string {
			if x, ok := actx.Vars[varRef.FindStringSubmatch(m)[1]]; ok {
				return x
			}
			return m
		})
	}
	return v
}

func paramValue(act tr.Action, actx *tr.ActionContext, p Param) string {
	if v := act.Params[p.Name]; v != "" || !p.Vars {
		return v
	}
	return actx.Vars[p.Name]
}

func andList(ns []string) string {
	if len(ns) < 2 {
		return strings.Join(ns, "")
	}
	return strings.Join(ns[:len(ns)-1], ", ") + " and " + ns[len(ns)-1]
}

func (x *Executor) exec(ctx context.Context, actx *tr.ActionContext, act tr.Action, a *Action) (map[string]string, error) {
	name := a.Action
	if actx.History == nil {
		return nil, fmt.Errorf("%s: no history (typed operations run only with a run bundle)", name)
	}
	node, err := x.Node(actx, act.Node)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	s := &scope{ctx: ctx, node: node, vals: map[string]string{}, subjects: map[string]Subject{}}
	expect := act.Params["expect"]
	if expect != "" && expect != tr.OutcomeRefused {
		return nil, fmt.Errorf("%s: expect %q; a step may declare only %q", name, expect, tr.OutcomeRefused)
	}
	var required []string
	missing := false
	for _, k := range x.File.Config.Keys {
		v := configValue(act, actx, k.Name)
		if strings.Contains(v, "{{") {
			return nil, fmt.Errorf("%s: unresolved configuration %q", name, v)
		}
		if v == "" {
			v = k.Default
		}
		if k.Required {
			required = append(required, k.Name)
			missing = missing || v == ""
		}
		s.vals["config."+k.Name] = v
	}
	if missing {
		return nil, fmt.Errorf("%s: %s are required (params or vars)", name, andList(required))
	}
	for _, p := range a.Params {
		switch p.Kind {
		case "bool":
			v := paramValue(act, actx, p)
			if v == "" {
				v = p.Default
			}
			if v == "" {
				v = "false"
			}
			b, err := strconv.ParseBool(v)
			if err != nil {
				return nil, fmt.Errorf("%s: %s %q is not true or false", name, p.Name, v)
			}
			s.vals[p.Name] = strconv.FormatBool(b)
		case "int":
			def, _ := strconv.Atoi(p.Default)
			n, err := strconv.Atoi(paramValue(act, actx, p))
			if err != nil {
				n = def
			}
			s.vals[p.Name] = strconv.Itoa(n)
		default:
			v := paramValue(act, actx, p)
			if v == "" && (p.Required || p.Kind == "subject") {
				return nil, fmt.Errorf("%s: %s is required", name, p.Name)
			}
			s.vals[p.Name] = v
			if p.Kind == "subject" {
				sub, err := x.Subject(actx, v)
				if err != nil {
					return nil, fmt.Errorf("%s: %w", name, err)
				}
				s.subjects[p.Name] = sub
			}
		}
	}
	if a.Kind == "observe" {
		s.derived = a.Derived
		vol, err := s.expand(a.Record.Volume)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		return x.observe(s, actx, a, vol)
	}
	var v *Variant
	for i := range a.Variants {
		w := a.Variants[i].When
		if w == nil {
			v = &a.Variants[i]
			break
		}
		got, err := s.get(w.Field)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if got == w.Equals {
			v = &a.Variants[i]
			break
		}
	}
	if v == nil {
		return nil, fmt.Errorf("%s: no variant applies", name)
	}
	s.vals["variant"] = v.Name
	cmd, err := s.command(v.Command)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	vol, err := s.expand(a.Record.Volume)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	args := map[string]any{}
	for k, val := range a.Record.Args {
		t, err := s.expand(val.Template)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if val.Type == "int" {
			n, err := strconv.Atoi(t)
			if err != nil {
				return nil, fmt.Errorf("%s: arg %s %q is not an int", name, k, t)
			}
			args[k] = n
		} else {
			args[k] = t
		}
	}
	if expect != "" {
		args["expect"] = expect
	}
	seq, err := actx.History.Invoke(tr.HistoryRecord{Client: s.vals["config."+x.File.Config.Client], Op: a.Op, Verb: a.Verb, Volume: vol, Args: args})
	if err != nil {
		return nil, err
	}
	outcome, result := x.run(s, a, v, cmd, expect)
	if err := actx.History.Complete(seq, outcome, result); err != nil {
		return nil, err
	}
	actx.Log("  %s(%s): %s", name, vol, outcome)
	return map[string]string{"value": outcome}, nil
}

// run runs the command once, or again as the retry declares.
func (x *Executor) run(s *scope, a *Action, v *Variant, cmd, expect string) (string, map[string]any) {
	r := a.Retry
	if r == nil {
		out, res, _ := x.once(s, a, v, cmd)
		return out, res
	}
	within, _ := strconv.Atoi(s.vals[r.WithinParam])
	end := time.Now().Add(time.Duration(within) * time.Second)
	pause := time.Duration(r.EverySeconds) * time.Second
	if x.Pause != nil {
		pause = x.Pause(pause)
	}
	var attempts []string
	for {
		out, res, transport := x.once(s, a, v, cmd)
		if transport {
			res[r.Attempts] = attempts
			return out, res
		}
		d, _ := res["detail"].(string)
		attempts = append(attempts, fmt.Sprintf("%s %s", out, FirstLine(d)))
		res[r.Attempts] = attempts
		again := strings.HasPrefix(d, r.WhileCode+":")
		if expect == tr.OutcomeRefused {
			er := r.ExpectedRefusal
			again = again && er != nil && s.vals[er.Param] == "true" && strings.Contains(d, er.WhileMessageContains)
		}
		if out == tr.OutcomeOK || !again || time.Now().After(end) {
			return out, res
		}
		select {
		case <-s.ctx.Done():
			return out, res
		case <-time.After(pause):
		}
	}
}

// once runs the command and decodes its answer; transport is true when the
// command could not be run (or, for an HTTP answer, did not answer).
func (x *Executor) once(s *scope, a *Action, v *Variant, cmd string) (string, map[string]any, bool) {
	deadline := time.Duration(v.RunnerDeadlineSeconds) * time.Second
	if x.Deadline != nil {
		deadline = x.Deadline(deadline)
	}
	poll := x.Poll
	if poll <= 0 {
		poll = 200 * time.Millisecond
	}
	dir, err := s.expand(x.File.RunnerIODir)
	if err != nil {
		return tr.OutcomeUnknown, map[string]any{"detail": err.Error()}, true
	}
	stdout, stderr, code, err := RunDetached(s.ctx, s.node, a.AsRoot, dir, cmd, deadline, poll)
	if t, ok := AsDetached(err); ok {
		return tr.OutcomeUnknown, TimeoutResult(t), true
	}
	d := v.Decode
	switch d.Kind {
	case "error_body":
		if d.Answer == "http_status_last_line" {
			if err != nil || code != 0 {
				s.vals["exit"] = strconv.Itoa(code)
				msg, _ := s.expand(d.TransportFailure)
				return tr.OutcomeUnknown, map[string]any{"detail": msg}, true
			}
			i := strings.LastIndex(strings.TrimRight(stdout, "\n"), "\n")
			code = 1
			if strings.TrimSpace(stdout[i+1:]) == "200" {
				code = 0
			}
			stdout = stdout[:max(i, 0)]
		} else if err != nil {
			return tr.OutcomeUnknown, map[string]any{"detail": err.Error()}, true
		}
		action := d.Action
		if action == "" {
			action = a.Verb
		}
		out, detail, effect := DecodeErrorBody(x.Lookup, action, d.Path, stdout, code)
		res := map[string]any{}
		if detail != "" {
			res["detail"] = detail
		}
		if effect != nil {
			res["effect"] = effect
		}
		return out, res, false
	case "self_reported":
		if err != nil {
			return tr.OutcomeUnknown, map[string]any{"detail": err.Error()}, true
		}
		ans, out := decodeSelf(d, s, stdout, code)
		res := map[string]any{}
		for k, f := range d.Result {
			res[k] = ans[f]
		}
		for f, val := range ans {
			s.vals["answer."+f] = fmt.Sprint(val)
		}
		s.vals["stderr"] = stderr
		if out != tr.OutcomeOK {
			msg, _ := s.expand(d.Detail)
			res["detail"] = FirstLine(msg)
			return out, res, false
		}
		for k, f := range d.ResultOK {
			res[k] = ans[f]
		}
		keys := make([]string, 0, len(a.OnOKSet))
		for k := range a.OnOKSet {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			val, err := s.expand(a.OnOKSet[k])
			i := strings.IndexByte(k, '.')
			if err == nil {
				err = s.subjects[k[:i]].Set(k[i+1:], val)
			}
			if err != nil {
				res["detail"] = "on_ok_set " + k + ": " + err.Error()
				return tr.OutcomeUnknown, res, false
			}
		}
		for _, sub := range s.subjects {
			sub.Save()
		}
		return out, res, false
	}
	return tr.OutcomeUnknown, map[string]any{"detail": "decode kind " + d.Kind}, false
}

// decodeSelf decodes a self-reported answer as encoding/json decodes a flat
// struct of the declared fields: keys match exactly, else case-insensitively;
// a value of the wrong type leaves its field zero and makes the answer
// unknown, as a syntax error does.
func decodeSelf(d Decode, s *scope, stdout string, code int) (map[string]any, string) {
	ans := map[string]any{}
	for f, typ := range d.Fields {
		switch typ {
		case "string":
			ans[f] = ""
		case "int":
			ans[f] = int64(0)
		case "bool":
			ans[f] = false
		}
	}
	var raw map[string]json.RawMessage
	bad := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &raw) != nil
	if !bad {
		for f, typ := range d.Fields {
			b, ok := raw[f]
			if !ok {
				for k, x := range raw {
					if strings.EqualFold(k, f) {
						b, ok = x, true
						break
					}
				}
			}
			if !ok {
				continue
			}
			var err error
			switch typ {
			case "string":
				var x string
				err = json.Unmarshal(b, &x)
				if err == nil {
					ans[f] = x
				}
			case "int":
				var x int64
				err = json.Unmarshal(b, &x)
				if err == nil {
					ans[f] = x
				}
			case "bool":
				var x bool
				err = json.Unmarshal(b, &x)
				if err == nil {
					ans[f] = x
				}
			}
			bad = bad || err != nil
		}
	}
	if bad {
		return ans, tr.OutcomeUnknown
	}
	for f, t := range d.Match {
		want, err := s.expand(t)
		if err != nil || ans[f] != want {
			return ans, tr.OutcomeUnknown
		}
	}
	o, _ := ans[d.OutcomeField].(string)
	want, known := d.Exits[o]
	if !known || want != code {
		return ans, tr.OutcomeUnknown
	}
	if o == tr.OutcomeOK {
		for f, n := range d.OKLength {
			if x, _ := ans[f].(string); len(x) != n {
				return ans, tr.OutcomeUnknown
			}
		}
	}
	return ans, o
}

// DecodeErrorBody maps one call's exit code and JSON error body to an
// outcome: exit 0 ok; a code the table refuses for (action, path), refused,
// or refused_effect with its documented effect; every other answer
// unknown. The detail is "code: message", or why there is no code.
func DecodeErrorBody(lookup LookupFunc, action, path, stdout string, code int) (string, string, map[string]any) {
	if code == 0 {
		return tr.OutcomeOK, "", nil
	}
	var e struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		Retryable bool   `json:"retryable"`
	}
	if json.Unmarshal([]byte(strings.TrimSpace(stdout)), &e) != nil || e.Code == "" {
		return tr.OutcomeUnknown, fmt.Sprintf("exit %d without an error body", code), nil
	}
	out, effect := lookup(action, path, e.Code, e.Message)
	if out != tr.OutcomeRefusedEffect {
		effect = nil
	}
	return out, e.Code + ": " + e.Message, effect
}

// ShellQuote leaves s bare only when every byte is plainly safe for a POSIX
// shell; otherwise it wraps s in single quotes, escaping embedded ones.
func ShellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789@%+=:,./_-") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// FirstLine is s trimmed, up to its first newline, at most 200 bytes.
func FirstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// TimeoutResult is the result of an operation its runner deadline ended:
// the detail, and the processes that survived the kill, if any.
func TimeoutResult(t *Detached) map[string]any {
	res := map[string]any{"detail": t.Error()}
	if len(t.Abandoned) > 0 {
		res["abandoned"] = t.Abandoned
	}
	return res
}
