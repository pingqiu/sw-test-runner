//go:build linux

package seaweed

import (
	"context"
	"encoding/json"
	"errors"
	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/inventory"
	"os/exec"
	"strings"
	"testing"
	"time"
)

type restartCPU struct {
	*cpuNode
	t                              *testing.T
	mode                           string
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
			if n.mode == "second-window" {
				o := row["outcomes"].([]any)
				o[3].(map[string]any)["exact"] = true
				o[4].(map[string]any)["exact"] = false
				f := row["faults"].([]any)[1].(map[string]any)
				f["operation_id"], f["key"] = 4, c.Client+"-0"
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
			if n.mode == "wrong-recovery" && stage == "recovery" && route == "dc" {
				raw = []byte(`{"status":"FAIL"}`)
			}
		}
	} else if n.anyKilled() && s.Argv[5] == "ready" {
		var i tr.ProcessIdentity
		n.mu.Lock()
		for job, spec := range n.specs {
			if len(spec.Argv) > 0 && spec.Argv[len(spec.Argv)-1] == "--replacement-volume" {
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
		ack := 30.
		if n.mode == "before-ack" {
			ack = 33
		}
		row := map[string]any{"status": "READY", "client_mono": ack, "processes": map[string]any{"volume": map[string]any{"scope_identity": i}}}
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
	for _, mode := range []string{"positive", "old-ready", "missing-ready", "wrong-product", "kill-identity", "wrong-recovery", "second-window", "before-ack"} {
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
