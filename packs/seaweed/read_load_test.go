package seaweed

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/inventory"
)

func readConfig() SmokeConfig {
	return SmokeConfig{RunID: "cpu", Client: "m01", Transport: "dc", Keys: 2, Length: 64, Root: "/testops/cpu/m01", RDMAAddress: "10.0.2.3:20980"}
}
func captured(raw []byte) tr.ExitInfo {
	return tr.ExitInfo{Settled: true, Output: tr.CapturedOutput{Complete: true, StdoutBase64: base64.StdEncoding.EncodeToString(raw), StdoutSHA256: fmt.Sprintf("%x", sha256.Sum256(raw)), StderrSHA256: fmt.Sprintf("%x", sha256.Sum256(nil)), ReadStart: time.Now(), ReadEnd: time.Now()}}
}

// Exercises the actual pack CLI functions and unchanged worker against a fake
// native boundary. No hardware, remote SSH or network is imported/executed.
func pythonReadOutput(t *testing.T, stage, route string) []byte {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 required for actual CLI CPU control")
	}
	script := `import sys,json
sys.path.insert(0,'worker')
import test_kv_smoke,test_read_load
if sys.argv[1]=='seed':
 r,events,_=test_kv_smoke.SmokeControls().run_mode('good')
 for event in events:print(json.dumps(event))
else:
 r,_=test_read_load.ReadControls().run_case('http' if sys.argv[2]=='http' else 'good',sys.argv[2])
print(json.dumps(r))`
	cmd := exec.Command(python, "-B", "-c", script, stage, route)
	raw, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("actual worker CPU: %v %s", err, raw)
	}
	return raw
}

func TestReadLoadActualWorkerToAssessor(t *testing.T) {
	for _, pair := range [][2]string{{"seed", "dc"}, {"recovery", "dc"}, {"recovery", "http"}} {
		t.Run(pair[0]+pair[1], func(t *testing.T) {
			raw := pythonReadOutput(t, pair[0], pair[1])
			if _, err := CheckReadTaskOutput(captured(raw), readConfig(), pair[0], pair[1]); err != nil {
				t.Fatal(err, string(raw))
			}
		})
	}
}
func TestReadLoadAssessorRefusesBoundariesAndMissingProof(t *testing.T) {
	original := pythonReadOutput(t, "recovery", "dc")
	for _, mode := range []string{"foreign-run", "foreign-client", "foreign-root", "wrong-path", "missing-stats", "missing-counter", "null-counter", "fallback", "copied", "write", "count-floor", "byte-floor", "short-load", "missing-exact-time", "duplicate-terminal", "duplicate-key", "bad-output", "nonzero-exit"} {
		t.Run(mode, func(t *testing.T) {
			var row map[string]any
			if err := json.Unmarshal(original, &row); err != nil {
				t.Fatal(err)
			}
			stage := "recovery"
			stats := row["after"].(map[string]any)["loader"].(map[string]any)
			switch mode {
			case "foreign-run":
				row["run_id"] = "other"
			case "foreign-client":
				row["client"] = "m02"
			case "foreign-root":
				row["root"] = "/other"
			case "wrong-path":
				row["transport"] = "http"
			case "missing-stats":
				delete(row, "before")
			case "missing-counter":
				delete(stats, "direct_dc_bytes")
			case "null-counter":
				stats["direct_rc_bytes"] = nil
			case "fallback":
				stats["http_fallback_allowed"] = true
			case "copied":
				stats["copied_reads"] = 1
			case "write":
				stats["write_bytes"] = 1
			case "count-floor":
				stats["read_objects"] = 1
				stats["direct_dc_reads"] = 1
			case "byte-floor":
				stats["read_bytes"] = 127
				stats["direct_dc_bytes"] = 127
			case "short-load":
				stage = "load"
				row["stage"] = "load"
				row["elapsed"] = 119.999
			case "missing-exact-time":
				delete(row, "first_exact")
			}
			raw, _ := json.Marshal(row)
			if mode == "duplicate-terminal" {
				raw = append(append(raw, '\n'), original...)
			}
			if mode == "duplicate-key" {
				raw = bytes.Replace(raw, []byte(`"status":"OBSERVED"`), []byte(`"status":"OBSERVED","status":"OBSERVED"`), 1)
			}
			e := captured(raw)
			if mode == "bad-output" {
				e.Output.StdoutSHA256 = "wrong"
			}
			if mode == "nonzero-exit" {
				e.Code = 1
			}
			if _, err := CheckReadTaskOutput(e, readConfig(), stage, "dc"); err == nil {
				t.Fatal("probe qualified")
			}
		})
	}
}

func TestReadSeedWrongDigestAndIncompleteACKRefuse(t *testing.T) {
	original := pythonReadOutput(t, "seed", "dc")
	for _, mode := range []string{"digest", "fid", "missing-row", "sequence"} {
		t.Run(mode, func(t *testing.T) {
			lines := bytes.Split(bytes.TrimSpace(original), []byte{'\n'})
			var row map[string]any
			json.Unmarshal(lines[0], &row)
			switch mode {
			case "digest":
				row["sha256"] = "wrong"
			case "fid":
				row["fids"] = []string{"1,"}
			case "sequence":
				row["sequence"] = 0
			case "missing-row":
				lines = lines[1:]
			}
			if mode != "missing-row" {
				lines[0], _ = json.Marshal(row)
			}
			if _, err := CheckReadTaskOutput(captured(bytes.Join(lines, []byte{'\n'})), readConfig(), "seed", "dc"); err == nil {
				t.Fatal("bad seed qualified")
			}
		})
	}
}

