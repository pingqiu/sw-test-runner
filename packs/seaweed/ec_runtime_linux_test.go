//go:build linux

package seaweed

import (
	"context"
	"encoding/json"
	"fmt"
	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/infra"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type ecCPU struct {
	*cpuNode
	mode   string
	seed   EC1aManifest
	killed map[string]bool
}

func (n *ecCPU) Kill(ctx context.Context, id string) (tr.KillWitness, error) {
	n.killed[id] = true
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.live, id)
	w := tr.KillWitness{Identity: n.ids[id], AliveBefore: true, Signal: "KILL", AppliedAt: time.Now()}
	if n.mode == "lost-kill" {
		return w, fmt.Errorf("lost kill reply")
	}
	return w, nil
}
func (n *ecCPU) Wait(ctx context.Context, id string) (tr.ExitInfo, error) {
	e, err := n.cpuNode.Wait(ctx, id)
	if err != nil {
		return e, err
	}
	s := n.specs[id]
	if n.killed[id] {
		e.Code, e.Signal = -1, 9
		return e, nil
	}
	if !s.CaptureOutput {
		return e, nil
	}
	var config struct{ Label string }
	arg := s.Argv[2]
	if arg == "--config" {
		arg = s.Argv[3]
	}
	_ = json.Unmarshal([]byte(arg), &config)
	label := config.Label
	var row any
	switch label {
	case "seed":
		row = n.seed
	case "capture":
		holders := []map[string]any{}
		shards := []map[string]any{}
		for j, spec := range n.specs {
			var c struct{ Label string }
			_ = json.Unmarshal([]byte(spec.Argv[2]), &c)
			if strings.HasPrefix(c.Label, "h") {
				i := n.ids[j]
				if n.mode == "holder-mismatch" {
					i.PID++
				}
				holders = append(holders, map[string]any{"job": c.Label, "pid": i.PID, "starttime": i.StartTime, "boot_id": i.BootID})
			}
		}
		for i := 0; i < 14; i++ {
			shards = append(shards, map[string]any{"volume": "1", "shard": i, "job": fmt.Sprint("h", i), "sha256": strings.Repeat("a", 64), "bytes": 1048576})
		}
		m := n.seed
		m.Generation = "encode-1"
		row = map[string]any{"manifest": m, "holders": holders, "shards": shards, "original_absent": n.mode != "original", "files": []any{map[string]any{"job": "h0", "name": "1.ecx", "sha256": strings.Repeat("a", 64), "bytes": 16}}}
	case "encode", "ready":
		row = map[string]any{"encoded": true}
	case "planted":
		row = map[string]string{"control": "WRONG_BYTE_REJECTED"}
	default:
		route := "s3"
		if strings.HasSuffix(label, "volume") {
			route = "volume"
		}
		object := n.seed.Objects[0]
		digest := object.SHA256
		if n.mode == "wrong" {
			digest = strings.Repeat("0", 64)
		}
		row = map[string]any{"run_id": n.seed.RunID, "wrong": 0, "outcomes": []any{map[string]any{"op": 0, "kind": "s3_get", "key": object.Key, "started": 1, "ended": 2, "exact": true, "route": route, "status": 200, "length": 262144, "sha256": digest}}}
	}
	if m, ok := row.(map[string]any); ok {
		if reads, ok := m["outcomes"].([]any); ok {
			if n.mode == "missing" {
				m["outcomes"] = []any{}
			}
			if n.mode == "late" {
				reads[0].(map[string]any)["ended"] = 31.001
			}
		}
	}
	raw, _ := json.Marshal(row)
	e.Output = captured(raw).Output
	return e, nil
}
func ecFixture(t *testing.T, mode string) (*cpuFixture, *ecCPU) {
	f := newCPUFixture(t)
	f.binding.Workload = "s3"
	f.binding.Transport = "http"
	f.binding.Hosts = f.binding.Hosts[1:]
	if f.binding.Hosts[0].Node != "m02" {
		t.Fatal("fixture host ordering changed")
	}
	for _, d := range f.policy.Deployments {
		if d.NodeID == "m02" {
			f.policy.Deployments = []infra.DeploymentPolicy{d}
		}
	}

	f.binding.Inventory = f.write(t, "ec-inventory.json", tr.ApprovedInventory{Version: "s0b-inventory-v1", Hosts: f.binding.Hosts})
	f.binding.Artifacts = f.write(t, "ec-artifacts.json", tr.ApprovedArtifacts{Version: "s0b-artifacts-v1", Deployments: f.policy.Deployments})
	f.policy.Jobs = map[string]tr.JobPin{}
	f.policy.GenerationRestarts = map[string]tr.GenerationRestartPlan{}
	add := func(label string) {
		f.policy.Jobs[label] = tr.JobPin{Node: "m02", Executable: "/opt/seaweed/product", SHA256: strings.Repeat("d", 64), Argv: []string{label}}
	}
	plan := EC1aPlan{Implementation: "rust", Seed: "seed", Encode: "encode", Capture: "capture", WrongByte: "planted", Ready: "ready", Keys: 1, S1: []string{"s1-s3", "s1-volume"}}
	for _, label := range []string{"master", "filer", "s3"} {
		add(label)
		plan.Services = append(plan.Services, label)
	}
	for i := 0; i < 14; i++ {
		label := fmt.Sprint("h", i)
		add(label)
		plan.Services = append(plan.Services, label)
	}
	for _, label := range []string{"ready", "seed", "encode", "capture", "planted", "s1-s3", "s1-volume"} {
		add(label)
	}
	for i := 0; i < 4; i++ {
		ep := fmt.Sprint("loss", i)
		rep := fmt.Sprint("replacement", i)
		add(rep)
		f.policy.GenerationRestarts[ep] = tr.GenerationRestartPlan{Victim: fmt.Sprint("h", i), Replacement: rep, Hold: true}
		plan.Episodes = append(plan.Episodes, ep)
		pair := []string{fmt.Sprint("s2-", i, "-s3"), fmt.Sprint("s2-", i, "-volume")}
		for _, label := range pair {
			add(label)
		}
		plan.S2 = append(plan.S2, pair)
	}

	for _, name := range []string{"ec_service.py", "ec_chaos.py", "ec_capture.py", "ec_setup.py", "chaos_load.py"} {
		body, e := workerFiles.ReadFile("worker/" + name)
		if e != nil {
			t.Fatal(e)
		}
		path := "/opt/seaweed/worker/" + name
		f.policy.Deployments[0].Files = append(f.policy.Deployments[0].Files, infra.DeploymentFile{Path: path, SHA256: hash(body), Size: int64(len(body)), Roles: []string{"adapter"}})
		f.policy.Scripts = append(f.policy.Scripts, f.bytes(t, "ec-"+name, body))
	}
	for label, j := range f.policy.Jobs {
		name := "ec_chaos.py"
		if contains(plan.Services, label) {
			name = "ec_service.py"
		}
		if label == "encode" || label == "ready" {
			name = "ec_setup.py"
		}
		if label == "capture" {
			name = "ec_capture.py"
		}
		cfg, _ := json.Marshal(map[string]string{"Label": label, "role": label, "root": f.binding.OutputDir, "exe": "/opt/seaweed/product", "sha256": strings.Repeat("d", 64), "weed": "/opt/seaweed/product", "stage": label})
		j.Argv = []string{"-I", "/opt/seaweed/worker/" + name, string(cfg)}
		if name == "ec_chaos.py" {
			j.Argv = []string{"-I", "/opt/seaweed/worker/" + name, "--config", string(cfg)}
		}
		if label == "planted" {
			j.Argv = append(j.Argv, "--plant-wrong-byte")
		}
		f.policy.Jobs[label] = j
	}
	f.binding.Artifacts = f.write(t, "ec-artifacts.json", tr.ApprovedArtifacts{Version: "s0b-artifacts-v1", Deployments: f.policy.Deployments})
	f.editScenario = func(s *tr.Scenario) {
		raw, _ := json.Marshal(plan)
		s.Metadata = map[string]string{"seaweed_ec_plan": string(raw)}
		s.Phases = []tr.Phase{{Name: "ec", Actions: []tr.Action{{Action: "seaweed_ec_stage1"}}}}
	}
	n := &ecCPU{cpuNode: f.node, mode: mode, killed: map[string]bool{}, seed: EC1aManifest{RunID: f.binding.RunID, Objects: []EC1aObject{{Key: "ec-0", FID: "1,abcdef12345678", Length: 262144, SHA256: "55e6c21379e879882f4d8e4b0347c772a3445c59ac53b1277b4f1de481f65b8a", ACK: true}}}}
	if mode == "helper-pin" {
		j := f.policy.Jobs["seed"]
		j.Argv[1] = "/opt/seaweed/worker/not-the-leaf.py"
		f.policy.Jobs["seed"] = j
	}
	f.runtime.Jobs = n
	f.publish(t, map[string]string{"m02": "/service/private-key"})
	return f, n
}

func TestECStage1ThroughApprovedEngine(t *testing.T) {
	for _, mode := range []string{"positive", "wrong", "original", "missing", "late", "holder-mismatch", "lost-kill", "helper-pin"} {
		t.Run(mode, func(t *testing.T) {
			f, n := ecFixture(t, mode)
			result, bundle, err := tr.RunApprovedRequest(context.Background(), f.request, f.profile, RegisterPack, &f.runtime, nil)
			if err != nil {
				t.Fatal(err)
			}
			want := tr.StatusFail
			if mode == "positive" {
				want = tr.StatusPass
			}
			if result == nil || result.Status != want {
				t.Fatalf("result%+v bundle%s", result, bundle)
			}
			if mode == "positive" {
				raw, e := os.ReadFile(filepath.Join(bundle, "artifacts/ec-stage1.json"))
				if e != nil {
					t.Fatal(e)
				}
				var row map[string]any
				if json.Unmarshal(raw, &row) != nil || row["s1"] != "PASS" || row["s2"] != "NOT_COVERED" || row["mandatory_bytes_deadlines"] != "PASS" || len(n.killed) != 4 {
					t.Fatalf("invalid Stage1 result %s", raw)
				}
			}
		})
	}
}
