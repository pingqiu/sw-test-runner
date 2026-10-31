package seaweed

import (
	"encoding/json"
	"os"
	"os/exec"
	"reflect"
	"testing"
)

func TestReviewerRealShapedWindowRecords(t *testing.T) {
	// The retained trace closes after marker1030+60 and is now correctly refused.
	// Its positive uses marker1032.75, the sampled success before the first error.
	raw, err := os.ReadFile("testdata/restart-budget-monotonic.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"positive-480-GETs", "fault-times-disagree", "outcomes-outside-load", "late-short-window", "old-marker-budget-exceeded", "zero-errors", "shift-all-operation-times"} {
		t.Run(mode, func(t *testing.T) {
			var r map[string]any
			if e := json.Unmarshal(raw, &r); e != nil {
				t.Fatal(e)
			}
			switch mode {
			case "zero-errors":
				r["faults"] = []any{}
				r["exact"] = 480.
				r["after"].(map[string]any)["read_errors"] = 0.
				for _, v := range r["outcomes"].([]any) {
					v.(map[string]any)["exact"] = true
				}
			case "fault-times-disagree":
				f := r["faults"].([]any)[0].(map[string]any)
				// Same retained client trace converted to the unified monotonic contract.
				// This operation stays at 1033, but its failure claims 1001.
				f["started"] = 1001.
				f["ended"] = 1001.01
			case "outcomes-outside-load":
				// The actual production read_only call ran from monotonic1000 to1120.
				// Keep its duration/failure records, but shift outcomes until the tail is past1120.
				for _, v := range r["outcomes"].([]any) {
					o := v.(map[string]any)
					o["started"] = o["started"].(float64) + 20
					o["ended"] = o["ended"].(float64) + 20
				}
			case "shift-all-operation-times":
				for _, v := range r["outcomes"].([]any) {
					o := v.(map[string]any)
					o["started"] = o["started"].(float64) + 20
					o["ended"] = o["ended"].(float64) + 20
				}
				for _, v := range r["faults"].([]any) {
					f := v.(map[string]any)
					f["started"] = f["started"].(float64) + 20
					f["ended"] = f["ended"].(float64) + 20
				}
			case "late-short-window":
				// A new isolated error 70 seconds after the post-Kill marker, not a <=60s recovery.
				for i, v := range r["outcomes"].([]any) {
					v.(map[string]any)["exact"] = i != 400
				}
				f := r["faults"].([]any)[0].(map[string]any)
				f["operation_id"] = 400
				f["key"] = "m01-0"
				f["started"] = 1100.
				f["ended"] = 1100.
				r["faults"] = []any{f}
				r["exact"] = 479.
				r["after"].(map[string]any)["read_errors"] = 1.
			}
			marker := 1032.75
			if mode == "shift-all-operation-times" {
				marker += 20
			}
			if mode == "old-marker-budget-exceeded" {
				marker = 1030
			}
			b, _ := json.Marshal(r)
			_, e := CheckReadTaskOutput(captured(b), readConfig(), "load", "dc", ReadWindow{marker})
			if (mode == "positive-480-GETs" || mode == "zero-errors") && e != nil {
				t.Fatal(e)
			}
			if mode != "positive-480-GETs" && mode != "zero-errors" && e == nil {
				t.Fatal("inconsistent/out-of-load operation times accepted")
			}
		})
	}
}
func TestReviewerTraceReproducedByUnifiedCLI(t *testing.T) {
	script := `import sys,json
sys.path.insert(0,'worker')
import kv_smoke
from test_kv_smoke import FakeConnector
from unittest.mock import patch
class RestartConnector(FakeConnector):
 def __init__(self):super().__init__();self.operation=0
 def submit_batch_get(self,keys,buffers):
  self.mode='miss' if 132<=self.operation<368 else 'good'
  self.operation+=1
  return super().submit_batch_get(keys,buffers)
 def complete(self,ok,results,error):
  if self.mode=='miss':
   ok,results,error=False,None,'RC push-read failed: remote endpoint disconnected before completion; native operation settled unsuccessfully; retry refused'
   self.stats['read_errors']+=1
  return super().complete(ok,results,error)
c=dict(run_id='cpu',client='m01',transport='dc',keys=2,length=64,root='/testops/cpu/m01',rdma_address='10.0.2.3:20980')
conn=RestartConnector()
for i in range(2):
 key='m01-'+str(i);conn.values[key]=kv_smoke.exact_helper('soak_native_worker').payload('cpu/'+key,64)
now=[1000.]
with patch.object(kv_smoke.time,'monotonic',lambda:now[0]),patch.object(kv_smoke.time,'sleep',lambda d:now.__setitem__(0,now[0]+d)):
 row=kv_smoke.read_only(conn,c,'load','dc')
conn.close();print(json.dumps(row))`
	raw, err := exec.Command("python3", "-B", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("actual CLI: %v %s", err, raw)
	}
	frozen, err := os.ReadFile("testdata/restart-budget-monotonic.json")
	if err != nil {
		t.Fatal(err)
	}
	var a, b any
	if json.Unmarshal(raw, &a) != nil || json.Unmarshal(frozen, &b) != nil || !reflect.DeepEqual(a, b) {
		t.Fatal("new producer differs from exact affine conversion of retained trace")
	}
	if _, err = CheckReadTaskOutput(captured(raw), readConfig(), "load", "dc", ReadWindow{1032.75}); err != nil {
		t.Fatal(err)
	}
}
