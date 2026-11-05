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
	load     string                 // the one task's job label
	nodes    map[string]*infra.Node // one connection per host, opened by open
	hosts    map[string]inventory.Host
}

func (s *directSoak) unit(label string) string { return "sw-cloud-" + s.d.RunID + "-" + label }

// open connects to each host once for the run (cleanup opens its own) and
// prepares it: a private run root, every declared release file uploaded from
// the local release tree (system files such as the interpreter are only
// checked), then every declared file checked against its pin, before any
// service starts.
func (s *directSoak) open(ctx context.Context) (err error) {
	s.nodes, s.hosts = map[string]*infra.Node{}, map[string]inventory.Host{}
	for _, h := range s.d.Hosts {
		n, sums := s.c.node(h), ""
		s.nodes[h.ID], s.hosts[h.ID] = n, h
		for p, sum := range s.in.Files[h.ID] {
			sums += sum + "  " + p + "\n"
		}
		if err = n.ConnectContext(ctx); err == nil {
			_, err = cloudStep(ctx, n, s.c.Output, "soak-prepare-"+h.ID+".json", "prepare-"+h.ID, 30,
				"sudo install -d -m 0700 -o "+cloudQuote(h.Management.User)+" "+cloudQuote(h.RunRoot))
		}
		release := path.Dir(s.b.Plan.Smoke.HelperDir)
		for p := range s.in.Files[h.ID] {
			rel, ok := strings.CutPrefix(p, release+"/")
			if tmp := path.Join(h.RunRoot, "upload-"+strings.ReplaceAll(rel, "/", "_")); ok && err == nil {
				if err = cloudUpload(n, s.c.Output, "soak-upload-"+h.ID+"-"+path.Base(tmp)+".json", rel, filepath.Join(s.c.SoakRelease, filepath.FromSlash(rel)), tmp); err == nil {
					_, err = cloudStep(ctx, n, s.c.Output, "soak-install-"+h.ID+"-"+path.Base(tmp)+".json", "install-"+rel, 60,
						"sudo install -D -m 0755 "+cloudQuote(tmp)+" "+cloudQuote(p)+" && rm -f "+cloudQuote(tmp))
				}
			}
		}
		if err == nil {
			_, err = cloudStep(ctx, n, s.c.Output, "soak-check-"+h.ID+".json", "check-"+h.ID, 120, "printf %s "+cloudQuote(sums)+" | sha256sum -c --quiet -")
		}
		if err != nil {
			return err
		}
	}
	return nil
}

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
	start := time.Now()
	out, err := cloudStep(ctx, s.nodes[pin.Node], s.c.Output, "soak-"+label+".json", label, seconds, cmd)
	e := exitInfo(out, 0, start)
	if err != nil { // a failed or unreachable step has no known end
		e.Code, e.Settled, e.Output.Complete = 1, false, false
	}
	return e, err
}

// exitInfo is a step's receipt: its stdout, read between start and now. The
// step's stderr is journalled in its step file, not carried here (empty).
func exitInfo(out string, code int, start time.Time) tr.ExitInfo {
	return tr.ExitInfo{Code: code, Settled: true, Output: tr.CapturedOutput{Complete: true, ReadStart: start, ReadEnd: time.Now(),
		StdoutBase64: base64.StdEncoding.EncodeToString([]byte(out)), StdoutSHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(out))), StderrSHA256: fmt.Sprintf("%x", sha256.Sum256(nil))}}
}

// start runs a job as a transient unit owned by the ssh user, with the ssh
// session's unlimited locked memory (RDMA registers its buffers; a unit's
// default is 8 MiB, and ibv_reg_mr then fails with ENOMEM), its stdout and
// stderr to this generation's own files in the run root (launch records both;
// ready and collect read them back).
func (s *directSoak) start(ctx context.Context, label string) error {
	pin, cmd, err := s.command(label)
	if err != nil {
		return err
	}
	h := s.hosts[pin.Node]
	_, err = cloudStep(ctx, s.nodes[pin.Node], s.c.Output, "soak-start-"+label+".json", "start-"+label, 30,
		"sudo systemd-run --unit="+cloudQuote(s.unit(label))+unitLifetime(*s.d)+" --uid="+cloudQuote(h.Management.User)+
			" --property=LimitMEMLOCK=infinity --property=WorkingDirectory="+cloudQuote(h.RunRoot)+" --property=StandardOutput="+cloudQuote("file:"+path.Join(h.RunRoot, label+".stdout"))+
			" --property=StandardError="+cloudQuote("file:"+path.Join(h.RunRoot, label+".stderr"))+" "+cmd)
	return err
}

