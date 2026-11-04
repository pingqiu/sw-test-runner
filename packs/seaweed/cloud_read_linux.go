//go:build linux

package seaweed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	tr "github.com/pingqiu/sw-test-runner"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Reuse the profile's jobs and direct executor, without an approved-run adapter.
func (s *directSoak) readJobs() error {
	role := strings.Split(s.c.ReadCase, "-")[0]
	victim := ""
	for _, step := range s.b.Plan.Deploy {
		if step.Role == role && step.Host == "m02" {
			victim = step.Job
		}
	}
	if victim == "" {
		return errors.New("read_case victim missing")
	}
	args, err := ProfileArgs(s.d, "m02", "deploy", role, s.b.Plan.Smoke, s.in.Files["m02"])
	if err != nil {
		return err
	}
	j := s.b.Jobs[victim]
	j.Argv = append(args, "--replacement-"+role)
	s.b.Jobs["read-g2"] = j
	s.b.Restarts["read-case"] = tr.GenerationRestartPlan{Victim: victim, Replacement: "read-g2"}
	for _, v := range []struct{ label, stage, route string }{{"read-seed", "seed", "dc"}, {"read-load", "load", "dc"}, {"read-dc", "recovery", "dc"}, {"read-http", "recovery", "http"}} {
		args, err := ReadLoadArgs(s.d, "m01", s.b.Plan.Smoke, v.stage, v.route)
		if err != nil {
			return err
		}
		s.b.Jobs[v.label] = tr.JobPin{Node: "m01", Executable: "/usr/bin/python3.12", SHA256: s.in.Files["m01"]["/usr/bin/python3.12"], Argv: args}
	}
	s.b.Jobs["read-http-before"] = s.b.Jobs["read-http"]
	for _, step := range s.b.Plan.Ready {
		s.b.Jobs["read-ready-"+step.Host] = s.b.Jobs[step.Job]
	}
	return nil
}

func (s *directSoak) readReady(ctx context.Context, want tr.ProcessIdentity) error {
	role := strings.Split(s.c.ReadCase, "-")[0]
	found := false
	for _, step := range s.b.Plan.Ready {
		e, err := s.step(ctx, "read-ready-"+step.Host, 60)
		if err != nil {
			return err
		}
		raw, _, err := e.Output.Decode()
		if err != nil {
			return err
		}
		var row struct {
			Status    string
			Processes map[string]struct {
				Identity tr.ProcessIdentity `json:"scope_identity"`
			}
		}
		if err = json.Unmarshal(raw, &row); err != nil || row.Status != "READY" {
			return errors.New("read_case not READY")
		}
		if step.Host == "m02" {
			id := row.Processes[role].Identity
			if id.PID != want.PID || id.StartTime != want.StartTime || id.BootID != want.BootID || id.ExecutableSHA256 != want.ExecutableSHA256 {
				return errors.New("read_case readiness generation differs")
			}
			found = true
		}
	}
	if !found {
		return errors.New("read_case server readiness missing")
	}
	return nil
}

// Fault dispatches own their connection; a transport failure leaves STOP uncertain.
func (s *directSoak) readSignal(ctx context.Context, name, command string) (string, error) {
	n := s.c.node(s.hosts["m02"])
	defer n.Close()
	if err := n.ConnectContext(ctx); err != nil {
		return "", err
	}
	return cloudStep(ctx, n, s.c.Output, name+".json", name, 20, command)
}

