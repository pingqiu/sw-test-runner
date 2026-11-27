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
//
// Result keys. A key "result" or "result_<S>" is the RESULT AT STAGE S:
// the fields written into the history record's result when the step
// reaches that stage, and nothing else. The stages are: result (the stage
// of its place: a decoded answer, an ok I/O, a sequence's start, a
// sequence step that holds), result_ok (a decoded ok), result_unanswered (an
// I/O that gave no answer), result_launched and result_ready (a process
// launched, then ready), result_failure, result_not_run and
// result_answered (a sequence step that failed, could not run, or
// answered at all). No result key decides an outcome; each is read where
// its stage is reached (Decode, device.go IO, process.go Process,
// sequence.go SeqStep and Sequence).
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
	// RunValues names the values a product's runner settings give the step
	// kinds ({{run.<name>}}), each with why; the executor's Values supplies
	// them, and a step it cannot supply them for does not run.
	RunValues []RunValue `json:"run_values,omitempty"`
	// DeviceResolvers are the product's declared guest-device resolvers, by
	// name (resolver.go).
	DeviceResolvers map[string]*Resolver `json:"device_resolvers,omitempty"`
	scripts         map[string]string
	// Guest is the product's guest configuration, read by the product's
	// own code; the executor does not interpret it.
	Guest json.RawMessage `json:"guest,omitempty"`
}

// Config is the configuration every declared action shares, each key read
// from the step's params, else from the run's vars (a var naming other vars
// is expanded). No key may stay unresolved. A Computed key is not read:
// the product computes it from the other keys (Executor.Compute).
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
	// Computed says how the product computes the key (and where); such a
	// key has no default and is not required.
	Computed string `json:"computed,omitempty"`
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

// RunValue is a value of the product's runner settings and why a step
// reads it.
type RunValue struct {
	Name string `json:"name"`
	Why  string `json:"why"`
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

	// The step kinds (device.go, bounded.go): the operation's runner
	// deadline, plus the int param PlusParam names (a wait the step sets).
	RunnerDeadlineSeconds int        `json:"runner_deadline_seconds,omitempty"`
	PlusParam             *PlusParam `json:"plus_param,omitempty"`
	// Kind "device": Each names a list param (one operation per item),
	// Input a runner-generated input, Device the lookup, IO the I/O.
	Each   string        `json:"each,omitempty"`
	Input  *Input        `json:"input,omitempty"`
	Device *DeviceLookup `json:"device,omitempty"`
	IO     *IO           `json:"io,omitempty"`
	// OnInvokeSet updates a subject's field just before each invoke:
	// "<subject param>.<field>" -> template.
	OnInvokeSet map[string]string `json:"on_invoke_set,omitempty"`
	// Kind "process" (process.go).
	Process *ProcessStep `json:"process,omitempty"`
	// OnOutcomeSet updates subjects' fields after the operation completed
	// with that outcome: outcome -> "<subject param>.<field>" -> template.
	OnOutcomeSet map[string]map[string]string `json:"on_outcome_set,omitempty"`
	// Kind "sequence" (sequence.go).
	Sequence *Sequence `json:"sequence,omitempty"`
}

// Param is one parameter of the step. Kind "subject" loads the subject it
// names (its fields are then {{<param>.<field>}}); "int" parses an integer
// (a bad or absent value is the default); "bool" true or false; "enum" one
// of Values (absent: the default); "list" whitespace-separated items, each
// matching ItemPattern, where a value that is a key of Aliases reads that
// subject field instead (a list of the subject's own); otherwise a string,
// which must match Pattern when one is declared. A value that breaks its
// kind's rule is the runner's error: nothing is recorded.
type Param struct {
	Name     string `json:"name"`
	Kind     string `json:"kind,omitempty"`
	Vars     bool   `json:"vars,omitempty"` // absent from the params, read the run's var of that name
	Required bool   `json:"required,omitempty"`
	Default  string `json:"default,omitempty"`
	Pattern  string `json:"pattern,omitempty"`
	// enum
	Values []string `json:"values,omitempty"`
	// list
	ItemPattern string            `json:"item_pattern,omitempty"`
	Aliases     map[string]string `json:"aliases,omitempty"`
	NonEmpty    bool              `json:"non_empty,omitempty"`
}

