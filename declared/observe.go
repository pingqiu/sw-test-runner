package declared

// Declared observations. An observation (kind "observe") is a sequence of
// read-only steps, each a command whose answer a declared reader turns into
// result fields; the action writes ONE observe record with them and has no
// outcome. Which fields a product's observation has, and how each is read
// from an answer, is data:
//
//   - "json_object": stdout is a JSON object decoded against a typed schema
//     exactly as encoding/json decodes a struct of those fields (keys match
//     case-insensitively; a value of the wrong type makes the answer
//     unreadable). Fields maps it to result fields; Require names a path
//     that must hold a value; OnUnreadable, OnFailure (exit not 0) and
//     OnNotRun (the command could not run) give the fields otherwise.
//   - "lines": when the command ran, Into is {"exit": code, "text": the
//     first Lines lines of stdout}.
//   - "json_strings": stdout is a JSON list of strings, into Into; when the
//     command fails or the answer is not such a list, ErrorInto is Error.
//
// A step's When skips it; a derived value a step names that cannot be
// computed puts its error in the step's ErrorInto and the command does not
// run.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	tr "github.com/pingqiu/sw-test-runner"
)

// Step is one read of an observation.
type Step struct {
	Name                  string `json:"name"`
	When                  *Cond  `json:"when,omitempty"`
	Command               []Word `json:"command"`
	RunnerDeadlineSeconds int    `json:"runner_deadline_seconds"`
	Cite                  string `json:"cite"`
	Read                  Read   `json:"read"`
}

// Read turns one step's answer into result fields.
type Read struct {
	Kind string `json:"kind"`
	// json_object
	Schema       map[string]json.RawMessage `json:"schema,omitempty"`
	Require      string                     `json:"require,omitempty"`
	Fields       map[string]Out             `json:"fields,omitempty"`
	OnUnreadable map[string]Out             `json:"on_unreadable,omitempty"`
	OnFailure    map[string]Out             `json:"on_failure,omitempty"`
	OnNotRun     map[string]Out             `json:"on_not_run,omitempty"`
	// FailureBody is the schema of stdout on a failure ({{body.<field>}}),
	// decoded leniently.
	FailureBody map[string]json.RawMessage `json:"failure_body,omitempty"`
	// lines, json_strings
	Into      string `json:"into,omitempty"`
	Lines     int    `json:"lines,omitempty"`
	ErrorInto string `json:"error_into,omitempty"`
	Error     string `json:"error,omitempty"`
}

// Out is one result field: a template (a string); {"value": path}, the
// decoded value at path; {"int": template}; {"sorted_keys": path}, the
// sorted keys of the object at path ([] when it is null); or {"object":
// {...}} of such fields.
type Out struct {
	Template   *string
	Value      string
	Int        string
	SortedKeys string
	Object     map[string]Out
}

func (o *Out) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		o.Template = &s
		return nil
	}
	var x struct {
		Value      string         `json:"value"`
		Int        string         `json:"int"`
		SortedKeys string         `json:"sorted_keys"`
		Object     map[string]Out `json:"object"`
	}
	if err := strict(b, &x); err != nil {
		return err
	}
	n := 0
	for _, set := range []bool{x.Value != "", x.Int != "", x.SortedKeys != "", x.Object != nil} {
		if set {
			n++
		}
	}
	if n != 1 {
		return errors.New("a result field is a template, or exactly one of value, int, sorted_keys, object")
	}
	o.Value, o.Int, o.SortedKeys, o.Object = x.Value, x.Int, x.SortedKeys, x.Object
	return nil
}

// Derived is a value computed from a template, named {{derived.<name>}}:
// "port_offset" moves the port of a host:port (Error when the port is not
// above MinPort-1 after parsing), "trim_right" trims Chars from the right,
// "prefix" keeps the first Length bytes.
type Derived struct {
	Kind    string `json:"kind"`
	From    string `json:"from"`
	Offset  int    `json:"offset,omitempty"`
	MinPort int    `json:"min_port,omitempty"`
	Error   string `json:"error,omitempty"`
	Chars   string `json:"chars,omitempty"`
	Length  int    `json:"length,omitempty"`
}