// identity is a unit's main process as lab_profile.process() reads it.
func (s *directSoak) identity(ctx context.Context, node, label string) (tr.ProcessIdentity, error) {
	out, err := cloudStep(ctx, s.nodes[node], s.c.Output, "soak-identity-"+label+".json", "identity-"+label, 20,
		"p=$(systemctl show -p MainPID --value "+cloudQuote(s.unit(label))+"); test \"$p\" -gt 0; "+
			"for i in $(seq 50); do case $(readlink /proc/$p/exe) in */weed|*/weed-volume) break;; esac; sleep 0.1; done; "+
			"echo $p $(sed 's/.*) //' /proc/$p/stat | cut -d' ' -f20) $(cat /proc/sys/kernel/random/boot_id) $(sha256sum /proc/$p/exe | cut -d' ' -f1)")
	f := strings.Fields(out)
	if err != nil || len(f) != 4 {
		return tr.ProcessIdentity{}, errors.Join(err, fmt.Errorf("soak: no process identity for %s", label))
	}
	pid, e1 := strconv.Atoi(f[0])
	start, e2 := strconv.ParseUint(f[1], 10, 64)
	if s.c.ReadCase != "" {
		binary := "weed-volume"
		if s.c.ReadCase == "filer-restart" {
			binary = "weed"
		}
		if pid <= 0 || start == 0 || f[2] != observedBoot(s.d, node) || f[3] != s.in.Files[node][path.Join(path.Dir(s.b.Plan.Smoke.HelperDir), "bin", binary)] {
			return tr.ProcessIdentity{}, errors.New("read_case foreign or invalid process identity")
		}
	}

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
	pin, u, start := s.b.Jobs[s.load], cloudQuote(s.unit(s.load)), time.Now() // StartTask checked the label
	out, err := cloudStep(ctx, s.nodes[pin.Node], s.c.Output, "soak-wait.json", "wait-"+s.load, int(time.Until(s.deadline)/time.Second)+60,
		"while systemctl is-active -q "+u+"; do sleep 2; done; systemctl show -p ExecMainStatus --value "+u+
			"; cat "+cloudQuote(path.Join(s.hosts[pin.Node].RunRoot, s.load+".stdout")))
	code, body, _ := strings.Cut(out, "\n")
	rc, cerr := strconv.Atoi(strings.TrimSpace(code))
	e := exitInfo(body, rc, start)
	if err = errors.Join(err, cerr); err != nil { // an unknown end is never a settled one
		e.Code, e.Settled, e.Output.Complete = -1, false, false
	} else if rc != 0 { // a settled failure stays a failure
		err = fmt.Errorf("soak: load task %s exited %d", s.load, rc)
	}
	return e, err
}