// Record is what the invocation records: the subject's name and the args.
type Record struct {
	Volume string           `json:"volume"`
	Args   map[string]Value `json:"args"`
}

// Value is a template; with Type "int" it is recorded as a number, with
// "bool" as true or false. An Optional value is left out when it expands
// to "" (or, for an int, to no int).
type Value struct {
	Template string
	Type     string
	Optional bool
}

func (v *Value) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		return json.Unmarshal(b, &v.Template)
	}
	var o struct {
		Template string `json:"template"`
		Type     string `json:"type"`
		Optional bool   `json:"optional"`
	}
	if err := strict(b, &o); err != nil {
		return err
	}
	if o.Type != "int" && o.Type != "bool" && !(o.Type == "" && o.Optional) {
		return fmt.Errorf("value type %q (want int or bool)", o.Type)
	}
	v.Template, v.Type, v.Optional = o.Template, o.Type, o.Optional
	return nil
}

// Word is one word of a command: a template, or {"json": {...}}, an object
// of templates written as one JSON word; every such word is shell quoted.
// {"raw": "&&"} is shell text written as it is (an operator or a
// redirection; it takes no template). {"when": cond, "words": [...]} is a
// group of words used only when the condition holds. {"script": path} is
// one word: the text of a script file the product supplies (ParseWith),
// without its final newline, shell quoted (a program given to an
// interpreter's -c, kept out of the description as a file with its own tests).
type Word struct {
	Text   string
	JSON   map[string]string
	Raw    string
	When   *Cond
	Words  []Word
	Script string
}

func (w *Word) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		return json.Unmarshal(b, &w.Text)
	}
	var o struct {
		JSON   map[string]string `json:"json"`
		Raw    string            `json:"raw"`
		When   *Cond             `json:"when"`
		Words  []Word            `json:"words"`
		Script string            `json:"script"`
	}
	if err := strict(b, &o); err != nil {
		return err
	}
	if o.Script != "" {
		if o.JSON != nil || o.Raw != "" || o.When != nil || o.Words != nil {
			return errors.New("a script word names its script only")
		}
		w.Script = o.Script
		return nil
	}
	switch {
	case len(o.JSON) > 0 && o.Raw == "" && o.When == nil && o.Words == nil:
		w.JSON = o.JSON
	case o.Raw != "" && o.JSON == nil && o.When == nil && o.Words == nil:
		w.Raw = o.Raw
	case o.When != nil && len(o.Words) > 0 && o.JSON == nil && o.Raw == "":
		w.When, w.Words = o.When, o.Words
	default:
		return errors.New("a command word is a string, {\"json\": {...}}, {\"raw\": text}, {\"script\": path} or {\"when\": cond, \"words\": [...]}")
	}
	return nil
}

// Cond holds when the field's value equals Equals.
type Cond struct {
	Field  string `json:"field"`
	Equals string `json:"equals"`
	// NotEquals, when set, holds when the field's value differs from it.
	NotEquals *string `json:"not_equals,omitempty"`
}