func (d Derived) compute(s *scope) (string, error) {
	from, err := s.expand(d.From)
	if err != nil {
		return "", err
	}
	switch d.Kind {
	case "port_offset":
		host, port, ok := strings.Cut(from, ":")
		p, err := strconv.Atoi(port)
		if !ok || err != nil || p < d.MinPort {
			return "", errors.New(d.Error)
		}
		return fmt.Sprintf("%s:%d", host, p+d.Offset), nil
	case "trim_right":
		return strings.TrimRight(from, d.Chars), nil
	case "prefix":
		return from[:min(d.Length, len(from))], nil
	}
	return "", fmt.Errorf("derived kind %q", d.Kind)
}

// structType builds the Go struct type of a schema: "string", "int",
// "uint", "bool", "object" (any object: its keys), or a nested schema
// object (a pointer, so null and absent are nil).
func structType(schema map[string]json.RawMessage) (reflect.Type, error) {
	names := make([]string, 0, len(schema))
	for k := range schema {
		names = append(names, k)
	}
	sort.Strings(names)
	var fields []reflect.StructField
	for i, k := range names {
		var t reflect.Type
		var typ string
		if err := json.Unmarshal(schema[k], &typ); err == nil {
			switch typ {
			case "string":
				t = reflect.TypeOf("")
			case "int":
				t = reflect.TypeOf(int64(0))
			case "uint":
				t = reflect.TypeOf(uint64(0))
			case "bool":
				t = reflect.TypeOf(false)
			case "object":
				t = reflect.TypeOf(map[string]json.RawMessage{})
			default:
				return nil, fmt.Errorf("schema %s: type %q", k, typ)
			}
		} else {
			var sub map[string]json.RawMessage
			if err := json.Unmarshal(schema[k], &sub); err != nil {
				return nil, fmt.Errorf("schema %s: %v", k, err)
			}
			st, err := structType(sub)
			if err != nil {
				return nil, err
			}
			t = reflect.PointerTo(st)
		}
		fields = append(fields, reflect.StructField{Name: fmt.Sprintf("F%d", i), Type: t, Tag: reflect.StructTag(fmt.Sprintf(`json:%q`, k))})
	}
	return reflect.StructOf(fields), nil
}

// decodeSchema decodes b as encoding/json decodes the schema's struct and
// returns it as generic JSON (numbers kept exact); ok false when the
// decoding fails.
func decodeSchema(schema map[string]json.RawMessage, b string) (map[string]any, bool) {
	t, err := structType(schema)
	if err != nil {
		return nil, false
	}
	v := reflect.New(t)
	if json.Unmarshal([]byte(b), v.Interface()) != nil {
		return nil, false
	}
	out, _ := json.Marshal(v.Interface())
	d := json.NewDecoder(bytes.NewReader(out))
	d.UseNumber()
	var m map[string]any
	if d.Decode(&m) != nil {
		return nil, false
	}
	return m, true
}

func lookupPath(m map[string]any, path string) any {
	var cur any = m
	for _, p := range strings.Split(path, ".") {
		o, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = o[p]
	}
	return cur
}

