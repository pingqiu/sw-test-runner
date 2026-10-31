//go:build linux

package seaweed

import (
	"context"
	"encoding/json"
	"errors"
	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/inventory"
	"golang.org/x/sys/unix"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

type restartCPU struct {
	*cpuNode
	t                              *testing.T
	mode                           string
	role                           string
	killMono                       float64
	killed                         map[string]bool
	events                         []string
	seed, dcRecovery, httpControls int
}

func (n *restartCPU) Kill(_ context.Context, id string) (tr.KillWitness, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if !n.live[id] {
		return tr.KillWitness{}, errors.New("victim gate lost before Kill")
	}
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts); err != nil {
		return tr.KillWitness{}, err
	}
	n.killMono = float64(ts.Sec) + float64(ts.Nsec)/1e9
	n.killed[id] = true
	delete(n.live, id)
	n.events = append(n.events, "kill")
	w := tr.KillWitness{Identity: n.ids[id], AliveBefore: true, Signal: "KILL", AppliedAt: time.Now()}
	if n.mode == "kill-identity" {
		w.Identity.PID++
	}
	return w, nil
}
func (n *restartCPU) Wait(ctx context.Context, id string) (tr.ExitInfo, error) {
	n.mu.Lock()
	s := n.specs[id]
	killed := n.killed[id]
	cleanup := n.cleanupStopped[id]
	if !s.CaptureOutput && !killed && !cleanup {
		n.mu.Unlock()
		return tr.ExitInfo{}, errors.New("live victim Wait started before restart owned the gate")
	}
	n.mu.Unlock()
	e, err := n.cpuNode.Wait(ctx, id)
	if err != nil {
		return e, err
	}
	if killed {
		e.Code, e.Signal, e.CleanupStopped = -1, 9, false
		return e, nil
	}
	if !s.CaptureOutput {
		return e, nil
	}
	var raw []byte
	if strings.HasSuffix(s.Argv[1], "kv_smoke.py") {
		var c SmokeConfig
		if err = json.Unmarshal([]byte(s.Argv[3]), &c); err != nil {
			return e, err
		}
		stage, route := s.Argv[5], s.Argv[7]
		if stage == "load" {
			n.events = append(n.events, "load-wait")
			row := windowBody(n.t, c)
			if n.mode == "zero-errors" {
				row["faults"], row["exact"] = []any{}, 6
				for _, v := range row["outcomes"].([]any) {
					v.(map[string]any)["exact"] = true
				}
				after := row["after"].(map[string]any)
				after["read_errors"] = 0
				counters := after["loader"].(map[string]any)
				counters["read_objects"], counters["direct_dc_reads"] = 6, 6
				counters["read_bytes"], counters["direct_dc_bytes"] = 6*c.Length, 6*c.Length
			}
			if n.mode == "second-window" {
				o := row["outcomes"].([]any)
				o[3].(map[string]any)["exact"] = true
				o[4].(map[string]any)["exact"] = false
				f := row["faults"].([]any)[1].(map[string]any)
				f["operation_id"], f["key"], f["started"], f["ended"] = 4, c.Client+"-0", 37., 38.
			}
			shiftRestartTrace(row, n.killMono-30)
			if n.mode == "before-prekill" {
				shiftRestartTrace(row, -4)
			}
			raw, _ = json.Marshal(row)
		} else {
			if stage == "seed" {
				n.seed++
				n.events = append(n.events, "seed")
			} else if route == "dc" {
				n.dcRecovery++
				n.events = append(n.events, "dc-recovery")
			} else {
				n.httpControls++
				n.events = append(n.events, "http-control")
			}
			raw = n.nativeOutput(c, stage, route)
			if (n.mode == "wrong-recovery" && stage == "recovery" && route == "dc") || (n.mode == "lost-metadata" && route == "http" && n.anyKilled()) {
				raw = []byte(`{"status":"FAIL"}`)
			}
		}
	} else if n.anyKilled() && s.Argv[5] == "ready" {
		var i tr.ProcessIdentity
		role := n.role
		if role == "" {
			role = "volume"
		}
		n.mu.Lock()
		for job, spec := range n.specs {
			if len(spec.Argv) > 0 && spec.Argv[len(spec.Argv)-1] == "--replacement-"+role {
				i = n.ids[job]
			}
		}
		n.mu.Unlock()
		i.ExecutableSHA256 = strings.Repeat("d", 64)
		if n.mode == "old-ready" {
			for job := range n.killed {
				i = n.ids[job]
			}
			i.ExecutableSHA256 = strings.Repeat("d", 64)
		}
		if n.mode == "wrong-product" {
			i.ExecutableSHA256 = strings.Repeat("e", 64)
		}
		ack := n.killMono + 40
		if n.mode == "before-prekill" {
			ack = n.killMono + 50
		}
		row := map[string]any{"status": "READY", "client_mono": ack, "processes": map[string]any{role: map[string]any{"scope_identity": i}}}
		if n.mode == "missing-ready" {
			delete(row, "processes")
		}
		raw, _ = json.Marshal(row)
		n.events = append(n.events, "ready")
	} else {
		raw = []byte("{}")
	}
	e.Output = captured(raw).Output
	return e, nil
}
func (n *restartCPU) anyKilled() bool { n.mu.Lock(); defer n.mu.Unlock(); return len(n.killed) > 0 }
func (n *restartCPU) nativeOutput(c SmokeConfig, stage, route string) []byte {
	n.t.Helper()
	// Actual accepted leaves + pack CLI, replacing only the native connector.
	script := `import sys,json,datetime
sys.path.insert(0,'worker')
import kv_smoke,test_kv_smoke
c=json.loads(sys.argv[1]);stage,route=sys.argv[2:]
conn=test_kv_smoke.FakeConnector('http' if route=='http' else 'good')
if route=='http':conn.stats['loader']['transport']='http'
if stage=='seed':
 events=[]
 r=kv_smoke.run(conn,c,lambda key:dict(FileSize=c['length'],Mtime=datetime.datetime.now(datetime.timezone.utc).isoformat(),chunks=[dict(file_id='1,abc')]),events.append)
 for e in events:print(json.dumps(e))
else:
 for i in range(c['keys']):
  key=c['client']+'-'+str(i);conn.values[key]=kv_smoke.exact_helper('soak_native_worker').payload(c['run_id']+'/'+key,c['length'])
 r=kv_smoke.read_only(conn,c,stage,route)
print(json.dumps(r));conn.close()`
	b, _ := json.Marshal(c)
	raw, err := exec.Command("python3", "-B", "-c", script, string(b), stage, route).CombinedOutput()
	if err != nil {
		n.t.Fatalf("actual CLI control: %v %s", err, raw)
	}
	return raw
}
func restartFixture(t *testing.T, mode string) (*cpuFixture, *restartCPU) {
	t.Helper()
	f := cpuProfile(t)
	d, err := inventory.Decode([]byte(f.plan.Resolved))
	if err != nil {
		t.Fatal(err)
	}
	bootRaw, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		t.Fatal(err)
	}
	controllerBoot := strings.TrimSpace(string(bootRaw))
	if mode == "controller-boot" {
		controllerBoot = "11111111-1111-1111-1111-111111111111"
	}
	f.node.boots["m01"] = controllerBoot
	for i := range d.Observation.Hosts {
		if d.Observation.Hosts[i].HostID == "m01" {
			d.Observation.Hosts[i].BootID = controllerBoot
		}
	}
	for i := range f.policy.Deployments {
		if f.policy.Deployments[i].NodeID == "m01" {
			f.policy.Deployments[i].ExpectedBootID = controllerBoot
		}
	}
	f.binding.Artifacts = f.write(t, "artifacts.json", tr.ApprovedArtifacts{Version: "s0b-artifacts-v1", Deployments: f.policy.Deployments})
	requested, err := inventory.Decode([]byte(f.plan.Requested))
	if err != nil {
		t.Fatal(err)
	}
	requested.ArtifactManifestSHA256 = f.binding.Artifacts.SHA256
	d.ArtifactManifestSHA256 = f.binding.Artifacts.SHA256
	requestRaw, _ := json.Marshal(requested)
	f.plan.Requested = string(requestRaw)
	d.Observation.RequestedSHA256 = hash(requestRaw)
	resolved, _ := json.Marshal(d)
	f.plan.Resolved = string(resolved)
	for label, job := range f.policy.Jobs {
		if job.Node == "m01" && strings.HasSuffix(job.Argv[1], "lab_profile.py") {
			var config ProfileConfig
			if err := json.Unmarshal([]byte(job.Argv[3]), &config); err != nil {
				t.Fatal(err)
			}
			config.BootID = controllerBoot
			raw, _ := json.Marshal(config)
			job.Argv[3] = string(raw)
			f.policy.Jobs[label] = job
		}
	}
	f.plan.Restart = &ReadRestart{Episode: "volume-once", DC: "read-dc", RecoverDC: "recover-dc", RecoverHTTP: "control-http"}
	for _, v := range []struct{ label, stage, route string }{{"smoke", "seed", "dc"}, {"read-dc", "load", "dc"}, {"recover-dc", "recovery", "dc"}, {"control-http", "recovery", "http"}} {
		args, err := ReadLoadArgs(d, "m01", f.plan.Smoke, v.stage, v.route)
		if err != nil {
			t.Fatal(err)
		}
		j := f.policy.Jobs["smoke"]
		j.Argv = args
		f.policy.Jobs[v.label] = j
	}
	replacement := f.policy.Jobs["deploy-volume"]
	replacement.Argv = append(append([]string{}, replacement.Argv...), "--replacement-volume")
	f.policy.Jobs["replacement-volume"] = replacement
	f.policy.GenerationRestarts = map[string]tr.GenerationRestartPlan{"volume-once": {Victim: "deploy-volume", Replacement: "replacement-volume"}}
	f.editScenario = func(s *tr.Scenario) { s.Phases[2].Actions[0].Action = "seaweed_read_restart" }
	f.publish(t, nil)
	n := &restartCPU{cpuNode: f.node, t: t, mode: mode, killed: map[string]bool{}}
	f.runtime.Jobs = n
	return f, n
}
func registerFastRestart(r *tr.Registry) {
	RegisterPack(r)
	p := &runtimePack{sessions: map[*tr.RunScope]*session{}, pause: func() <-chan time.Time { c := make(chan time.Time, 1); c <- time.Now(); return c }}
	for _, name := range append(append([]string{}, phases...), "seaweed_read_restart") {
		tier := tr.TierDevOps
		if name == "seaweed_read_restart" {
			tier = tr.TierChaos
		}
		r.RegisterMutatingFunc(name, tier, p.run)
	}
}
func TestRestartThroughApprovedEngine(t *testing.T) {
	for _, mode := range []string{"positive", "old-ready", "missing-ready", "wrong-product", "kill-identity", "wrong-recovery", "second-window", "before-prekill"} {
		t.Run(mode, func(t *testing.T) {
			f, n := restartFixture(t, mode)
			result, bundle, err := tr.RunApprovedRequest(context.Background(), f.request, f.profile, registerFastRestart, &f.runtime, nil)
			if err != nil {
				t.Fatalf("entry failed %v bundle %s", err, bundle)
			}
			if mode == "positive" {
				if result == nil || result.Status != tr.StatusPass {
					t.Fatalf("result=%+v bundle=%s", result, bundle)
				}
				events := strings.Join(n.events, ",")
				if n.seed != 1 || n.dcRecovery != 1 || n.httpControls != 2 || !strings.Contains(events, "kill,ready,ready,load-wait,dc-recovery,http-control") {
					t.Fatalf("bad sequence %s", events)
				}
			} else if result == nil || result.Status != tr.StatusFail {
				t.Fatalf("invalid proof became PASS: %+v", result)
			}
			if len(n.live) != 0 {
				t.Fatal("owned issuers remain live")
			}
		})
	}
}
func TestRestartBindingRefusesBeforeAnyJob(t *testing.T) {
	for _, mode := range []string{"reused-label", "missing-episode", "wrong-replacement-argv", "http-load", "wrong-seed", "missing-profile"} {
		t.Run(mode, func(t *testing.T) {
			f, n := restartFixture(t, mode)
			switch mode {
			case "reused-label":
				f.plan.Restart.DC = f.plan.Restart.RecoverHTTP
			case "missing-episode":
				f.plan.Restart.Episode = "foreign"
			case "wrong-replacement-argv":
				j := f.policy.Jobs["replacement-volume"]
				j.Argv = j.Argv[:len(j.Argv)-1]
				f.policy.Jobs["replacement-volume"] = j
			case "http-load":
				j := f.policy.Jobs["read-dc"]
				j.Argv[len(j.Argv)-1] = "http"
				f.policy.Jobs["read-dc"] = j
			case "wrong-seed":
				j := f.policy.Jobs["smoke"]
				j.Argv[len(j.Argv)-3] = "recovery"
				f.policy.Jobs["smoke"] = j
			case "missing-profile":
				f.plan.Profile = ""
			}
			f.publish(t, nil)
			result, _, err := tr.RunApprovedRequest(context.Background(), f.request, f.profile, registerFastRestart, &f.runtime, nil)
			if err == nil && (result == nil || result.Status != tr.StatusFail) {
				t.Fatal("invalid binding passed")
			}
			if n.authorized != 0 {
				t.Fatal("binding refusal followed an effect")
			}
		})
	}
}

// Real runner CLOCK_MONOTONIC anchor; fixture outcomes retain their fixed offsets.
func shiftRestartTrace(row map[string]any, shift float64) {
	for _, k := range []string{"started", "ended", "first_exact", "last_exact"} {
		row[k] = row[k].(float64) + shift
	}
	for _, field := range []string{"outcomes", "faults"} {
		for _, value := range row[field].([]any) {
			r := value.(map[string]any)
			r["started"], r["ended"] = r["started"].(float64)+shift, r["ended"].(float64)+shift
		}
	}
}

func TestRestartRefusesForeignControllerClock(t *testing.T) {
	f, n := restartFixture(t, "controller-boot")
	result, _, err := tr.RunApprovedRequest(context.Background(), f.request, f.profile, registerFastRestart, &f.runtime, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || result.Status != tr.StatusFail || !strings.Contains(result.Error, "runner is not the admitted M01 clock") || n.seed != 0 || n.anyKilled() {
		t.Fatalf("foreign controller credited: result=%+v seeds=%d", result, n.seed)
	}
}
