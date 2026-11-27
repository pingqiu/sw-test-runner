package declared

// Device resolvers. How a guest finds, connects and disconnects the block
// device of a subject is the product's to declare (File DeviceResolvers,
// by name); the executor knows a resolver, not a protocol. A resolver has
// three declared commands, each a template over {{subsystem}} (and, for the
// connect, {{address}}, {{host}} and {{port}} split at its first ':', and
// {{options}}):
//
//   - scan: one look at the subsystem's devices. A complete scan's stdout
//     ends with End; every other line matches one of Lines (kind ->
//     pattern), counted by kind. The subsystem is absent when every count
//     of Absent holds, has exactly one device when every count of One holds
//     (the device is Device, a template over the groups of the one line of
//     DeviceLine: {{<kind>.<n>}}), and is incomplete otherwise. A scan that
//     fails, does not end with End, or prints a line no pattern matches is
//     unreadable: nothing may be concluded from it.
//   - connect: Require rows checked in order (each value must match its
//     pattern, or the connect fails with that row's failure and nothing
//     runs), then the command; exit 0 connected.
//   - disconnect: the command, exit 0, its count of devices removed read
//     from stdout by CountPattern (-1 when not read); the disconnect is
//     certified only by a scan that then finds the subsystem absent within
//     ConfirmWithinSeconds.
//
// A resolver may also declare a fourth command:
//
//   - readback: one look at named values of the subsystem's unit (its
//     controller, say) after a connect, a template over {{subsystem}} and
//     {{files}} (the names, space separated, one word). A line matching
//     UnitLine (one group) names a unit, counted; a line matching ValueLine
//     (two groups) gives a name's value, NotShown when the unit has none.
//     Other lines are ignored. A failing command is Unreadable; a count of
//     units other than one is NotOne ({{count}}), the values read kept.
//     TextValues gives the integer some value texts stand for.
//
// Every failure text is the resolver's ({{exit}}, {{stderr_first_line}},
// {{line}}, {{why}}, {{count.<kind>}}, {{count}}, {{subsystem}}, {{options}}).

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// What a scan says about one subsystem.
const (
	DeviceAbsent     = "absent"     // a complete scan found no device of it
	DeviceOne        = "one"        // exactly one device: its path
	DeviceIncomplete = "incomplete" // something, but not exactly one device (still coming up, or not ours)
	DeviceUnreadable = "unreadable" // the scan failed: nothing may be concluded
)

// Resolver is one declared device resolver.
type Resolver struct {
	Cite       string             `json:"cite"`
	Scan       ResolverScan       `json:"scan"`
	Connect    ResolverConnect    `json:"connect"`
	Disconnect ResolverDisconnect `json:"disconnect"`
	Readback   *ResolverReadback  `json:"readback,omitempty"`
}

// ResolverReadback is a resolver's read of a unit's named values.
type ResolverReadback struct {
	Command    []Word         `json:"command"`
	AsRoot     bool           `json:"as_root,omitempty"`
	UnitLine   string         `json:"unit_line"`
	ValueLine  string         `json:"value_line"`
	NotShown   string         `json:"not_shown"`
	TextValues map[string]int `json:"text_values,omitempty"`
	Unreadable string         `json:"unreadable"`
	NotOne     string         `json:"not_one"`
}

// ResolverScan is a resolver's look at a subsystem.
type ResolverScan struct {
	Command    []Word            `json:"command"`
	AsRoot     bool              `json:"as_root,omitempty"`
	End        string            `json:"end"`
	Lines      map[string]string `json:"lines"`
	Absent     map[string]int    `json:"absent"`
	One        map[string]int    `json:"one"`
	DeviceLine string            `json:"device_line"`
	Device     string            `json:"device"`
	Unreadable string            `json:"unreadable"`
	BadLine    string            `json:"bad_line"`
	Incomplete string            `json:"incomplete"`
	// The lookup's texts when its bound passes with no device, absent or
	// not, and when the runner deadline ends it.
	NoDevice           string `json:"no_device"`
	NoDeviceByDeadline string `json:"no_device_by_deadline"`
	EveryMs            int    `json:"every_ms"`
}

// ResolverConnect is a resolver's connect.
type ResolverConnect struct {
	Command []Word    `json:"command"`
	AsRoot  bool      `json:"as_root,omitempty"`
	Require []Require `json:"require,omitempty"`
	Failure string    `json:"failure"`
}

// Require is one value that must match its pattern.
type Require struct {
	Value   string `json:"value"`
	Pattern string `json:"pattern"`
	Failure string `json:"failure"`
}