func (s *scope) out(o Out, data map[string]any) (any, error) {
	switch {
	case o.Template != nil:
		return s.expand(*o.Template)
	case o.Value != "":
		return lookupPath(data, o.Value), nil
	case o.Int != "":
		v, err := s.expand(o.Int)
		if err != nil {
			return nil, err
		}
		return strconv.Atoi(v)
	case o.SortedKeys != "":
		obj, _ := lookupPath(data, o.SortedKeys).(map[string]any)
		keys := make([]string, 0, len(obj))
		for k := range obj {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return keys, nil
	}
	m := map[string]any{}
	for k, sub := range o.Object {
		v, err := s.out(sub, data)
		if err != nil {
			return nil, err
		}
		m[k] = v
	}
	return m, nil
}

func (s *scope) fill(res map[string]any, outs map[string]Out, data map[string]any) error {
	for k, o := range outs {
		v, err := s.out(o, data)
		if err != nil {
			return err
		}
		res[k] = v
	}
	return nil
}

// observe runs an observation's steps and writes its observe record.
func (x *Executor) observe(s *scope, actx *tr.ActionContext, a *Action, vol string) (map[string]string, error) {
	res := map[string]any{}
	var abandoned []Proc
	for i := range a.Steps {
		st := &a.Steps[i]
		if st.When != nil {
			hold, err := s.holds(st.When)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", a.Action, err)
			}
			if !hold {
				continue
			}
		}
		cmd, err := s.command(st.Command)
		if err != nil {
			var d derivedError
			if errors.As(err, &d) && st.Read.ErrorInto != "" {
				res[st.Read.ErrorInto] = d.msg
				continue
			}
			return nil, fmt.Errorf("%s: %w", a.Action, err)
		}
		deadline := time.Duration(st.RunnerDeadlineSeconds) * time.Second
		if x.Deadline != nil {
			deadline = x.Deadline(deadline)
		}
		dir, err := s.expand(x.File.RunnerIODir)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", a.Action, err)
		}
		poll := x.Poll
		if poll <= 0 {
			poll = 200 * time.Millisecond
		}
		stdout, stderr, code, runErr := RunDetached(s.ctx, s.node, a.AsRoot, dir, cmd, deadline, poll)
		if t, ok := AsDetached(runErr); ok {
			abandoned = append(abandoned, t.Abandoned...)
		}
		if err := s.read(res, st.Read, stdout, stderr, code, runErr); err != nil {
			return nil, fmt.Errorf("%s: step %s: %w", a.Action, st.Name, err)
		}
	}
	if len(abandoned) > 0 {
		res["abandoned"] = abandoned
	}
	return map[string]string{"value": "observed"}, actx.History.Observe(tr.HistoryObserve,
		tr.HistoryRecord{Client: s.vals["config."+x.File.Config.Client], Volume: vol, Verb: a.Verb, Result: res})
}

func (s *scope) read(res map[string]any, r Read, stdout, stderr string, code int, runErr error) error {
	s.vals["exit"] = strconv.Itoa(code)
	s.vals["stderr"] = stderr
	s.vals["stderr_first_line"] = FirstLine(stderr)
	s.vals["error"] = ""
	if runErr != nil {
		s.vals["error"] = runErr.Error()
	}
	switch r.Kind {
	case "json_object":
		if runErr != nil || code != 0 {
			body, _ := decodeSchemaLenient(r.FailureBody, stdout)
			for k, v := range body {
				s.vals["body."+k] = fmt.Sprint(v)
			}
			outs := r.OnFailure
			if runErr != nil {
				outs = r.OnNotRun
			}
			return s.fill(res, outs, body)
		}
		data, ok := decodeSchema(r.Schema, strings.TrimSpace(stdout))
		if !ok || (r.Require != "" && lookupPath(data, r.Require) == nil) {
			return s.fill(res, r.OnUnreadable, nil)
		}
		return s.fill(res, r.Fields, data)
	case "lines":
		if runErr == nil {
			lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
			if len(lines) > r.Lines {
				lines = lines[:r.Lines]
			}
			res[r.Into] = map[string]any{"exit": code, "text": lines}
		}
		return nil
	case "json_strings":
		var list []string
		if runErr != nil || code != 0 || json.Unmarshal([]byte(strings.TrimSpace(stdout)), &list) != nil {
			msg, err := s.expand(r.Error)
			if err != nil {
				return err
			}
			res[r.ErrorInto] = msg
			return nil
		}
		res[r.Into] = list
		return nil
	}
	return fmt.Errorf("read kind %q", r.Kind)
}

