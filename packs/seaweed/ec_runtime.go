package seaweed

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	tr "github.com/pingqiu/sw-test-runner"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"
)

// EC1aPlan names independently approved jobs, never commands, PIDs or credentials.
// Each S2 row adds one killed holder to the preceding loss set.
type EC1aPlan struct {
	Services                                                []string `json:"services"`
	Seed, Encode, Capture, WrongByte, Ready, Implementation string
	S1                                                      []string   `json:"s1"`
	S2                                                      [][]string `json:"s2"`
	Episodes                                                []string   `json:"episodes"`
	Keys                                                    int        `json:"keys"`
}
type ecSnapshot struct {
	Manifest       EC1aManifest `json:"manifest"`
	OriginalAbsent bool         `json:"original_absent"`
	Holders        []struct {
		Job   string
		PID   int
		Start uint64 `json:"starttime"`
		Boot  string `json:"boot_id"`
	} `json:"holders"`
	Shards []struct {
		Volume      string
		Shard       int
		Job, SHA256 string
		Bytes       int64
	} `json:"shards"`
	Files []struct {
		Job, Name, SHA256 string
		Bytes             int64
	} `json:"files"`
}

func ecTask(ctx context.Context, a *tr.ActionContext, label string, v any) error {
	_, e, err := a.RunApprovedPackJob(ctx, label, false)
	if err != nil {
		return err
	}
	raw, _, err := e.Output.Decode()
	if err != nil {
		return err
	}
	return json.Unmarshal(bytes.TrimSpace(raw), v)
}
func ecRead(ctx context.Context, a *tr.ActionContext, label, route string, m EC1aManifest) error {
	var body struct {
		RunID    string `json:"run_id"`
		Wrong    int
		Outcomes []struct {
			chaosOp
			Route          string
			Status, Length int
			SHA256         string
		}
	}
	if err := ecTask(ctx, a, label, &body); err != nil {
		return err
	}
	if body.RunID != m.RunID || body.Wrong != 0 {
		return errors.New("EC read scope/wrong bytes")
	}
	rows := make([]EC1aRead, len(body.Outcomes))
	for i, v := range body.Outcomes {
		if v.Route != route {
			return errors.New("EC route mismatch")
		}
		rows[i] = EC1aRead{v.chaosOp, v.Route, v.Status, v.Length, v.SHA256}
	}
	_, err := AssessEC1a(m, rows, 30)
	return err
}

// Bind every EC leaf to the installed and independently scanned pack source.
func ecPin(in tr.PackInputs, label, name string) error {
	j := in.Jobs[label]
	body, e := workerFiles.ReadFile("worker/" + name)
	if e != nil || len(j.Argv) < 3 || j.Argv[0] != "-I" || !strings.HasSuffix(j.Argv[1], "/worker/"+name) || in.Files["m02"][j.Argv[1]] != hash(body) || in.Files["m02"][j.Executable] != j.SHA256 {
		return errors.New("EC helper/interpreter pin differs")
	}
	arg := j.Argv[2]
	if name == "ec_chaos.py" {
		if len(j.Argv) < 4 || j.Argv[2] != "--config" {
			return errors.New("EC leaf config absent")
		}
		arg = j.Argv[3]
	}
	var c struct {
		Root, Role, Exe, SHA256, Weed, Stage string
		Holders                              map[string]string
	}
	if json.Unmarshal([]byte(arg), &c) != nil || c.Root != in.Binding.OutputDir || (name == "ec_service.py" && (c.Role != label || len(c.SHA256) != 64 || in.Files["m02"][c.Exe] != c.SHA256)) || (name == "ec_setup.py" && c.Stage != "ready" && len(in.Files["m02"][c.Weed]) != 64) {
		return errors.New("EC helper not bound to owned run root/role")
	}
	for label, dir := range c.Holders {
		if dir != filepath.Join(c.Root, label) {
			return errors.New("EC holder directory binding differs")
		}
	}
	for _, raw := range in.Scripts {
		if hash(raw) == hash(body) {
			return nil
		}
	}
	return errors.New("EC helper absent from independent script scan")
}

