//go:build linux

package seaweed

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pingqiu/sw-test-runner/inventory"
)

// The stage-3 chain through its real callers: the Go-admitted assessor argv runs the real soak_assess.py on
// row evidence (the Python fixture's), and Go decodes and integrates the row it prints.
func TestTheAdmittedRowAssessorChain(t *testing.T) {
	helpers, _ := filepath.Abs("worker")
	bootRaw, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		t.Fatal(err)
	}
	boot := strings.TrimSpace(string(bootRaw))
	for name, c := range map[string]struct {
		kind, edit, status, causal string
	}{
		"F3 witnessed":                  {KindLinkFlap, "None", "PASS", "PASS"},
		"F5 without a new registration": {KindMRPressure, "t.no_registration", "NOT_COVERED", "NOT_COVERED"},
		"R0 clean":                      {KindControl, "None", "PASS", "PASS"},
	} {
		root := t.TempDir()
		write := exec.Command("python3", "-B", "-c", "import sys, test_soak_fault_row as t; t.write_row(sys.argv[1], sys.argv[2], "+c.edit+")", root, c.kind)
		write.Dir = helpers
		if out, err := write.CombinedOutput(); err != nil {
			t.Fatalf("%s: row evidence: %v %s", name, err, out)
		}
		d := &inventory.Document{RunID: "run-1", Hosts: []inventory.Host{{ID: "m01", RunRoot: root}},
			Observation: &inventory.Observation{Hosts: []inventory.HostObservation{{HostID: "m01", BootID: boot}, {HostID: "m02", BootID: "m02-boot"}}}}
		x := rowPlan(c.kind)
		x.Row.FreeBelowBytes = 1 << 20
		x.Row.ClientFallbackCounters = []string{"rdma_fallbacks"}
		argv, err := SoakAssessArgs(d, SmokeSettings{HelperDir: helpers}, x)
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("python3", argv...)
		cmd.Dir = t.TempDir()
		raw, err := cmd.Output()
		if err != nil {
			t.Fatalf("%s: admitted argv failed: %v", name, err)
		}
		v, err := decodeVerdict(captured(raw), SoakBinding{SoakVerdictSchema, "run-1", boot, root}, x.criteria())
		if err != nil || v.Status != c.status || v.Criteria["causal"].Outcome != c.causal || v.LayoutScope != rowLayoutScope {
			t.Fatalf("%s: got %+v %v\n%s", name, v, err, raw)
		}
	}
}

// The load is gated on a real pre-load sample: the script the runner runs on M02, against rows written by the
// sampler's own serializer (Python json.dumps, sort_keys), accepts only a pre_load row, never a request or later phase.
func TestTheLoadWaitsForARealPreloadSample(t *testing.T) {
	dir := t.TempDir()
	for name, c := range map[string]struct {
		phases []string
		ok     bool
	}{"pre-load row": {[]string{"pre_load"}, true}, "no file": {nil, false}, "only load rows": {[]string{"load", "quiescence"}, false}} {
		file := filepath.Join(dir, strings.ReplaceAll(name, " ", "-")+".jsonl")
		if c.phases != nil {
			code := "import json, sys\nwith open(sys.argv[1], 'w') as f:\n    for p in sys.argv[2:]:\n        f.write(json.dumps(dict(seq=0, mono=1.5, phase=p, identity=dict(pid=7, starttime=9)), sort_keys=True) + '\\n')\n"
			if out, err := exec.Command("python3", append([]string{"-c", code, file}, c.phases...)...).CombinedOutput(); err != nil {
				t.Fatalf("%s: %v %s", name, err, out)
			}
		}
		err := exec.Command("bash", "-c", preloadWitnessScript(file, 2)).Run()
		if (err == nil) != c.ok {
			t.Errorf("%s: err=%v, want ok=%v", name, err, c.ok)
		}
	}
}