// Restart kills the victim's unit and starts the replacement generation, with
// the controller clock read immediately before the kill (fault_errors needs it).
func (s *directSoak) Restart(ctx context.Context, episode string, _ tr.JobHandle) (tr.JobHandle, tr.GenerationRestartReceipt, error) {
	plan, ok := s.b.Restarts[episode]
	victim, known := s.b.Jobs[plan.Victim]
	r := tr.GenerationRestartReceipt{Plan: plan}
	if !ok || !known || plan.Hold {
		return tr.JobHandle{}, r, fmt.Errorf("soak: no replace plan for %s", episode)
	}
	old, err := s.identity(ctx, victim.Node, plan.Victim)
	var pre float64
	if err == nil {
		pre, err = controllerMono()
	}
	if u := cloudQuote(s.unit(plan.Victim)); err == nil {

		command := "sudo systemctl kill -s KILL " + u + "; while systemctl is-active -q " + u + "; do sleep 0.1; done"
		if s.c.ReadCase != "" {
			prefix := mainPID(s.unit(plan.Victim))
			command = prefix + fmt.Sprintf("test \"$pid:$start\" = '%d:%d'; ", old.PID, old.StartTime) + strings.TrimPrefix(chaosKillScript(s.unit(plan.Victim)), prefix)
		}
		_, err = cloudStep(ctx, s.nodes[victim.Node], s.c.Output, "soak-kill-"+episode+".json", "kill-"+episode, 20, command)
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

// Horizon is the direct path's soak horizon (#1002); runSoak's schedule check and waits use it.
func (s *directSoak) Horizon() time.Duration { return DirectSoakHorizon }

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
	d, err := inventory.Join([]byte(in.Requested), []byte(in.Resolved))
	if err != nil || (b.Plan.Soak == nil) != (c.ReadCase != "") {
		return "", errors.Join(err, errors.New("kv_soak needs a soak profile input"))
	}
	if b.Plan.Soak != nil && (b.Plan.Soak.Pattern != nil) != c.KVLoad {
		return "", errors.New("kv_load needs a profile with a load pattern, and kv_soak one without")
	}
	boot := observedBoot(d, "m01")
	if local, err := os.ReadFile("/proc/sys/kernel/random/boot_id"); err != nil || boot == "" || string(bytes.TrimSpace(local)) != boot {
		return "", errors.New("kv_soak: the controller must run on the admitted M01 boot")
	}
	deadline, err := time.Parse(time.RFC3339Nano, d.Deadlines.Work) // the work deadline the role commands obey
	if err != nil {
		return "", err
	}
	if timeout, bounded := ctx.Deadline(); b.Plan.Soak != nil && bounded {
		if err = directSoakBudget(b.Plan.Soak, time.Now(), deadline, timeout); err != nil {
			return "", err // before any deploy: an unsized long soak would fail hours in
		}
	}
	c.localHost = "m01"
	executor := &directSoak{soakWindow: soakWindow{root: runRoot(d, "m01"), runID: d.RunID, bootID: boot, roles: map[string]string{}},
		c: c, d: d, b: b, in: in, deadline: deadline}
	if c.ReadCase != "" {
		if err = executor.readJobs(); err != nil {
			return "", err
		}
	}
	return executor.run(ctx)
}

// run executes the plan, then always cleans up and postflights every host,
// each into its own output directory.
func (s *directSoak) run(ctx context.Context) (rows string, failure error) {
	p, d, c := s.b.Plan, s.d, s.c
	defer func() {
		for _, h := range d.Hosts {
			if n := s.nodes[h.ID]; n != nil {
				n.Close()
			}
			hc := c
			hc.Output = filepath.Join(c.Output, h.ID)
			failure = errors.Join(failure, os.MkdirAll(hc.Output, 0700), directCleanup(hc, *d, h))
		}
	}()
	if err := s.open(ctx); err != nil {
		return "", err
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
	if err := steps(p.Preflight, p.Deploy, p.Ready); err != nil {
		return "", err
	}
	if c.ReadCase != "" {
		rows, err := s.readCase(ctx)
		return rows, errors.Join(err, steps(p.Collect))
	}
	if err := s.clocked(ctx, p.Soak.Setup.Job, s.root, "soak-clock-s1.json", ""); err != nil {
		return "", err
	}
	records, exit, err := runSoak(ctx, s, p, tr.PackInputs{Jobs: s.b.Jobs, Restarts: s.b.Restarts, Files: s.in.Files}, map[string]tr.JobHandle{})
	if err == nil && (!exit.Settled || exit.Code != 0) { // the load's receipt is part of the result
		err = fmt.Errorf("soak: load ended unsettled or with status %d", exit.Code)
	}
	if err = errors.Join(err, publishFile(filepath.Join(s.root, "faults.json"), records)); err != nil {
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
	e, err := s.step(ctx, p.Soak.Assess.Job, int(assessTimeout(p.Soak.DurationS)/time.Second)) // reserved by trailingReserve
	if err != nil {
		return "", err
	}
	v, err := decodeVerdict(e, want, p.Soak.criteria())
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
		// #1002: the row is the evidence file's reference; the file itself, past the 128 KiB a job's stdout may
		// carry at 2 h, is read back through the same step path and checked against it before publication.
		node := s.b.Jobs[label].Node
		var body []byte
		body, err = m02Evidence(row, evidence, func(name string) (string, error) {
			return cloudStep(ctx, s.nodes[node], s.c.Output, "soak-fetch-"+name, "fetch-"+name, 120,
				"cat "+cloudQuote(path.Join(s.hosts[node].RunRoot, name)))
		})
		if err == nil {
			err = clockPublish(path.Join(root, evidence), body)
		}
	}
	if err != nil {
		return err
	}
	return publishSoakClock(root, clock, s.d.RunID, row, t1, t1, t4)
}
