//go:build linux

package seaweed

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/infra"
	"github.com/pingqiu/sw-test-runner/inventory"
)

// directSoak runs the KV soak on the direct entry: the existing profile
// bundle's jobs, run as transient units and bounded steps on M01 (locally,
// where the controller and its clock live) and M02 (over plain ssh). The
// fault schedule, the fault-window files and the verdict are the approved
// path's own code (runSoak, soakWindow, the assessor); only execution moves.
type directSoak struct {
	soakWindow
	c        CloudConfig
	d        *inventory.Document
	b        ProfileBundle
	in       ProfileInput
	deadline time.Time
	load     string // the one task's job label
}

func (s *directSoak) unit(label string) string { return "sw-cloud-" + s.d.RunID + "-" + label }

func (s *directSoak) host(id string) inventory.Host {
	for _, h := range s.d.Hosts {
		if h.ID == id {
			return h
		}
	}
	return inventory.Host{}
}

func (s *directSoak) node(id string) *infra.Node { return s.c.node(s.host(id)) }

func (s *directSoak) command(label string) (tr.JobPin, string, error) {
	pin, ok := s.b.Jobs[label]
	if !ok {
		return pin, "", fmt.Errorf("soak: no job %s in the profile bundle", label)
	}
	cmd := cloudQuote(pin.Executable)
	for _, a := range pin.Argv {
		cmd += " " + cloudQuote(a)
	}
	return pin, cmd, nil
}

// step runs one bounded job and returns its receipt as the approved path would.
func (s *directSoak) step(ctx context.Context, label string, seconds int) (tr.ExitInfo, error) {
	pin, cmd, err := s.command(label)
	if err != nil {
		return tr.ExitInfo{}, err
	}
	out, err := cloudStep(ctx, s.node(pin.Node), s.c.Output, "soak-"+label+".json", label, seconds, cmd)
	e := exitInfo(out, 0)
	if err != nil {
		e.Code = 1
	}
	return e, err
}

func exitInfo(out string, code int) tr.ExitInfo {
	sum := sha256.Sum256([]byte(out))
	return tr.ExitInfo{Code: code, Settled: true, Output: tr.CapturedOutput{Complete: true, StdoutBase64: base64.StdEncoding.EncodeToString([]byte(out)), StdoutSHA256: fmt.Sprintf("%x", sum)}}
}

// start runs a job as a transient unit owned by the ssh user, stdout to a file.
func (s *directSoak) start(ctx context.Context, label string) error {
	pin, cmd, err := s.command(label)
	if err != nil {
		return err
	}
	h := s.host(pin.Node)
	out := path.Join(h.RunRoot, label+".stdout")
	_, err = cloudStep(ctx, s.node(pin.Node), s.c.Output, "soak-start-"+label+".json", "start-"+label, 30,
		"sudo systemd-run --unit="+cloudQuote(s.unit(label))+" --uid="+cloudQuote(h.Management.User)+
			" --property=WorkingDirectory="+cloudQuote(h.RunRoot)+" --property=StandardOutput="+cloudQuote("file:"+out)+" "+cmd)
	return err
}

// identity is a unit's main process as lab_profile.process() reads it.
func (s *directSoak) identity(ctx context.Context, node, label string) (tr.ProcessIdentity, error) {
	out, err := cloudStep(ctx, s.node(node), s.c.Output, "soak-identity-"+label+".json", "identity-"+label, 20,
		"p=$(systemctl show -p MainPID --value "+cloudQuote(s.unit(label))+"); test \"$p\" -gt 0; "+
			"for i in $(seq 50); do case $(readlink /proc/$p/exe) in */weed|*/weed-volume) break;; esac; sleep 0.1; done; "+
			"echo $p $(awk '{print $22}' /proc/$p/stat) $(cat /proc/sys/kernel/random/boot_id) $(sha256sum /proc/$p/exe | cut -d' ' -f1)")
	f := strings.Fields(out)
	if err != nil || len(f) != 4 {
		return tr.ProcessIdentity{}, errors.Join(err, fmt.Errorf("soak: no process identity for %s", label))
	}
	pid, e1 := strconv.Atoi(f[0])
	start, e2 := strconv.ParseUint(f[1], 10, 64)
	return tr.ProcessIdentity{Node: node, PID: pid, PGID: pid, StartTime: start, BootID: f[2], ExecutableSHA256: f[3], RunID: s.d.RunID}, errors.Join(e1, e2)
}

func (s *directSoak) StartTask(ctx context.Context, label string) (tr.JobHandle, error) {
	s.load = label
	return tr.JobHandle{}, s.start(ctx, label)
}