// ResolverDisconnect is a resolver's disconnect.
type ResolverDisconnect struct {
	Command              []Word `json:"command"`
	AsRoot               bool   `json:"as_root,omitempty"`
	CountPattern         string `json:"count_pattern,omitempty"`
	Failure              string `json:"failure"`
	ConfirmWithinSeconds int    `json:"confirm_within_seconds"`
	ConfirmEveryMs       int    `json:"confirm_every_ms"`
	NotConfirmed         string `json:"not_confirmed"`
	Remains              string `json:"remains"`
	RemainsByDeadline    string `json:"remains_by_deadline"`
}

// check checks one resolver.
func (r *Resolver) check() error {
	if strings.TrimSpace(r.Cite) == "" {
		return errors.New("cite is required")
	}
	sc := r.Scan
	if len(sc.Command) == 0 || sc.End == "" || len(sc.Lines) == 0 || len(sc.One) == 0 || len(sc.Absent) == 0 || sc.Device == "" || sc.EveryMs <= 0 {
		return errors.New("scan: command, end, lines, absent, one, device and every_ms")
	}
	for k, p := range sc.Lines {
		if _, err := regexp.Compile(p); err != nil {
			return fmt.Errorf("scan: line %s: %w", k, err)
		}
	}
	for _, m := range []map[string]int{sc.Absent, sc.One} {
		for k := range m {
			if _, ok := sc.Lines[k]; !ok {
				return fmt.Errorf("scan: a count of %q, not a line kind", k)
			}
		}
	}
	if _, ok := sc.Lines[sc.DeviceLine]; !ok || sc.One[sc.DeviceLine] != 1 {
		return errors.New("scan: device_line is a kind of which exactly one makes the device")
	}
	for _, t := range []string{sc.Unreadable, sc.BadLine, sc.Incomplete, sc.NoDevice, sc.NoDeviceByDeadline} {
		if t == "" {
			return errors.New("scan: every failure text is declared")
		}
	}
	c := r.Connect
	if len(c.Command) == 0 || c.Failure == "" {
		return errors.New("connect: command and failure")
	}
	for _, q := range c.Require {
		if _, err := regexp.Compile(q.Pattern); err != nil || q.Value == "" || q.Failure == "" {
			return fmt.Errorf("connect: require needs a value, a pattern and a failure: %v", err)
		}
	}
	d := r.Disconnect
	if len(d.Command) == 0 || d.Failure == "" || d.ConfirmWithinSeconds <= 0 || d.ConfirmEveryMs <= 0 || d.NotConfirmed == "" || d.Remains == "" || d.RemainsByDeadline == "" {
		return errors.New("disconnect: command, failure, confirm_within_seconds, confirm_every_ms and its texts")
	}
	if d.CountPattern != "" {
		if re, err := regexp.Compile(d.CountPattern); err != nil || re.NumSubexp() != 1 {
			return errors.New("disconnect: count_pattern has one group")
		}
	}
	if rb := r.Readback; rb != nil {
		u, eu := regexp.Compile(rb.UnitLine)
		v, ev := regexp.Compile(rb.ValueLine)
		if len(rb.Command) == 0 || eu != nil || ev != nil || u.NumSubexp() != 1 || v.NumSubexp() != 2 || rb.NotShown == "" || rb.Unreadable == "" || rb.NotOne == "" {
			return errors.New("readback: command, unit_line (one group), value_line (two groups), not_shown and its texts")
		}
	}
	return nil
}

// resolverScope is a scope for one resolver command.
func resolverScope(ctx context.Context, node Runner, vals map[string]string) *scope {
	return &scope{ctx: ctx, node: node, vals: vals, subjects: map[string]Subject{}}
}

func (x *Executor) resolver(name string) (*Resolver, error) {
	r, ok := x.File.DeviceResolvers[name]
	if !ok {
		return nil, fmt.Errorf("no device resolver %q", name)
	}
	return r, nil
}

