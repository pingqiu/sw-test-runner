//go:build linux

package seaweed

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/infra"
)

// Real approved entry, Engine, SSHJobs JSON channel, worker leaves and oracle.
// Only the native host endpoint/custodian replies are CPU fixtures (not hardware).
func freezeFixture(t *testing.T, mode string) *cpuFixture {
	t.Helper()
	f, n := restartFixture(t, "positive")
	f.policy.GenerationRestarts = nil
	delete(f.policy.Jobs, "replacement-volume")
	delete(f.policy.Jobs, "control-http")
	f.plan.Restart.Freeze = true
	f.plan.Restart.RecoverHTTP = ""
	f.policy.ProcessPause = &tr.ProcessPausePlan{Name: "volume-once", Victim: "deploy-volume", Deadline: f.binding.HardDeadline, AllowWorkResume: true}
	var c SmokeConfig
	_ = json.Unmarshal([]byte(f.policy.Jobs["smoke"].Argv[3]), &c)
	outputDir := t.TempDir()
	for _, stage := range []string{"seed", "recovery"} {
		if err := os.WriteFile(filepath.Join(outputDir, stage), n.nativeOutput(c, stage, "dc"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	row := windowBody(t, c)
	if mode == "no-overlap" {
		row["faults"] = []any{}
		row["exact"] = 6
		times := [][2]float64{{1, 2}, {10, 11}, {60, 61}, {70, 71}, {80, 81}, {119, 120}}
		for i, v := range row["outcomes"].([]any) {
			q := v.(map[string]any)
			q["started"], q["ended"], q["exact"] = times[i][0], times[i][1], true
		}
		after := row["after"].(map[string]any)
		after["read_errors"] = 0
		counters := after["loader"].(map[string]any)
		counters["read_objects"], counters["direct_dc_reads"] = 6, 6
		counters["read_bytes"], counters["direct_dc_bytes"] = 6*c.Length, 6*c.Length
		raw, _ := json.Marshal(row)
		if _, err := CheckReadTaskOutput(captured(raw), c, "load", "dc", ReadWindow{30}); err != nil {
			t.Fatalf("no-overlap fixture invalid before exposure gate: %v", err)
		}
	}
	if mode == "second-window" {
		row["outcomes"].([]any)[4].(map[string]any)["exact"] = false
	}
	raw, _ := json.Marshal(row)
	if err := os.WriteFile(filepath.Join(outputDir, "load"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	boots, _ := json.Marshal(f.node.boots)
	script := `#!/usr/bin/python3
import sys,json,time,os,hashlib,base64
root=ROOT
boots=BOOTS
mode=MODE
spec={};ident={};settled=False;stopped=False
for line in sys.stdin:
 q=json.loads(line);op=q['Operation'];r={'Sequence':q['Sequence']}
 with open(root+'/commands','a') as trace:trace.write(json.dumps(dict(operation=op,argv=q.get('Spec',{}).get('Argv',[])))+'\n')
 if op=='enroll':
  spec=q['Spec'];a=q['Admission'];ident=dict(Node=spec['Node'],BootID=boots[spec['Node']],RunID=a['RunID'],ExecutableSHA256=spec['SHA256'],PID=os.getpid(),PGID=os.getpid(),StartTime=42)
  r.update(Identity=ident,EffectDeadline=a['EffectDeadline'])
 elif op.startswith('pause-'):
  if op=='pause-stop':
   stopped=True
   with open(root+'/stop','w') as f:f.write(str(time.monotonic()))
  if op=='pause-resume':stopped=False
  r['Pause']=dict(Identity=ident,Before='LIVE',State='STOPPED' if stopped else 'LIVE',ObservedAt=time.strftime('%Y-%m-%dT%H:%M:%SZ',time.gmtime()))
  if op=='pause-resume' and mode=='cont-failure':r['Error']='injected CONT failure'
 elif op in ('status','settle') and (spec['CaptureOutput'] or op=='settle'):
  cleanup=op=='settle' and not spec['CaptureOutput']
  e=dict(Identity=ident,Settled=True,Code=-1 if cleanup else 0,Signal=9 if cleanup else 0,CleanupStopped=cleanup,StdoutPath=spec['StdoutPath'],StderrPath=spec['StderrPath'])
  if spec['CaptureOutput']:
   args=spec['Argv'];raw=b'{}'
   if '--stage' in args:
    stage=args[args.index('--stage')+1];raw=open(root+'/'+stage,'rb').read()
    if stage=='load':
     b=json.loads(raw);shift=float(open(root+'/stop').read())-30
     for k in ('started','ended','first_exact','last_exact'):b[k]+=shift
     for k in ('outcomes','faults'):
      for v in b[k]:v['started']+=shift;v['ended']+=shift
     raw=json.dumps(b).encode()
   e['Output']=dict(Complete=True,StdoutBase64=base64.b64encode(raw).decode(),StderrBase64='',StdoutSHA256=hashlib.sha256(raw).hexdigest(),StderrSHA256=hashlib.sha256(b'').hexdigest(),ReadStart=time.strftime('%Y-%m-%dT%H:%M:%SZ',time.gmtime()),ReadEnd=time.strftime('%Y-%m-%dT%H:%M:%SZ',time.gmtime()))
  r['Exit']=e
 print(json.dumps(r),flush=True)
`
	root, _ := json.Marshal(outputDir)
	m, _ := json.Marshal(mode)
	script = strings.NewReplacer("ROOT", string(root), "BOOTS", string(boots), "MODE", string(m)).Replace(script)
	runner := filepath.Join(outputDir, "custody.py")
	if err := os.WriteFile(runner, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	f.runtime.Jobs = &tr.SSHJobs{Nodes: map[string]*infra.Node{"m01": {IsNative: true}, "m02": {IsNative: true}}, Runner: runner}
	f.publish(t, nil)
	return f
}
func TestFreezeThroughApprovedEngine(t *testing.T) {
	for _, mode := range []string{"positive", "cont-failure", "second-window", "no-overlap"} {
		t.Run(mode, func(t *testing.T) {
			f := freezeFixture(t, mode)
			result, bundle, err := tr.RunApprovedRequest(context.Background(), f.request, f.profile, registerFastRestart, &f.runtime, nil)
			if err != nil {
				t.Fatalf("entry=%v bundle=%s", err, bundle)
			}
			want := tr.StatusFail
			if mode == "positive" {
				want = tr.StatusPass
			}
			if mode == "positive" {
				raw, err := os.ReadFile(filepath.Join(bundle, "artifacts", "authority.json"))
				if err != nil {
					t.Fatal(err)
				}
				var evidence struct{ Pause *tr.ProcessPauseReceipt }
				if err = json.Unmarshal(raw, &evidence); err != nil {
					t.Fatal(err)
				}
				if evidence.Pause == nil || evidence.Pause.Final.State != "LIVE" || evidence.Pause.PreContMono-evidence.Pause.StoppedMono < 20 {
					t.Fatalf("missing final pause proof: %s", raw)
				}
				commands, err := os.ReadFile(filepath.Join(filepath.Dir(f.runtime.Jobs.(*tr.SSHJobs).Runner), "commands"))
				if err != nil {
					t.Fatal(err)
				}
				if strings.Count(string(commands), `"operation": "pause-resume"`) != 1 || strings.Count(string(commands), `"operation": "pause-stop"`) != 1 {
					t.Fatalf("STOP/CONT not one-shot: %s", commands)
				}
			}
			if result == nil || result.Status != want {
				t.Fatalf("mode=%s result=%+v bundle=%s", mode, result, bundle)
			}
		})
	}
}

func TestFreezeBindingRefusals(t *testing.T) {
	for _, mode := range []string{"no-optin", "wrong-victim", "foreign-episode", "mixed-restart", "http-claim", "filer-role", "http-load"} {
		t.Run(mode, func(t *testing.T) {
			f := freezeFixture(t, mode)
			switch mode {
			case "no-optin":
				f.policy.ProcessPause.AllowWorkResume = false
			case "wrong-victim":
				f.policy.ProcessPause.Victim = "deploy-filer"
			case "foreign-episode":
				f.plan.Restart.Episode = "foreign"
			case "mixed-restart":
				f.policy.GenerationRestarts = map[string]tr.GenerationRestartPlan{"other": {Victim: "deploy-volume", Replacement: "deploy-filer"}}
			case "http-claim":
				f.plan.Restart.RecoverHTTP = "smoke"
			case "filer-role":
				f.plan.Restart.Role = "filer"
			case "http-load":
				j := f.policy.Jobs["read-dc"]
				j.Argv[len(j.Argv)-1] = "http"
				f.policy.Jobs["read-dc"] = j
			}
			f.publish(t, nil)
			result, _, err := tr.RunApprovedRequest(context.Background(), f.request, f.profile, registerFastRestart, &f.runtime, nil)
			if err == nil && (result == nil || result.Status != tr.StatusFail) {
				t.Fatalf("bad binding passed: %+v", result)
			}
			if _, e := os.Stat(filepath.Join(filepath.Dir(f.runtime.Jobs.(*tr.SSHJobs).Runner), "stop")); !os.IsNotExist(e) {
				t.Fatalf("binding refusal dispatched STOP: %v", e)
			}
		})
	}
}
