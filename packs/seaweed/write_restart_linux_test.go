//go:build linux

package seaweed

import (
	"context"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"testing"

	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/inventory"
)

func masterFixture(t *testing.T, mode string) (*cpuFixture, *restartCPU) {
	f, n := restartFixture(t, mode)
	n.role = "master"
	f.plan.Restart.Role, f.plan.Restart.Episode, f.plan.Restart.Writes = "master", "master-once", true
	for label, job := range f.policy.Jobs {
		if strings.HasSuffix(job.Argv[1], "lab_profile.py") {
			setFilerVerbose(t, &job, true)
			f.policy.Jobs[label] = job
		}
	}
	d, err := inventory.Decode([]byte(f.plan.Resolved))
	if err != nil {
		t.Fatal(err)
	}
	args, err := ReadLoadArgs(d, "m01", f.plan.Smoke, "write-load", "dc")
	if err != nil {
		t.Fatal(err)
	}
	j := f.policy.Jobs["read-dc"]
	j.Argv = args
	f.policy.Jobs["read-dc"] = j
	replacement := f.policy.Jobs["deploy-master"]
	replacement.Argv = append(append([]string{}, replacement.Argv...), "--replacement-master")
	delete(f.policy.Jobs, "replacement-volume")
	f.policy.Jobs["replacement-master"] = replacement
	f.policy.GenerationRestarts = map[string]tr.GenerationRestartPlan{"master-once": {Victim: "deploy-master", Replacement: "replacement-master"}}
	f.publish(t, nil)
	return f, n
}

func setFilerVerbose(t *testing.T, job *tr.JobPin, on bool) {
	t.Helper()
	var config ProfileConfig
	if err := json.Unmarshal([]byte(job.Argv[3]), &config); err != nil {
		t.Fatal(err)
	}
	config.FilerVerbose = on
	raw, _ := json.Marshal(config)
	job.Argv = append([]string{}, job.Argv...)
	job.Argv[3] = string(raw)
}

// writeLoadRows is the worker's stdout around the fake's kill: one SET every 10 s
// from 30 s before it, the first one after the kill failing.
func writeLoadRows(c SmokeConfig, kill float64, mode string) []byte {
	out := []byte{}
	row := func(v map[string]any) {
		b, _ := json.Marshal(v)
		out = append(append(out, b...), '\n')
	}
	stats := map[string]any{"loader": map[string]any{"transport": "rdma"}}
	sets, acked := 10, 0
	for i := 0; i < sets; i++ {
		start := kill - 30 + float64(i)*10
		ok := i != 3
		if ok {
			acked++
		}
		id := i
		if mode == "unbound-set" && i == 4 {
			id = 7
		}
		errText := ""
		if !ok {
			errText = "no writable volumes"
		}
		row(map[string]any{"event": "SET", "operation_id": id, "key": c.Client + "-w" + strconv.Itoa(i), "started": start, "ended": start + 1, "ok": ok, "error": errText})
	}
	row(map[string]any{"event": "GET", "operation_id": 0, "key": c.Client + "-w0", "started": kill + 5, "ended": kill + 8, "ok": true, "error": ""})
	end := map[string]any{"status": "OBSERVED", "code": "OK", "stage": "write-load", "transport": "dc", "run_id": c.RunID, "client": c.Client, "root": c.Root, "sets": sets, "gets": 1, "acked": acked, "failed": sets - acked, "readback_exact": acked, "readback_missing": 0, "started": kill - 30, "ended": kill + 90, "before": stats, "after": stats}
	row(end)
	return out
}