// DeviceScan is one scan of subsystem by the resolver name: its state, the
// device when there is exactly one, or why not.
func (x *Executor) DeviceScan(ctx context.Context, node Runner, name, subsystem string) (state, dev, why string) {
	r, err := x.resolver(name)
	if err != nil {
		return DeviceUnreadable, "", err.Error()
	}
	sc := r.Scan
	s := resolverScope(ctx, node, map[string]string{"subsystem": subsystem})
	cmd, err := s.command(sc.Command)
	if err != nil {
		return DeviceUnreadable, "", err.Error()
	}
	run := node.Run
	if sc.AsRoot {
		run = node.RunRoot
	}
	stdout, stderr, code, err := Call(ctx, run, cmd)
	s.vals["exit"], s.vals["stderr_first_line"] = strconv.Itoa(code), FirstLine(stderr)
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if err != nil || code != 0 || len(lines) == 0 || lines[len(lines)-1] != sc.End {
		msg, _ := s.expand(sc.Unreadable)
		return DeviceUnreadable, "", msg
	}
	count := map[string]int{}
	var deviceGroups []string
	for _, l := range lines[:len(lines)-1] {
		matched := false
		for _, kind := range sortedKeys(sc.Lines) {
			if m := regexp.MustCompile(sc.Lines[kind]).FindStringSubmatch(l); m != nil {
				count[kind]++
				if kind == sc.DeviceLine {
					deviceGroups = m
				}
				matched = true
				break
			}
		}
		if !matched {
			s.vals["line"] = FirstLine(l)
			msg, _ := s.expand(sc.BadLine)
			return DeviceUnreadable, "", msg
		}
	}
	holds := func(m map[string]int) bool {
		for k, n := range m {
			if count[k] != n {
				return false
			}
		}
		return true
	}
	for k := range sc.Lines {
		s.vals["count."+k] = strconv.Itoa(count[k])
	}
	switch {
	case holds(sc.Absent):
		return DeviceAbsent, "", ""
	case holds(sc.One):
		for i, g := range deviceGroups {
			s.vals[sc.DeviceLine+"."+strconv.Itoa(i)] = g
		}
		dev, err := s.expand(sc.Device)
		if err != nil {
			return DeviceUnreadable, "", err.Error()
		}
		return DeviceOne, dev, ""
	}
	msg, _ := s.expand(sc.Incomplete)
	return DeviceIncomplete, "", msg
}

// DeviceOf waits up to bound until the subsystem has exactly one device and
// returns it, or why not: an unreadable scan ends the wait at once.
func (x *Executor) DeviceOf(ctx context.Context, node Runner, name, subsystem string, bound time.Duration) (string, string) {
	r, err := x.resolver(name)
	if err != nil {
		return "", err.Error()
	}
	s := resolverScope(ctx, node, map[string]string{"subsystem": subsystem})
	end := time.Now().Add(bound)
	for {
		state, dev, why := x.DeviceScan(ctx, node, name, subsystem)
		if state == DeviceOne {
			return dev, ""
		}
		if state == DeviceUnreadable {
			return "", why
		}
		if time.Now().After(end) {
			if state == DeviceAbsent {
				msg, _ := s.expand(r.Scan.NoDevice)
				return "", msg
			}
			return "", why
		}
		if !Pause(ctx, time.Duration(r.Scan.EveryMs)*time.Millisecond) {
			msg, _ := s.expand(r.Scan.NoDeviceByDeadline)
			return "", msg
		}
	}
}

// DeviceConnect connects the subsystem at address with options: "" or why
// not.
func (x *Executor) DeviceConnect(ctx context.Context, node Runner, name, address, subsystem, options string) string {
	r, err := x.resolver(name)
	if err != nil {
		return err.Error()
	}
	c := r.Connect
	host, port, _ := strings.Cut(address, ":")
	s := resolverScope(ctx, node, map[string]string{"subsystem": subsystem, "address": address, "host": host, "port": port, "options": options})
	for _, q := range c.Require {
		v, err := s.expand(q.Value)
		if err != nil {
			return err.Error()
		}
		if !regexp.MustCompile(q.Pattern).MatchString(v) {
			msg, _ := s.expand(q.Failure)
			return msg
		}
	}
	cmd, err := s.command(c.Command)
	if err != nil {
		return err.Error()
	}
	run := node.Run
	if c.AsRoot {
		run = node.RunRoot
	}
	_, stderr, code, err := Call(ctx, run, cmd)
	if err != nil || code != 0 {
		s.vals["exit"], s.vals["stderr_first_line"] = strconv.Itoa(code), FirstLine(stderr)
		msg, _ := s.expand(c.Failure)
		return msg
	}
	return ""
}