// WaitTask waits (bounded by the work deadline) for the one task's unit to
// end, then returns its exit status and stdout. A failed transient unit stays
// loaded with its status; a clean exit unloads it and reads as status 0.
func (s *directSoak) WaitTask(ctx context.Context, _ tr.JobHandle) (tr.ExitInfo, error) {
	pin, _, err := s.command(s.load)
	if err != nil {
		return tr.ExitInfo{}, err
	}
	u := cloudQuote(s.unit(s.load))
	out, err := cloudStep(ctx, s.node(pin.Node), s.c.Output, "soak-wait.json", "wait-"+s.load, int(time.Until(s.deadline)/time.Second)+60,
		"while systemctl is-active -q "+u+"; do sleep 2; done; systemctl show -p ExecMainStatus --value "+u+
			"; cat "+cloudQuote(path.Join(s.host(pin.Node).RunRoot, s.load+".stdout")))
	code, body, _ := strings.Cut(out, "\n")
	rc, cerr := strconv.Atoi(strings.TrimSpace(code))
	return exitInfo(body, rc), errors.Join(err, cerr)
}

// Restart kills the victim's unit and starts the replacement generation, with
// the controller clock read immediately before the kill (fault_errors needs it).
func (s *directSoak) Restart(ctx context.Context, episode string, _ tr.JobHandle) (tr.JobHandle, tr.GenerationRestartReceipt, error) {
	plan, ok := s.b.Restarts[episode]
	r := tr.GenerationRestartReceipt{Plan: plan}
	if !ok || plan.Hold {
		return tr.JobHandle{}, r, fmt.Errorf("soak: no replace plan for %s", episode)
	}
	victim, _, err := s.command(plan.Victim)
	if err != nil {
		return tr.JobHandle{}, r, err
	}
	old, err := s.identity(ctx, victim.Node, plan.Victim)
	var pre float64
	if err == nil {
		pre, err = controllerMono()
	}
	if u := cloudQuote(s.unit(plan.Victim)); err == nil {
		_, err = cloudStep(ctx, s.node(victim.Node), s.c.Output, "soak-kill-"+episode+".json", "kill-"+episode, 20,
			"sudo systemctl kill -s KILL "+u+"; while systemctl is-active -q "+u+"; do sleep 0.1; done")
	}
	if err != nil {
		return tr.JobHandle{}, r, err
	}
	r.Kill = tr.KillWitness{Identity: old, AliveBefore: true, Signal: "SIGKILL", AppliedAt: time.Now(), PreKillMono: pre}
	if err = s.start(ctx, plan.Replacement); err != nil {
		return tr.JobHandle{}, r, err
	}
	r.Replacement, err = s.identity(ctx, victim.Node, plan.Replacement)
	r.State = "REPLACED"
	return tr.JobHandle{}, r, err
}

func (s *directSoak) RunJob(ctx context.Context, label string) (tr.ExitInfo, error) {
	return s.step(ctx, label, 60)
}
func (s *directSoak) Mono() (float64, error)                 { return controllerMono() }
func (s *directSoak) Now() time.Time                         { return time.Now() }
func (s *directSoak) After(d time.Duration) <-chan time.Time { return time.After(d) }
func (s *directSoak) WorkDeadline() time.Time                { return s.deadline }

// prepare makes the host's private run root and checks every declared file
// against its pin. Binaries are copied before the run (the lab process); a
// missing or different file refuses the run here, before any service starts.
func (s *directSoak) prepare(ctx context.Context, h inventory.Host) error {
	sums := ""
	for p, sum := range s.in.Files[h.ID] {
		sums += sum + "  " + p + "\n"
	}
	_, err := cloudStep(ctx, s.c.node(h), s.c.Output, "soak-prepare-"+h.ID+".json", "prepare-"+h.ID, 120,
		"sudo install -d -m 0700 -o "+cloudQuote(h.Management.User)+" "+cloudQuote(h.RunRoot)+
			"; printf %s "+cloudQuote(sums)+" | sha256sum -c --quiet -")
	return err
}