func TestMasterWritesThroughApprovedEngine(t *testing.T) {
	for _, mode := range []string{"positive", "unbound-set", "missing-ready", "wrong-product", "load-exit"} {
		t.Run(mode, func(t *testing.T) {
			f, n := masterFixture(t, mode)
			r, _, err := tr.RunApprovedRequest(context.Background(), f.request, f.profile, registerFastRestart, &f.runtime, nil)
			if err != nil {
				t.Fatal(err)
			}
			want := tr.StatusFail
			if mode == "positive" {
				want = tr.StatusPass
			}
			if r == nil || r.Status != want {
				t.Fatalf("mode=%s result=%+v events=%v", mode, r, n.events)
			}
			if len(n.live) != 0 {
				t.Fatal("owned generations remain")
			}
			if mode != "positive" {
				return
			}
			got := strings.Join(n.events, ",")
			if !strings.Contains(got, "http-control,kill,ready,ready,write-load-wait,dc-recovery,http-control") {
				t.Fatalf("not started before the kill and waited after READY: %s", got)
			}
			b, _ := json.Marshal(r)
			if !regexp.MustCompile(`kill_to_ready_s[\\"]*:40\.`).Match(b) {
				t.Fatalf("window not closed at the READY pass time: %s", b)
			}
			for _, s := range []string{"REPORT_ONLY", "no writable volumes"} {
				if !strings.Contains(string(b), s) {
					t.Fatalf("report missing %s in the result: %s", s, b)
				}
			}
		})
	}
}

// Each of these fixtures is otherwise exact, so the role/writes gate alone refuses it.
func TestWritesGateIsTheOnlyRefusal(t *testing.T) {
	for _, mode := range []string{"master-read-load", "volume-writes"} {
		t.Run(mode, func(t *testing.T) {
			var f *cpuFixture
			var n *restartCPU
			stage := "load"
			quiet := mode == "master-read-load"
			if quiet {
				f, n = masterFixture(t, "positive")
				f.plan.Restart.Writes = false
			} else {
				f, n = restartFixture(t, "positive")
				f.plan.Restart.Writes, stage = true, "write-load"
			}
			for label, job := range f.policy.Jobs {
				if strings.HasSuffix(job.Argv[1], "lab_profile.py") {
					setFilerVerbose(t, &job, !quiet)
					f.policy.Jobs[label] = job
				}
			}
			d, err := inventory.Decode([]byte(f.plan.Resolved))
			if err != nil {
				t.Fatal(err)
			}
			j := f.policy.Jobs["read-dc"]
			if j.Argv, err = ReadLoadArgs(d, "m01", f.plan.Smoke, stage, "dc"); err != nil {
				t.Fatal(err)
			}
			f.policy.Jobs["read-dc"] = j
			f.publish(t, nil)
			r, _, err := tr.RunApprovedRequest(context.Background(), f.request, f.profile, registerFastRestart, &f.runtime, nil)
			if err == nil && (r == nil || r.Status != tr.StatusFail) {
				t.Fatal("role/writes mismatch passed")
			}
			if n.authorized != 0 {
				t.Fatal("effect after binding refusal")
			}
		})
	}
}

func TestMasterWritesBindingRefuses(t *testing.T) {
	for _, mode := range []string{"read-load-job", "volume-victim", "quiet-filer"} {
		t.Run(mode, func(t *testing.T) {
			f, n := masterFixture(t, "positive")
			switch mode {
			case "read-load-job":
				j := f.policy.Jobs["read-dc"]
				j.Argv = append(append([]string{}, j.Argv[:len(j.Argv)-3]...), "load", "--read-path", "dc")
				f.policy.Jobs["read-dc"] = j
			case "quiet-filer":
				j := f.policy.Jobs["deploy-filer"]
				setFilerVerbose(t, &j, false)
				f.policy.Jobs["deploy-filer"] = j
			case "volume-victim":
				ep := f.policy.GenerationRestarts["master-once"]
				ep.Victim = "deploy-volume"
				f.policy.GenerationRestarts["master-once"] = ep
			}
			f.publish(t, nil)
			r, _, err := tr.RunApprovedRequest(context.Background(), f.request, f.profile, registerFastRestart, &f.runtime, nil)
			if err == nil && (r == nil || r.Status != tr.StatusFail) {
				t.Fatal("invalid master writes binding passed")
			}
			if n.authorized != 0 {
				t.Fatal("effect after binding refusal")
			}
		})
	}
}