func ecStage1(ctx context.Context, a *tr.ActionContext, _ tr.Action) (out map[string]string, err error) {
	defer func() { a.FailApprovedPack(err) }()
	in, err := a.ApprovedPackInputs()
	if err != nil {
		return nil, err
	}
	b := in.Binding
	if b.Workload != "s3" || b.Transport != "http" || len(b.Hosts) != 1 || b.Hosts[0].Node != "m02" || b.HardDeadline.Sub(time.Now()) > 10*time.Minute || a.Bundle == nil {
		return nil, errors.New("EC requires admitted M02-only S3 HTTP ten-minute scope and bundle")
	}
	var p EC1aPlan
	raw := a.Scenario.Metadata["seaweed_ec_plan"]
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if len(raw) > 65536 || len(a.Scenario.Phases) != 1 || len(a.Scenario.Phases[0].Actions) != 1 || a.Scenario.Phases[0].Actions[0].Action != "seaweed_ec_stage1" || a.Scenario.Phases[0].Parallel || a.Scenario.Phases[0].Repeat > 1 || a.Scenario.Phases[0].Actions[0].Retry != 0 || a.Scenario.Phases[0].Actions[0].IgnoreError {
		return nil, errors.New("EC plan too large")
	}
	if err = dec.Decode(&p); err != nil {
		return nil, err
	}
	if (p.Implementation != "go" && p.Implementation != "rust") || p.Keys < 1 || p.Keys > 128 || len(p.Services) < 5 || len(p.Services) > 17 || len(p.S1) != 2 || len(p.S2) != 4 || len(p.Episodes) != 4 {
		return nil, errors.New("EC Stage1 shape")
	}
	seen := map[string]bool{}
	labels := append(append([]string{}, p.Services...), p.Seed, p.Encode, p.Capture, p.WrongByte, p.Ready)
	labels = append(labels, p.S1...)
	for _, pair := range p.S2 {
		if len(pair) != 2 {
			return nil, errors.New("two EC routes required")
		}
		labels = append(labels, pair...)
	}
	for _, label := range labels {
		job, ok := in.Jobs[label]
		if !ok || job.Node != "m02" || seen[label] {
			return nil, errors.New("unapproved/reused EC job")
		}
		seen[label] = true
	}
	if !seen["master"] || !seen["filer"] || !seen["s3"] {
		return nil, errors.New("EC metadata services missing")
	}
	for _, episode := range p.Episodes {
		g, ok := in.Restarts[episode]
		if !ok || !g.Hold || !contains(p.Services, g.Victim) || g.Victim == "master" || g.Victim == "filer" || g.Victim == "s3" {
			return nil, errors.New("EC holder loss lacks held approved generation")
		}
	}
	for _, label := range labels {
		name := "ec_chaos.py"
		if contains(p.Services, label) {
			name = "ec_service.py"
		}
		if label == p.Capture {
			name = "ec_capture.py"
		}
		if label == p.Ready || label == p.Encode {
			name = "ec_setup.py"
		}
		if err = ecPin(in, label, name); err != nil {
			return nil, err
		}
		if name == "ec_chaos.py" && strings.Contains(strings.Join(in.Jobs[label].Argv, " "), "--plant-wrong-byte") != (label == p.WrongByte) {
			return nil, errors.New("wrong-byte planting escaped its control job")
		}
	}
	body, _ := workerFiles.ReadFile("worker/chaos_load.py")
	for _, label := range append(p.S1, p.Seed) {
		sibling := filepath.Join(filepath.Dir(in.Jobs[label].Argv[1]), "chaos_load.py")
		if in.Files["m02"][sibling] != hash(body) {
			return nil, errors.New("EC reused chaos payload source differs")
		}
	}
	handles := map[string]tr.JobHandle{}
	for _, label := range p.Services {
		h, _, e := a.RunApprovedPackJob(ctx, label, true)
		if e != nil {
			return nil, e
		}
		handles[label] = h
	}
	var ready any
	if err = ecTask(ctx, a, p.Ready, &ready); err != nil {
		return nil, err
	}
	var seed EC1aManifest
	if err = ecTask(ctx, a, p.Seed, &seed); err != nil {
		return nil, err
	}
	if seed.RunID != b.RunID || len(seed.Objects) != p.Keys {
		return nil, errors.New("EC seed count/run mismatch")
	}
	for _, obj := range seed.Objects {
		block := sha256.Sum256([]byte(b.RunID + "/" + obj.Key))
		want := sha256.New()
		for i := 0; i < 8192; i++ {
			want.Write(block[:])
		}
		if obj.Length != 262144 || hex.EncodeToString(want.Sum(nil)) != obj.SHA256 {
			return nil, errors.New("EC seed digest not independently generated")
		}
	}
	if _, _, err = a.RunApprovedPackJob(ctx, p.Encode, false); err != nil {
		return nil, err
	}
	var snap ecSnapshot
	if err = ecTask(ctx, a, p.Capture, &snap); err != nil {
		return nil, err
	}
	if !snap.OriginalAbsent || !reflect.DeepEqual(seed.Objects, snap.Manifest.Objects) || snap.Manifest.RunID != b.RunID || snap.Manifest.Generation == "" || len(snap.Holders) != len(p.Services)-3 || len(snap.Shards) > 256 || len(snap.Files) == 0 {
		return nil, errors.New("EC frozen capture invalid")
	}
	holders := map[string]bool{}
	for _, h := range snap.Holders {
		if holders[h.Job] || h.Job == "master" || h.Job == "filer" || h.Job == "s3" || !contains(p.Services, h.Job) || h.PID <= 0 || h.Start == 0 || h.Boot != in.Boots["m02"] {
			return nil, errors.New("EC holder capture invalid")
		}
		holders[h.Job] = true
	}
	shardSet := map[string]map[int]bool{}
	for _, s := range snap.Shards {
		if !holders[s.Job] || s.Shard < 0 || s.Shard >= 14 || len(s.SHA256) != 64 || s.Bytes <= 0 {
			return nil, errors.New("EC shard capture invalid")
		}
		if shardSet[s.Volume] == nil {
			shardSet[s.Volume] = map[int]bool{}
		}
		if shardSet[s.Volume][s.Shard] {
			return nil, errors.New("EC stage1a duplicate holder placement unsupported")
		}
		shardSet[s.Volume][s.Shard] = true
	}
	for _, o := range snap.Manifest.Objects {
		if len(shardSet[strings.Split(o.FID, ",")[0]]) != 14 {
			return nil, errors.New("EC full14 shard set missing")
		}
	}
	report := map[string]any{"implementation": p.Implementation, "s1": "NOT_RUN", "s2": "NOT_COVERED", "exposure": "NOT_OBSERVED", "capture": snap, "reads": []string{}, "losses": []tr.GenerationRestartReceipt{}}
	save := func() error {
		raw, e := json.MarshalIndent(report, "", "  ")
		if e != nil {
			return e
		}
		return os.WriteFile(filepath.Join(a.Bundle.ArtifactsDir(), "ec-stage1.json"), raw, 0600)
	}
	defer func() { err = errors.Join(err, save()) }()
	var planted struct {
		Control string `json:"control"`
	}
	if err = ecTask(ctx, a, p.WrongByte, &planted); err != nil {
		return nil, err
	}
	if planted.Control != "WRONG_BYTE_REJECTED" {
		return nil, errors.New("planted wrong byte not rejected")
	}
	report["wrong_byte_control"] = planted.Control
	for i, job := range p.S1 {
		if err = ecRead(ctx, a, job, []string{"s3", "volume"}[i], snap.Manifest); err != nil {
			return nil, err
		}
	}
	report["s1"] = "PASS"
	if err = save(); err != nil {
		return nil, err
	}
	lost := map[string]bool{}
	for i, episode := range p.Episodes {
		g := in.Restarts[episode]
		if !holders[g.Victim] || lost[g.Victim] {
			return nil, errors.New("EC repeated/foreign holder loss")
		}
		_, receipt, e := a.RestartApprovedPackGeneration(ctx, episode, handles[g.Victim])
		if e != nil {
			return nil, e
		}
		for _, h := range snap.Holders {
			if h.Job == g.Victim && (h.PID != receipt.Kill.Identity.PID || h.Start != receipt.Kill.Identity.StartTime || h.Boot != receipt.Kill.Identity.BootID) {
				return nil, errors.New("EC frozen holder generation differs")
			}
		}
		lost[g.Victim] = true
		unavailable := map[string]bool{}
		for _, sh := range snap.Shards {
			if lost[sh.Job] {
				unavailable[fmt.Sprintf("%s/%d", sh.Volume, sh.Shard)] = true
			}
		}
		if len(unavailable) != i+1 {
			return nil, errors.New("EC S2 requires exactly one additional unavailable shard per episode")
		}
		report["losses"] = append(report["losses"].([]tr.GenerationRestartReceipt), receipt)
		if err = save(); err != nil {
			return nil, err
		}
		for route, job := range p.S2[i] {
			if err = ecRead(ctx, a, job, []string{"s3", "volume"}[route], snap.Manifest); err != nil {
				return nil, err
			}
			report["reads"] = append(report["reads"].([]string), job)
		}
	}
	report["mandatory_bytes_deadlines"] = "PASS"
	return map[string]string{"s1": "PASS", "s2": "NOT_COVERED", "exposure": "NOT_OBSERVED", "value": "S1 exact; S2 bytes/deadlines observed, product reconstruction marker absent"}, nil
}