func TestReadLoadArgsRefuseUncutModes(t *testing.T) {
	var d inventory.Document
	for _, pair := range [][2]string{{"seed", "http"}, {"load", "rc"}, {"new-mode", "dc"}} {
		if _, err := ReadLoadArgs(&d, "m01", SmokeSettings{}, pair[0], pair[1]); err == nil {
			t.Fatal("uncut mode admitted")
		}
	}
}

func TestReadLoadArgsKeepOneJoinedNamespace(t *testing.T) {
	raw, err := os.ReadFile("../../examples/inventory/dc.resolved.json")
	if err != nil {
		t.Fatal(err)
	}
	d, err := inventory.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	client := ""
	for _, h := range d.Hosts {
		if contains(h.Roles, "client") {
			client = h.ID
			break
		}
	}
	s := SmokeSettings{NativeDir: "/opt/seaweed/native", HelperDir: "/opt/seaweed/worker", Keys: 2, Length: 64}
	var first string
	for _, pair := range [][2]string{{"seed", "dc"}, {"load", "dc"}, {"load", "http"}, {"recovery", "dc"}, {"recovery", "http"}} {
		args, err := ReadLoadArgs(d, client, s, pair[0], pair[1])
		if err != nil {
			t.Fatal(err)
		}
		if len(args) != 8 || args[4] != "--stage" || args[5] != pair[0] || args[6] != "--read-path" || args[7] != pair[1] {
			t.Fatal(args)
		}
		if first == "" {
			first = args[3]
		} else if args[3] != first {
			t.Fatal("stage/path changed the key namespace")
		}
	}
}

func TestReadLoadCompletedFailuresAreObservationsOnly(t *testing.T) {
	raw := pythonReadOutput(t, "recovery", "dc")
	var row map[string]any
	json.Unmarshal(raw, &row)
	start := row["started"].(float64)
	row["stage"] = "load"
	row["elapsed"] = 120.0
	row["ended"] = start + 120
	// Synthetic timing only; payload/counters come from the real CPU worker path.
	row["attempts"] = 3
	failure := map[string]any{"action": "get", "key": "m01-0", "operation_id": 2, "transport": "dc", "target": readConfig().RDMAAddress, "started": start + 40, "ended": start + 41, "ok": false, "results": nil, "error": "completed transport failure"}
	row["faults"] = []any{failure}
	row["after"].(map[string]any)["read_errors"] = 1
	original, _ := json.Marshal(row)
	observed, err := CheckReadTaskOutput(captured(original), readConfig(), "load", "dc")
	if err != nil {
		t.Fatal(err)
	}
	if _, credited := observed["expected_errors"]; credited {
		t.Fatal("failure received automatic fault credit")
	}
	for _, mode := range []string{"foreign-target", "set", "foreign-key", "foreign-path", "after-end", "duplicate-operation", "missing-ok", "successful-operation"} {
		t.Run(mode, func(t *testing.T) {
			var probe map[string]any
			json.Unmarshal(original, &probe)
			f := probe["faults"].([]any)[0].(map[string]any)
			switch mode {
			case "foreign-target":
				f["target"] = "10.0.3.3:20980"
			case "set":
				f["action"] = "set"
			case "foreign-key":
				f["key"] = "not-seeded"
			case "foreign-path":
				f["transport"] = "http"
			case "after-end":
				f["ended"] = start + 121
			case "missing-ok":
				delete(f, "ok")
			case "successful-operation":
				f["ok"] = true
				f["results"] = nil
			case "duplicate-operation":
				probe["faults"] = []any{f, f}
				probe["attempts"] = 4
			}
			b, _ := json.Marshal(probe)
			if _, err := CheckReadTaskOutput(captured(b), readConfig(), "load", "dc"); err == nil {
				t.Fatal("unbound failure qualified")
			}
		})
	}
}

// A cloud HTTP inventory seeds, loads and recovers over the HTTP loader only.
func TestReadLoadHTTPInventory(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 required for actual CLI CPU control")
	}
	script := `import sys,json
sys.path.insert(0,'worker')
import test_kv_smoke,test_read_load
if sys.argv[1]=='seed':
 r,events,_=test_kv_smoke.SmokeControls().run_mode('explicit-http','http')
 for event in events:print(json.dumps(event))
else:
 r,_=test_read_load.ReadControls().run_case('http','http','recovery','http')
print(json.dumps(r))`
	c := readConfig()
	c.Transport, c.RDMAAddress = "http", ""
	for _, stage := range []string{"seed", "recovery"} {
		raw, err := exec.Command(python, "-B", "-c", script, stage).CombinedOutput()
		if err != nil {
			t.Fatalf("actual worker CPU: %v %s", err, raw)
		}
		if _, err = CheckReadTaskOutput(captured(raw), c, stage, "http"); err != nil {
			t.Fatal(stage, err, string(raw))
		}
		if _, err = CheckReadTaskOutput(captured(raw), c, stage, "dc"); err == nil {
			t.Fatal(stage, "DC route accepted for an HTTP inventory")
		}
	}
}