// runDirectSoak is the kv_soak phase of the direct entry, in the approved
// path's order: preflight, deploy and READY, setup (clock S1), the fault
// schedule, collect, then M02 evidence (clock S2) and the assessor.
func runDirectSoak(ctx context.Context, inventoryRaw []byte, c CloudConfig) (rows string, failure error) {
	raw, err := os.ReadFile(c.SoakProfile)
	var in ProfileInput
	if err == nil {
		in, err = DecodeProfileInput(raw)
	}
	if err == nil && !bytes.Equal(bytes.TrimSpace(inventoryRaw), bytes.TrimSpace([]byte(in.Requested))) {
		err = errors.New("kv_soak: the inventory is not the soak profile's requested inventory")
	}
	b, berr := BuildProfile(in)
	if err = errors.Join(err, berr); err != nil {
		return "", err
	}
	p := b.Plan
	d, err := inventory.Join([]byte(in.Requested), []byte(in.Resolved))
	if err != nil || p.Soak == nil {
		return "", errors.Join(err, errors.New("kv_soak needs a soak profile input"))
	}
	boot := observedBoot(d, "m01")
	if local, err := os.ReadFile("/proc/sys/kernel/random/boot_id"); err != nil || boot == "" || string(bytes.TrimSpace(local)) != boot {
		return "", errors.New("kv_soak: the controller must run on the admitted M01 boot")
	}
	deadline, _ := ctx.Deadline()
	c.localHost = "m01"
	s := &directSoak{soakWindow: soakWindow{root: runRoot(d, "m01"), runID: d.RunID, bootID: boot, roles: map[string]string{}},
		c: c, d: d, b: b, in: in, deadline: deadline}
	defer func() { // every host is cleaned and checked, each into its own output dir
		for _, h := range d.Hosts {
			hc := c
			hc.Output = filepath.Join(c.Output, h.ID)
			failure = errors.Join(failure, os.MkdirAll(hc.Output, 0700), directCleanup(hc, *d, h))
		}
	}()
	for _, h := range d.Hosts {
		if err = s.prepare(ctx, h); err != nil {
			return "", err
		}
	}
	// In order, stopping at the first failure; a deploy job is a long-running
	// unit (the approved runtime's "background"), every other job a bounded step.
	steps := func(lists ...[]Step) (err error) {
		for _, list := range lists {
			for i := 0; i < len(list) && err == nil; i++ {
				if containsStep(p.Deploy, list[i].Job) {
					err = s.start(ctx, list[i].Job)
				} else {
					_, err = s.step(ctx, list[i].Job, 120)
				}
			}
		}
		return err
	}
	if err = steps(p.Preflight, p.Deploy, p.Ready); err != nil {
		return "", err
	}
	if err = s.clocked(ctx, p.Soak.Setup.Job, s.root, "soak-clock-s1.json", ""); err != nil {
		return "", err
	}
	records, _, serr := runSoak(ctx, s, p, tr.PackInputs{Jobs: b.Jobs, Restarts: b.Restarts, Files: in.Files}, map[string]tr.JobHandle{})
	if err = errors.Join(serr, publishFile(filepath.Join(s.root, "faults.json"), records)); err != nil {
		return "", err
	}
	if err = steps(p.Collect); err != nil {
		return "", err
	}
	want, err := soakBinding(d)
	if err == nil {
		err = s.clocked(ctx, p.Soak.Evidence.Job, want.Root, "soak-clock-s2.json", "m02-collect.json")
	}
	if err != nil {
		return "", err
	}
	e, err := s.step(ctx, p.Soak.Assess.Job, 120)
	if err != nil {
		return "", err
	}
	v, err := decodeVerdict(e, want)
	rows = filepath.Join(c.Output, "soak-verdict.json")
	body, _ := json.MarshalIndent(v, "", " ")
	if err = errors.Join(err, os.WriteFile(rows, body, 0600)); err == nil && v.Status != "PASS" {
		err = errors.New("soak verdict FAIL")
	}
	return rows, err
}

// clocked runs one M02 job inside a paired clock sample: t1 at the ssh exec,
// t4 on return. With evidence set, its row is first published as that file.
func (s *directSoak) clocked(ctx context.Context, label, root, clock, evidence string) error {
	var e tr.ExitInfo
	var t4 float64
	t1, err := controllerMono()
	if err == nil {
		e, err = s.step(ctx, label, 300)
	}
	if err == nil {
		t4, err = controllerMono()
	}
	if err != nil {
		return err
	}
	row, _, err := e.Output.Decode()
	if err == nil && evidence != "" {
		var probe struct {
			Status string `json:"status"`
		}
		if oneStrictValue(row) != nil || json.Unmarshal(row, &probe) != nil || probe.Status != "SOAK_COLLECTED" {
			return errors.New("soak M02 evidence failed, truncated or malformed")
		}
		err = clockPublish(path.Join(root, evidence), row)
	}
	if err != nil {
		return err
	}
	return publishSoakClock(root, clock, s.d.RunID, row, t1, t1, t4)
}