// UnmarshalJSON reads a condition as the vocabulary allows it: ONE field
// compared, by equals or by not_equals (not both), with a constant (no
// template).
func (c *Cond) UnmarshalJSON(b []byte) error {
	var o struct {
		Field     string  `json:"field"`
		Equals    *string `json:"equals"`
		NotEquals *string `json:"not_equals"`
	}
	if err := strict(b, &o); err != nil {
		return err
	}
	if o.Field == "" || (o.Equals == nil) == (o.NotEquals == nil) {
		return errors.New("a condition is one field with equals or not_equals")
	}
	for _, v := range []*string{o.Equals, o.NotEquals} {
		if v != nil && strings.Contains(*v, "{{") {
			return fmt.Errorf("condition on %s: its value is a constant, not a template", o.Field)
		}
	}
	c.Field, c.NotEquals = o.Field, o.NotEquals
	if o.Equals != nil {
		c.Equals = *o.Equals
	}
	return nil
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

// Retry declares the rerun of the same command, as the documented operator
// does, ONLY where an operator page tells the operator to run it again for
// that answer: PageCite is that page line (docs/<page>.md:<line>), and an
// answer in the product marked "retryable" is not one. The rerun is for the
// answer whose error code is WhileCode and whose message contains
// WhileMessageContains (the page's answer), and only when that answer
// decodes refused: an unknown is never re-issued. The executor runs ONE attempt
// per call and never loops: the DRIVER re-issues the action (Rerun says
// when), so every attempt is its OWN operation in the history: an invoke
// and a complete, its args carrying the request id (RequestArg, one of the
// record's args) and "attempt": n (the step param AttemptParam). The COUNT
// is the bound: at most MaxAttempts attempts, EverySeconds apart; the int
// param WithinParam (seconds), when set, is only a wall-clock guard. The
// last attempt's outcome stands. A step that declares expect=refused is one
// attempt, unless ExpectedRefusal names a bool param it sets true.
type Retry struct {
	WhileCode            string `json:"while_code,omitempty"`
	WhileMessageContains string `json:"while_message_contains,omitempty"`
	// Answers lists the page's answers when there are several, each a code
	// and (optional, when the page names the code alone) a message phrase.
	Answers         []RerunAnswer         `json:"answers,omitempty"`
	MaxAttempts     int                   `json:"max_attempts"`
	EverySeconds    int                   `json:"every_seconds"`
	WithinParam     string                `json:"within_param,omitempty"`
	RequestArg      string                `json:"request_arg"`
	PageCite        string                `json:"page_cite"`
	ExpectedRefusal *ExpectedRefusalRetry `json:"expected_refusal,omitempty"`
	Note            string                `json:"note,omitempty"`
}

// RerunAnswer is one answer a page tells the operator to run the command
// again for: its error code, and the phrase its message contains when the
// page names one.
type RerunAnswer struct {
	Code            string `json:"code"`
	MessageContains string `json:"message_contains,omitempty"`
}

// scriptRefs lists the paths every {"script": path} word of a description
// names, in order of first appearance.
func scriptRefs(b []byte) []string {
	var d any
	if json.Unmarshal(b, &d) != nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	var walk func(any)
	walk = func(o any) {
		switch o := o.(type) {
		case map[string]any:
			if p, ok := o["script"].(string); ok && len(o) == 1 && !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
			for _, v := range o {
				walk(v)
			}
		case []any:
			for _, v := range o {
				walk(v)
			}
		}
	}
	walk(d)
	return out
}

// answers is the retry's rerun answers: WhileCode's, then Answers.
func (r *Retry) answers() []RerunAnswer {
	var out []RerunAnswer
	if r.WhileCode != "" {
		out = append(out, RerunAnswer{r.WhileCode, r.WhileMessageContains})
	}
	return append(out, r.Answers...)
}

// matches says whether an answer's detail ("code: message") is one the page
// names.
func (r *Retry) matches(detail string) bool {
	for _, a := range r.answers() {
		if strings.HasPrefix(detail, a.Code+":") && strings.Contains(detail, a.MessageContains) {
			return true
		}
	}
	return false
}

// ExpectedRefusalRetry names the bool param that lets a step declaring
// expect=refused rerun through the same answer (a refusal that is not the
// one the step probes).
type ExpectedRefusalRetry struct {
	Param string `json:"param"`
	Why   string `json:"why,omitempty"`
}

// pageCite is a citation of an operator page line.
var pageCite = regexp.MustCompile(`^docs/[A-Za-z0-9._/-]+\.md:[0-9]+`)

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
func Parse(b []byte) (*File, error) { return ParseWith(b, nil) }

// ParseWith is Parse for a description whose script words name files of
// scripts (repository path -> text). A script that is not supplied, or
// whose text is not a file's (a carriage return, or no final newline),
// refuses the description.
func ParseWith(b []byte, scripts map[string]string) (*File, error) {
	var f File
	f.scripts = map[string]string{}
	for _, p := range scriptRefs(b) {
		text, ok := scripts[p]
		switch {
		case !ok:
			return nil, fmt.Errorf("actions: script %s is not supplied", p)
		case strings.Contains(text, "\r") || !strings.HasSuffix(text, "\n"):
			return nil, fmt.Errorf("actions: script %s: LF line ends and a final newline", p)
		}
		f.scripts[p] = strings.TrimSuffix(text, "\n")
	}
	if err := strict(b, &f); err != nil {
		return nil, fmt.Errorf("actions: %w", err)
	}
	if f.Product == "" || f.ReadAt == "" {
		return nil, errors.New("actions: product and read_at are required")
	}
	keys := map[string]bool{}
	for _, k := range f.Config.Keys {
		keys[k.Name] = true
		if k.Computed != "" && (k.Required || k.Default != "") {
			return nil, fmt.Errorf("actions: config key %s is computed: no default, not required", k.Name)
		}
	}
	if f.Config.Client != "" && !keys[f.Config.Client] {
		return nil, fmt.Errorf("actions: client key %q is not a config key", f.Config.Client)
	}
	run := map[string]bool{}
	for _, r := range f.RunValues {
		if r.Name == "" || strings.TrimSpace(r.Why) == "" {
			return nil, fmt.Errorf("actions: run value %q needs its name and why", r.Name)
		}
		run["run."+r.Name] = true
	}
	seen := map[string]bool{}
	for i := range f.Actions {
		a := &f.Actions[i]
		if err := a.check(keys, run); err != nil {
			return nil, fmt.Errorf("actions: %s: %w", a.Action, err)
		}
		if seen[a.Action] {
			return nil, fmt.Errorf("actions: %s declared twice", a.Action)
		}
		seen[a.Action] = true
	}
	if err := f.checkSequences(keys, run); err != nil {
		return nil, err
	}
	for name, r := range f.DeviceResolvers {
		if err := r.check(); err != nil {
			return nil, fmt.Errorf("actions: device resolver %s: %w", name, err)
		}
	}
	for _, n := range f.resolverRefs() {
		if f.DeviceResolvers[n] == nil {
			return nil, fmt.Errorf("actions: device resolver %q is named and not declared", n)
		}
	}
	if err := f.checkReadbacks(); err != nil {
		return nil, err
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

func (a *Action) check(keys, run map[string]bool) error {
	if a.Action == "" || a.Verb == "" || (a.Op == "") != (a.Kind == "observe") {
		return errors.New("action and verb are required, and an op exactly when it is not an observation")
	}
	if a.RunsOn != "step_node" {
		return fmt.Errorf("runs_on %q (want step_node)", a.RunsOn)
	}
	stepKind := a.Kind == "device" || a.Kind == "process" || a.Kind == "sequence"
	if a.Kind != "" && a.Kind != "observe" && !stepKind {
		return fmt.Errorf("kind %q", a.Kind)
	}
	if len(a.Variants) == 0 && a.Kind == "" {
		return errors.New("no variant")
	}
	if !stepKind && (a.RunnerDeadlineSeconds != 0 || a.PlusParam != nil || a.Each != "" || a.Input != nil || a.Device != nil || a.IO != nil ||
		len(a.OnInvokeSet) != 0 || a.Process != nil || len(a.OnOutcomeSet) != 0 || a.Sequence != nil) {
		return errors.New("runner_deadline_seconds, plus_param, each, input, device, io, on_invoke_set, process, on_outcome_set and sequence belong to the step kinds")
	}
	if a.Kind == "device" && (a.Process != nil || len(a.OnOutcomeSet) != 0) {
		return errors.New("process and on_outcome_set do not belong to a device action")
	}
	for o := range a.OnOutcomeSet {
		if o != tr.OutcomeOK && o != tr.OutcomeUnknown {
			return fmt.Errorf("on_outcome_set %q: only ok and unknown change state (a refusal changes nothing)", o)
		}
	}
	plain, subjects, ints, lists := map[string]bool{"variant": true}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	for k := range keys {
		plain["config."+k] = true
	}
	for k := range a.Derived {
		plain["derived."+k] = true
	}
	if stepKind {
		for k := range run {
			plain[k] = true
		}
	}
	for _, p := range a.Params {
		switch p.Kind {
		case "subject":
			subjects[p.Name] = true
		case "process":
			if a.Kind != "process" {
				return fmt.Errorf("param %s: a process param belongs to a process action", p.Name)
			}
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
		case "enum":
			if len(p.Values) == 0 || (p.Default != "" && !contains(p.Values, p.Default)) {
				return fmt.Errorf("param %s: an enum needs values, and its default among them", p.Name)
			}
		case "list":
			lists[p.Name] = true
			if _, err := regexp.Compile(p.ItemPattern); err != nil || p.ItemPattern == "" {
				return fmt.Errorf("param %s: a list needs an item_pattern: %v", p.Name, err)
			}
			for k, f := range p.Aliases {
				if i := strings.IndexByte(f, '.'); k == "" || i <= 0 || !subjects[f[:i]] {
					return fmt.Errorf("param %s: alias %q -> %q is not a field of an earlier subject param", p.Name, k, f)
				}
			}
		case "":
			if _, err := regexp.Compile(p.Pattern); err != nil {
				return fmt.Errorf("param %s: pattern: %w", p.Name, err)
			}
		default:
			return fmt.Errorf("param %s: kind %q", p.Name, p.Kind)
		}
		if p.Kind != "" && p.Pattern != "" {
			return fmt.Errorf("param %s: a pattern is for a string param", p.Name)
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
	if stepKind {
		if err := checkDerived(a.Derived, base); err != nil {
			return err
		}
		for o, sets := range a.OnOutcomeSet {
			for k, t := range sets {
				if i := strings.IndexByte(k, '.'); i <= 0 || !subjects[k[:i]] {
					return fmt.Errorf("on_outcome_set %s %q: not <subject>.<field>", o, k)
				}
				if err := base(t); err != nil {
					return err
				}
			}
		}
		if a.Kind == "sequence" {
			if len(a.OnInvokeSet) != 0 {
				return errors.New("a sequence has no on_invoke_set")
			}
			for _, v := range a.Record.Args {
				if err := base(v.Template); err != nil {
					return err
				}
			}
			return nil // its steps are checked once every action is read (steps_of)
		}
		if a.Kind == "process" {
			for k, t := range a.OnInvokeSet {
				if i := strings.IndexByte(k, '.'); i <= 0 || !subjects[k[:i]] {
					return fmt.Errorf("on_invoke_set %q: not <subject>.<field>", k)
				}
				if err := base(t); err != nil {
					return err
				}
			}
			for _, v := range a.Record.Args {
				if err := base(v.Template); err != nil {
					return err
				}
			}
			return a.checkProcess(defined, subjects, ints)
		}
		return a.checkDevice(defined, subjects, lists)
	}
	if len(a.Derived) != 0 {
		return errors.New("derived values serve observations and the step kinds only")
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
		if err := checkWords(v.Command, base); err != nil {
			return fmt.Errorf("%s: %w", at, err)
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
		if !pageCite.MatchString(r.PageCite) {
			return fmt.Errorf("a rerun needs the operator page line that tells the operator to run the command again (page_cite docs/<page>.md:<line>), not %q: without one there is no rerun", r.PageCite)
		}
		if len(r.answers()) == 0 || r.MaxAttempts < 2 || r.EverySeconds <= 0 {
			return errors.New("retry needs the page's answers (while_code, or answers), max_attempts of 2 or more and every_seconds")
		}
		if r.WhileCode == "" && r.WhileMessageContains != "" {
			return errors.New("retry: while_message_contains without while_code")
		}
		for _, an := range r.answers() {
			if an.Code == "" {
				return errors.New("retry: an answer without its code")
			}
		}
		if r.WithinParam != "" && !ints[r.WithinParam] {
			return errors.New("retry.within_param must be an int param")
		}
		if _, ok := a.Record.Args[r.RequestArg]; !ok || r.RequestArg == "" {
			return errors.New("retry.request_arg must name a record arg (the request id every attempt repeats)")
		}
		if _, clash := a.Record.Args["attempt"]; clash {
			return errors.New("a retried action's record has no arg named attempt: the executor writes it")
		}
		if er := r.ExpectedRefusal; er != nil {
			isBool := false
			for _, p := range a.Params {
				isBool = isBool || (p.Name == er.Param && p.Kind == "bool")
			}
			if !isBool {
				return errors.New("retry.expected_refusal needs a bool param")
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

func contains(set []string, v string) bool {
	for _, x := range set {
		if x == v {
			return true
		}
	}
	return false
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
	// Values supplies the description's run values (RunValues) for a step
	// of a step kind; an error stops the step before anything is recorded.
	Values func(actx *tr.ActionContext, act tr.Action) (map[string]string, error)
	// Compute supplies the description's computed configuration keys from
	// the others (by name, without "config."): each key's value, or why it
	// cannot be computed. A step that names a key that cannot be computed
	// does not run (as a value it cannot expand).
	Compute func(config map[string]string) (vals, why map[string]string)
	// OpDeadline, when set, replaces a step kind's runner deadline (tests).
	OpDeadline func(declared time.Duration) time.Duration
	// Process resolves a process param: the process's record as a subject,
	// or why the runner knows no such process (the operation is refused).
	Process func(ctx context.Context, actx *tr.ActionContext, act tr.Action, node Runner, name string) (Subject, string)
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
	// uncomputed: computed configuration the product could not compute,
	// and why.
	uncomputed map[string]string
	params     map[string]string // the step's own params
	// scripts: the description's script files, by path.
	scripts map[string]string
	// unresolved: process params the product could not resolve, and why.
	unresolved map[string]string
	procName   string
	// fatal: a probe found the run's evidence configuration broken; the
	// action fails the run after its record.
	fatal string
}

func (s *scope) get(n string) (string, error) {
	if q, ok := strings.CutSuffix(n, "|q"); ok {
		v, err := s.get(q)
		return strconv.Quote(v), err
	}
	if v, ok := s.vals[n]; ok {
		return v, nil
	}
	if why, ok := s.uncomputed[n]; ok {
		return "", derivedError{why}
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
	out, err := s.words(words)
	return strings.Join(out, " "), err
}

func (s *scope) words(words []Word) ([]string, error) {
	var out []string
	for _, w := range words {
		switch {
		case w.Script != "":
			text, ok := s.scripts[w.Script]
			if !ok {
				return nil, fmt.Errorf("script %s is not supplied", w.Script)
			}
			out = append(out, ShellQuote(text))
		case w.Raw != "":
			out = append(out, w.Raw)
		case w.When != nil:
			hold, err := s.holds(w.When)
			if err != nil {
				return nil, err
			}
			if hold {
				sub, err := s.words(w.Words)
				if err != nil {
					return nil, err
				}
				out = append(out, sub...)
			}
		case w.JSON != nil:
			obj := map[string]string{}
			for k, t := range w.JSON {
				v, err := s.expand(t)
				if err != nil {
					return nil, err
				}
				obj[k] = v
			}
			b, _ := json.Marshal(obj)
			out = append(out, ShellQuote(string(b)))
		default:
			v, err := s.expand(w.Text)
			if err != nil {
				return nil, err
			}
			out = append(out, ShellQuote(v))
		}
	}
	return out, nil
}

// argv expands words into a command line (each word one argument, not
// quoted): a process's argv. A raw word has no place in an argv.
func (s *scope) argv(words []Word) ([]string, error) {
	var out []string
	for _, w := range words {
		switch {
		case w.Raw != "" || w.JSON != nil || w.Script != "":
			return nil, errors.New("an argv word is a template or a group")
		case w.When != nil:
			hold, err := s.holds(w.When)
			if err != nil {
				return nil, err
			}
			if hold {
				sub, err := s.argv(w.Words)
				if err != nil {
					return nil, err
				}
				out = append(out, sub...)
			}
		default:
			v, err := s.expand(w.Text)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
	}
	return out, nil
}

var varRef = regexp.MustCompile(`\{\{\s*(\w+)\s*\}\}`)

// computeConfig sets the computed configuration keys from the others.
func (x *Executor) computeConfig(s *scope, computed []string) error {
	if len(computed) == 0 {
		return nil
	}
	if x.Compute == nil {
		return errors.New("the description names computed configuration and the product computes none")
	}
	cfg := map[string]string{}
	for k, v := range s.vals {
		if n, ok := strings.CutPrefix(k, "config."); ok {
			cfg[n] = v
		}
	}
	vals, whys := x.Compute(cfg)
	for _, k := range computed {
		if why, ok := whys[k]; ok {
			if s.uncomputed == nil {
				s.uncomputed = map[string]string{}
			}
			s.uncomputed["config."+k] = why
			continue
		}
		v, ok := vals[k]
		if !ok {
			return fmt.Errorf("computed configuration %q not supplied", k)
		}
		s.vals["config."+k] = v
	}
	return nil
}

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
	s := &scope{ctx: ctx, node: node, vals: map[string]string{}, subjects: map[string]Subject{}, params: act.Params, scripts: x.File.scripts}
	expect := act.Params["expect"]
	if expect != "" && expect != tr.OutcomeRefused {
		return nil, fmt.Errorf("%s: expect %q; a step may declare only %q", name, expect, tr.OutcomeRefused)
	}
	if a.Retry == nil && act.Params[AttemptParam] != "" {
		return nil, fmt.Errorf("%s: %s is the driver's, for an action with a declared rerun only", name, AttemptParam)
	}
	var required []string
	missing := false
	var computed []string
	for _, k := range x.File.Config.Keys {
		if k.Computed != "" {
			computed = append(computed, k.Name)
			continue
		}
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
	if err := x.computeConfig(s, computed); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
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
		case "process":
			v := paramValue(act, actx, p)
			s.vals[p.Name] = v
			if x.Process == nil {
				return nil, fmt.Errorf("%s: the product resolves no process", name)
			}
			pctx, cancel := context.WithTimeout(ctx, x.opDeadline(s, a))
			sub, why := x.Process(pctx, actx, act, node, v)
			cancel()
			if why != "" {
				if s.unresolved == nil {
					s.unresolved = map[string]string{}
				}
				s.unresolved[p.Name] = why
				continue
			}
			s.subjects[p.Name] = sub
		case "enum":
			v := paramValue(act, actx, p)
			if v == "" {
				v = p.Default
			}
			if !contains(p.Values, v) {
				return nil, fmt.Errorf("%s: %s %q; want one of %s", name, p.Name, v, strings.Join(p.Values, ", "))
			}
			s.vals[p.Name] = v
		case "list":
			v := paramValue(act, actx, p)
			alias, isAlias := p.Aliases[v]
			if isAlias {
				i := strings.IndexByte(alias, '.')
				got, err := s.subjects[alias[:i]].Field(ctx, node, alias[i+1:])
				if err != nil {
					return nil, fmt.Errorf("%s: %s: %w", name, p.Name, err)
				}
				v = got
			}
			items := strings.Fields(v)
			re := regexp.MustCompile(p.ItemPattern)
			for _, it := range items {
				if !re.MatchString(it) {
					return nil, fmt.Errorf("%s: %s: item %q", name, p.Name, it)
				}
			}
			if len(items) == 0 && (!isAlias || p.NonEmpty) {
				return nil, fmt.Errorf("%s: %s are required (none given in %q)", name, p.Name, paramValue(act, actx, p))
			}
			s.vals[p.Name] = strings.Join(items, " ")
		default:
			v := paramValue(act, actx, p)
			if v == "" && (p.Required || p.Kind == "subject") {
				return nil, fmt.Errorf("%s: %s is required", name, p.Name)
			}
			if p.Pattern != "" && !regexp.MustCompile(p.Pattern).MatchString(v) {
				return nil, fmt.Errorf("%s: %s %q does not match %s", name, p.Name, v, p.Pattern)
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
		if err := Check(s.ctx, node, actx.History, false); err != nil {
			return nil, err
		}
		s.ctx = WithOp(s.ctx, OpRef{History: actx.History, Volume: vol})
		return x.observe(s, actx, a, vol)
	}
	if a.Kind == "device" || a.Kind == "process" || a.Kind == "sequence" {
		if err := x.runValues(s, actx, act); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		s.derived = a.Derived
		if a.Kind == "process" {
			s.procName = a.Process.Name
			return x.processStep(s, actx, act, a, expect)
		}
		if a.Kind == "sequence" {
			return x.sequence(s, actx, act, a, expect)
		}
		return x.device(s, actx, act, a, expect)
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
	if err := Check(s.ctx, node, actx.History, false); err != nil {
		return nil, err
	}
	attempt, driven := 0, false
	if a.Retry != nil {
		// One attempt of a declared rerun: the driver re-issues it
		// (Executor.Rerun), and each attempt is its own operation.
		attempt = 1
		if t := act.Params[AttemptParam]; t != "" {
			driven = true
			n, err := strconv.Atoi(t)
			if err != nil || n < 1 {
				return nil, fmt.Errorf("%s: %s %q is not an attempt number", name, AttemptParam, t)
			}
			attempt = n
		}
		args[AttemptParam] = attempt
	}
	opCtx := s.ctx
	seq, err := actx.History.Invoke(tr.HistoryRecord{Client: s.vals["config."+x.File.Config.Client], Op: a.Op, Verb: a.Verb, Volume: vol, Args: args})
	if err != nil {
		return nil, err
	}
	s.ctx = WithOp(opCtx, OpRef{History: actx.History, Seq: seq, Volume: vol})
	outcome, result, transport := x.once(s, a, v, cmd)
	if err := actx.History.Complete(seq, outcome, result); err != nil {
		return nil, err
	}
	tally(actx.History, a.Verb, result)
	if a.Retry == nil {
		actx.Log("  %s(%s): %s", name, vol, outcome)
		return map[string]string{"value": outcome}, nil
	}
	actx.Log("  %s(%s) attempt %d: %s", name, vol, attempt, outcome)
	out := map[string]string{"value": outcome}
	if driven {
		// What the driver matches against the declared rerun answers
		// (Rerun), so it never reads the history to decide.
		out["detail"], _ = result["detail"].(string)
		if transport {
			out["transport"] = "true"
		}
	}
	return out, nil
}

// AttemptParam is the step param that carries the attempt number of an
// action with a declared rerun; the driver sets it (absent: attempt 1, a
// call from no driver). A call that carries it answers, besides its value,
// the answer's detail and "transport" (the command could not be run or did
// not answer): what Rerun reads.
const AttemptParam = "attempt"

// RequestArg names the record arg that carries the request id every
// attempt of action name repeats; "" when name declares no rerun.
func (x *Executor) RequestArg(name string) string {
	if a := x.action(name); a != nil && a.Retry != nil {
		return a.Retry.RequestArg
	}
	return ""
}

// Rerun is action name's declared rerun, read by the DRIVER, which
// re-issues the action: whether attempt n+1 follows attempt n, which
// answered out (the handler's value, detail and transport) since the
// first attempt began, and after what pause. ONLY after an attempt that
// answered refused, with an answer the operator page names
// (Retry.matches): never after an unknown, even one whose detail matches
// (an unknown may have changed the subject, and the next attempt would
// have no "before"), never after a refused_effect, an ok or a transport
// failure. A step that declares expect=refused only when its
// expected_refusal param is true. The COUNT is the bound: at most
// max_attempts attempts. within_param (seconds) is only a guard, a wall
// clock past which no attempt follows (why "guard"). It reads nothing but
// the declaration and the step's params, and writes nothing.
func (x *Executor) Rerun(name string, params map[string]string, n int, since time.Duration, out map[string]string) (bool, time.Duration, string) {
	a := x.action(name)
	if a == nil || a.Retry == nil {
		return false, 0, ""
	}
	r := a.Retry
	if out["transport"] != "" || out["value"] != tr.OutcomeRefused || !r.matches(out["detail"]) {
		return false, 0, ""
	}
	if params["expect"] == tr.OutcomeRefused {
		if er := r.ExpectedRefusal; er == nil || !a.boolParam(er.Param, params) {
			return false, 0, ""
		}
	}
	if n >= r.MaxAttempts {
		return false, 0, "max_attempts"
	}
	if r.WithinParam != "" && since > time.Duration(a.intParam(r.WithinParam, params))*time.Second {
		return false, 0, "guard"
	}
	pause := time.Duration(r.EverySeconds) * time.Second
	if x.Pause != nil {
		pause = x.Pause(pause)
	}
	return true, pause, ""
}

// action is the declared action name, nil when there is none.
func (x *Executor) action(name string) *Action {
	for i := range x.File.Actions {
		if x.File.Actions[i].Action == name {
			return &x.File.Actions[i]
		}
	}
	return nil
}

// intParam is an int param's value as exec reads it: the step's, else its
// default.
func (a *Action) intParam(name string, params map[string]string) int {
	for _, p := range a.Params {
		if p.Name == name {
			def, _ := strconv.Atoi(p.Default)
			n, err := strconv.Atoi(params[name])
			if err != nil {
				return def
			}
			return n
		}
	}
	return 0
}

// boolParam is a bool param's value as exec reads it (exec refuses one
// that is not true or false before any attempt).
func (a *Action) boolParam(name string, params map[string]string) bool {
	for _, p := range a.Params {
		if p.Name == name {
			v := params[name]
			if v == "" {
				v = p.Default
			}
			b, _ := strconv.ParseBool(v)
			return b
		}
	}
	return false
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
		return tr.OutcomeUnknown, Abandon(s.ctx, t), true
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