// DeviceDisconnect removes the subsystem's devices: how many it removed (-1
// when not known), and "" only when a scan then finds the subsystem absent.
func (x *Executor) DeviceDisconnect(ctx context.Context, node Runner, name, subsystem string) (int, string) {
	r, err := x.resolver(name)
	if err != nil {
		return -1, err.Error()
	}
	d := r.Disconnect
	s := resolverScope(ctx, node, map[string]string{"subsystem": subsystem})
	cmd, err := s.command(d.Command)
	if err != nil {
		return -1, err.Error()
	}
	run := node.Run
	if d.AsRoot {
		run = node.RunRoot
	}
	stdout, stderr, code, err := Call(ctx, run, cmd)
	if err != nil || code != 0 {
		s.vals["exit"], s.vals["stderr_first_line"] = strconv.Itoa(code), FirstLine(stderr)
		msg, _ := s.expand(d.Failure)
		return -1, msg
	}
	n := -1
	if d.CountPattern != "" {
		if m := regexp.MustCompile(d.CountPattern).FindStringSubmatch(strings.TrimSpace(stdout)); m != nil {
			n, _ = strconv.Atoi(m[1])
		}
	}
	end := time.Now().Add(time.Duration(d.ConfirmWithinSeconds) * time.Second)
	for {
		state, _, why := x.DeviceScan(ctx, node, name, subsystem)
		switch {
		case state == DeviceAbsent:
			return n, ""
		case state == DeviceUnreadable:
			s.vals["why"] = why
			msg, _ := s.expand(d.NotConfirmed)
			return n, msg
		case time.Now().After(end):
			msg, _ := s.expand(d.Remains)
			return n, msg
		}
		if !Pause(ctx, time.Duration(d.ConfirmEveryMs)*time.Millisecond) {
			msg, _ := s.expand(d.RemainsByDeadline)
			return n, msg
		}
	}
}

// DeviceReadback reads the values of files of the subsystem's one unit by
// the resolver name: the unit (the last one named), the values shown by
// file, and why not one unit, or why unreadable.
func (x *Executor) DeviceReadback(ctx context.Context, node Runner, name, subsystem string, files []string) (string, map[string]string, string) {
	r, err := x.resolver(name)
	if err != nil {
		return "", nil, err.Error()
	}
	rb := r.Readback
	if rb == nil {
		return "", nil, fmt.Errorf("device resolver %q declares no readback", name).Error()
	}
	s := resolverScope(ctx, node, map[string]string{"subsystem": subsystem, "files": strings.Join(files, " ")})
	cmd, err := s.command(rb.Command)
	if err != nil {
		return "", nil, err.Error()
	}
	run := node.Run
	if rb.AsRoot {
		run = node.RunRoot
	}
	stdout, stderr, code, err := Call(ctx, run, cmd)
	if err != nil || code != 0 {
		s.vals["exit"], s.vals["stderr_first_line"] = strconv.Itoa(code), FirstLine(stderr)
		msg, _ := s.expand(rb.Unreadable)
		return "", map[string]string{}, msg
	}
	unitRe, valueRe := regexp.MustCompile(rb.UnitLine), regexp.MustCompile(rb.ValueLine)
	unit, units, byFile := "", 0, map[string]string{}
	for _, l := range strings.Split(strings.TrimSpace(stdout), "\n") {
		if m := unitRe.FindStringSubmatch(l); m != nil {
			unit, units = m[1], units+1
		} else if m := valueRe.FindStringSubmatch(l); m != nil && m[2] != rb.NotShown {
			byFile[m[1]] = m[2]
		}
	}
	if units != 1 {
		s.vals["count"] = strconv.Itoa(units)
		msg, _ := s.expand(rb.NotOne)
		return unit, byFile, msg
	}
	return unit, byFile, ""
}

// checkReadbacks refuses a device_readback step whose resolver declares no
// readback.
func (f *File) checkReadbacks() error {
	for _, a := range f.Actions {
		if p := a.Process; p != nil {
			for _, ps := range [][]Probe{p.Before, p.Owned, p.After} {
				for _, pr := range ps {
					if r := f.DeviceResolvers[pr.Resolver]; pr.Kind == "device_readback" && r != nil && r.Readback == nil {
						return fmt.Errorf("actions: %s: step %s: device resolver %q declares no readback", a.Action, pr.Name, pr.Resolver)
					}
				}
			}
		}
	}
	return nil
}

// resolverRefs lists every device resolver an action names.
func (f *File) resolverRefs() []string {
	var out []string
	for _, a := range f.Actions {
		if a.Device != nil {
			out = append(out, a.Device.Resolver)
		}
		if p := a.Process; p != nil {
			for _, ps := range [][]Probe{p.Before, p.Owned, p.After} {
				for _, pr := range ps {
					if pr.Resolver != "" {
						out = append(out, pr.Resolver)
					}
				}
			}
		}
		if q := a.Sequence; q != nil {
			for _, st := range q.Steps {
				if st.Resolver != "" {
					out = append(out, st.Resolver)
				}
			}
		}
	}
	return out
}