// decodeSchemaLenient decodes what it can: a failed decoding leaves the
// fields it did not reach at their zero values (as encoding/json does).
func decodeSchemaLenient(schema map[string]json.RawMessage, b string) (map[string]any, bool) {
	t, err := structType(schema)
	if err != nil {
		return map[string]any{}, false
	}
	v := reflect.New(t)
	err = json.Unmarshal([]byte(strings.TrimSpace(b)), v.Interface())
	out, _ := json.Marshal(v.Interface())
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	return m, err == nil
}

// derivedError is a derived value that cannot be computed.
type derivedError struct{ msg string }

func (d derivedError) Error() string { return d.msg }

func (s *scope) holds(c *Cond) (bool, error) {
	got, err := s.get(c.Field)
	if err != nil {
		return false, err
	}
	if c.NotEquals != nil {
		return got != *c.NotEquals, nil
	}
	return got == c.Equals, nil
}

// checkObserve checks an observation's steps.
func (a *Action) checkObserve(defined func(map[string]bool) func(string) error) error {
	if len(a.Steps) == 0 || len(a.Variants) != 0 || a.Retry != nil || len(a.OnOKSet) != 0 || len(a.Record.Args) != 0 {
		return errors.New("an observation has steps only (no variants, retry, on_ok_set or args)")
	}
	base := defined(nil)
	for k, d := range a.Derived {
		switch d.Kind {
		case "port_offset", "trim_right", "prefix":
		default:
			return fmt.Errorf("derived %s: kind %q", k, d.Kind)
		}
		if err := base(d.From); err != nil {
			return fmt.Errorf("derived %s: %w", k, err)
		}
	}
	readNames := map[string]bool{"exit": true, "stderr": true, "stderr_first_line": true, "error": true}
	for i, st := range a.Steps {
		at := fmt.Sprintf("step %d (%s)", i, st.Name)
		if len(st.Command) == 0 || strings.TrimSpace(st.Cite) == "" || st.RunnerDeadlineSeconds <= 0 {
			return fmt.Errorf("%s: command, cite and a positive runner_deadline_seconds are required", at)
		}
		if st.When != nil {
			if err := base("{{" + st.When.Field + "}}"); err != nil {
				return fmt.Errorf("%s: %w", at, err)
			}
		}
		for _, w := range st.Command {
			for _, t := range append([]string{w.Text}, mapValues(w.JSON)...) {
				if err := base(t); err != nil {
					return fmt.Errorf("%s: %w", at, err)
				}
			}
		}
		r := st.Read
		names := map[string]bool{}
		for k := range readNames {
			names[k] = true
		}
		for k := range r.FailureBody {
			names["body."+k] = true
		}
		check := defined(names)
		var outs func(map[string]Out) error
		outs = func(m map[string]Out) error {
			for _, o := range m {
				for _, t := range []string{deref(o.Template), o.Int} {
					if err := check(t); err != nil {
						return err
					}
				}
				if err := outs(o.Object); err != nil {
					return err
				}
			}
			return nil
		}
		switch r.Kind {
		case "json_object":
			if _, err := structType(r.Schema); err != nil || len(r.Fields) == 0 {
				return fmt.Errorf("%s: json_object needs a schema and fields: %v", at, err)
			}
			if _, err := structType(r.FailureBody); err != nil {
				return fmt.Errorf("%s: failure_body: %w", at, err)
			}
			for _, m := range []map[string]Out{r.Fields, r.OnUnreadable, r.OnFailure, r.OnNotRun} {
				if err := outs(m); err != nil {
					return fmt.Errorf("%s: %w", at, err)
				}
			}
		case "lines":
			if r.Into == "" || r.Lines <= 0 {
				return fmt.Errorf("%s: lines needs into and lines", at)
			}
		case "json_strings":
			if r.Into == "" || r.ErrorInto == "" {
				return fmt.Errorf("%s: json_strings needs into and error_into", at)
			}
			if err := check(r.Error); err != nil {
				return fmt.Errorf("%s: %w", at, err)
			}
		default:
			return fmt.Errorf("%s: read kind %q", at, r.Kind)
		}
	}
	return nil
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

var _ = context.Background