// Resume is registered before STOP. A joined uncertain CONT is sticky: never
// send a second signal or turn a later LIVE observation into a successful run.
func (s *directSoak) readFreeze(ctx context.Context, victim string) (w freezeWindow, failure error) {
	id, err := s.identity(ctx, "m02", victim)
	if err != nil {
		return w, err
	}
	attempted, stopped := false, false
	resume := func(cx context.Context) error {
		attempted = true
		cmd := "test \"$(cat /proc/sys/kernel/random/boot_id)\" = " + cloudQuote(id.BootID) + "; " + chaosContScript(strconv.Itoa(id.PID), strconv.FormatUint(id.StartTime, 10))

		out, err := s.readSignal(cx, "read-cont", cmd)
		if err == nil && strings.TrimSpace(out) != "LIVE" {
			err = errors.New("read_case CONT not LIVE")
		}
		if err != nil {
			return fmt.Errorf("read_case CONT UNKNOWN: %w", err)
		}
		return nil
	}
	defer func() {
		if stopped && !attempted {
			hard, _ := time.Parse(time.RFC3339Nano, s.d.Deadlines.Hard)
			cleanup, cancel := context.WithDeadline(context.WithoutCancel(ctx), hard)
			defer cancel()
			cleanup, cancel = context.WithTimeout(cleanup, 25*time.Second)
			defer cancel()
			failure = errors.Join(failure, resume(cleanup))
		}
	}()
	if err = ctx.Err(); err != nil {
		return w, err
	}
	w.PreStop, err = controllerMono()
	if err != nil {
		return w, err
	}
	prefix := mainPID(s.unit(victim))
	cmd := prefix + fmt.Sprintf("test \"$pid:$start\" = '%d:%d'; ", id.PID, id.StartTime) + strings.TrimPrefix(chaosStopScript(s.unit(victim)), prefix)
	// Only a successful STOP reply permits CONT; otherwise owned teardown kills it.
	stop, cancel := context.WithDeadline(context.WithoutCancel(ctx), s.deadline)
	defer cancel()
	out, err := s.readSignal(stop, "read-stop", cmd)
	if err != nil || !strings.HasSuffix(out, "STOPPED\n") {
		return w, errors.Join(err, errors.New("read_case STOP UNKNOWN: not witnessed"))
	}
	stopped = true
	w.Stopped, err = controllerMono()
	if err != nil {
		return w, err
	}
	select {
	case <-ctx.Done():
		return w, ctx.Err()
	case <-time.After(chaosHold):
	}
	if err = ctx.Err(); err != nil {
		return w, err
	}
	w.PreCont, err = controllerMono()
	if err != nil {
		return w, err
	}
	cont, cancel := context.WithDeadline(context.WithoutCancel(ctx), s.deadline)
	defer cancel()
	if err = resume(cont); err != nil {
		return w, err
	}
	if err = ctx.Err(); err != nil {
		return w, err
	}
	if err = s.readReady(ctx, id); err != nil {
		return w, err
	}
	w.Resumed, err = controllerMono()
	return w, err
}

func (s *directSoak) readCase(ctx context.Context) (string, error) {
	var c SmokeConfig
	if err := json.Unmarshal([]byte(s.b.Jobs["read-load"].Argv[3]), &c); err != nil {
		return "", err
	}
	observed := map[string]any{"case": s.c.ReadCase, "scope": "case-only"}
	read := func(label, stage, route string) error {
		e, err := s.step(ctx, label, 150)
		if err != nil {
			return err
		}
		row, err := CheckReadTaskOutput(e, c, stage, route)
		observed[label] = row
		return err
	}
	if err := read("read-seed", "seed", "dc"); err != nil {
		return "", err
	}
	freeze := s.c.ReadCase == "volume-freeze"
	if !freeze {
		if err := read("read-http-before", "recovery", "http"); err != nil {
			return "", err
		}
	}
	h, err := s.StartTask(ctx, "read-load")
	if err != nil {
		return "", err
	}
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-time.After(chaosSettle):
	}
	var pre float64
	var window freezeWindow
	if freeze {
		window, err = s.readFreeze(ctx, s.b.Restarts["read-case"].Victim)
		pre = window.PreStop
		observed["freeze"] = window
	} else {
		var receipt tr.GenerationRestartReceipt
		_, receipt, err = s.Restart(ctx, "read-case", tr.JobHandle{})
		pre = receipt.Kill.PreKillMono
		observed["restart"] = receipt
		old, next := receipt.Kill.Identity, receipt.Replacement
		if err == nil && (old.PID == next.PID && old.StartTime == next.StartTime || old.BootID != next.BootID || old.ExecutableSHA256 != next.ExecutableSHA256) {
			err = errors.New("read_case replacement identity differs")
		}
		if err == nil {
			err = s.readReady(ctx, next)
		}
	}
	if err != nil {
		return "", err
	}
	e, err := s.WaitTask(ctx, h)
	if err != nil {
		return "", err
	}
	row, err := CheckReadTaskOutput(e, c, "load", "dc", ReadWindow{pre})
	observed["read-load"] = row
	if err == nil && freeze {
		err = checkFreezeExposure(row, window)
	}
	if err != nil {
		return "", err
	}
	if err = read("read-dc", "recovery", "dc"); err != nil {
		return "", err
	}
	if !freeze {
		if err = read("read-http", "recovery", "http"); err != nil {
			return "", err
		}
	}
	observed["status"] = "PASS"
	body, err := json.MarshalIndent(observed, "", "  ")
	out := filepath.Join(s.c.Output, "read-case.json")
	if err == nil {
		err = os.WriteFile(out, body, 0600)
	}
	return out, err
}