// Profile admission refuses a row whose load can write more than one object size (the soak's own mix), with
// the injector's bytes pinned so that nothing else can be the reason; the same row on kv_load builds.
func TestProfileAdmissionRefusesAMixedSizeRow(t *testing.T) {
	for _, kind := range []string{KindVolumeKillIO, KindClientKillPut, KindControl} {
		for _, fixed := range []bool{false, true} {
			input := soakInput(t, 30*time.Minute, func(x *SoakPlan) {
				row := rowPlan(kind)
				x.DurationS, x.Faults, x.Row, x.ClientKillAtS, x.ReadyAssign, x.Pattern = row.DurationS, row.Faults, row.Row, 0, nil, nil
				if fixed {
					x.Pattern = row.Pattern
				}
			})
			for _, name := range []string{"soak_fault.py", "soak_stats.py"} {
				body, _ := workerFiles.ReadFile("worker/" + name)
				input.Files["m02"]["/opt/profile/worker/"+name] = hash(body)
			}
			_, err := BuildProfile(input)
			if fixed && err != nil || !fixed && (err == nil || !strings.Contains(err.Error(), "more than one object size")) {
				t.Errorf("%s fixed=%v: err=%v", kind, fixed, err)
			}
		}
	}
}

// A row's profile pins the injector, sampler and markers on M02 from the joined inventory, requires the
// injector's exact bytes there, and restarts nothing for an injected kind.
func TestARowProfilePinsItsFaultJobs(t *testing.T) {
	input := soakInput(t, 30*time.Minute, func(x *SoakPlan) {
		row := rowPlan(KindLinkFlap)
		x.DurationS, x.Faults, x.Row, x.ClientKillAtS, x.ReadyAssign, x.Pattern = row.DurationS, row.Faults, row.Row, 0, nil, row.Pattern
	})
	if _, err := BuildProfile(input); err == nil || !strings.Contains(err.Error(), "soak_fault.py") {
		t.Fatalf("a row profile without the injector's pinned bytes must refuse: %v", err)
	}
	for _, name := range []string{"soak_fault.py", "soak_stats.py"} {
		body, _ := workerFiles.ReadFile("worker/" + name)
		input.Files["m02"]["/opt/profile/worker/"+name] = hash(body)
	}
	b, err := BuildProfile(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Restarts) != 0 {
		t.Fatalf("F3 restarts nothing: %+v", b.Restarts)
	}
	for _, action := range soakFaultActions {
		j, ok := b.Jobs["fault-"+action+"-m02"]
		if !ok || j.Node != "m02" || j.Argv[len(j.Argv)-1] != action {
			t.Fatalf("job fault-%s-m02: %+v", action, j)
		}
	}
	var config map[string]any
	argv := b.Jobs["fault-apply-m02"].Argv
	if err = json.Unmarshal([]byte(argv[3]), &config); err != nil {
		t.Fatal(err)
	}
	for _, seam := range []string{"sys_root", "mountinfo"} { // test-only seams: the runner never sets them
		if _, ok := config[seam]; ok {
			t.Fatalf("runner config carries the test seam %s", seam)
		}
	}
	units, _ := config["units"].([]any)
	if config["kind"] != KindLinkFlap || len(units) != 1 || !strings.HasSuffix(units[0].(string), "-deploy-m02-volume") ||
		config["deadman_s"] != float64(30+120) || len(config["management_addrs"].([]any)) != 1 ||
		!strings.HasPrefix(config["volume_status_url"].(string), "http://") || !strings.HasSuffix(config["volume_status_url"].(string), "/status") {
		t.Fatalf("injector config: %v", config)
	}
	load := b.Jobs["soak-load-m01"].Argv[3]
	if !strings.Contains(load, `"fault_row":{"at_s":60,"audit_s":20,"episode":"E1","kind":"link_flap"`) {
		t.Fatalf("the load carries its row: %s", load)
	}
}
