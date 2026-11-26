package v3block

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/actions"
)

const m1Scenario = "../../scenarios/v3block/m1-random.yaml"

// m1Render is one run of the committed M1 scenario through the real engine:
// v3_run_plan is the real handler, every other action records its resolved
// params (and, for the typed actions, the configuration they would read).
type m1Render struct {
	status tr.ResultStatus
	steps  []tr.Action       // resolved, in order
	opCfg  map[string]string // v3_gen_run's resolved configuration vars
	gen    GenConfig         // v3_gen_run's parsed params
	bad    []string          // unresolved or unreadable values
	env    map[string]string // the scenario's env as parsed
}

func renderM1(t *testing.T, env map[string]string, fail string) *m1Render {
	t.Helper()
	s, err := tr.ParseFile(m1Scenario)
	if err != nil {
		t.Fatal(err)
	}
	out := &m1Render{env: map[string]string{}}
	for k, v := range s.Env {
		out.env[k] = v
	}
	s.Env["product_src"], s.Env["runner_src"], s.Env["weed"] = "/src", "/runner", "/bin/weed"
	s.Env["bundle_dir"], s.Env["artifacts_dir"] = "/bundle", "/bundle/artifacts"
	for k, v := range env {
		s.Env[k] = v
	}
	var mu sync.Mutex
	handler := func(_ context.Context, actx *tr.ActionContext, a tr.Action) (map[string]string, error) {
		mu.Lock()
		defer mu.Unlock()
		out.steps = append(out.steps, a)
		for k, v := range a.Params {
			if strings.Contains(v, "{{") {
				out.bad = append(out.bad, fmt.Sprintf("%s %s=%q", a.Action, k, v))
			}
		}
		if a.Action == "v3_gen_run" {
			out.opCfg = map[string]string{}
			for _, k := range []string{"bin_dir", "master", "filer", "chunk_path", "root", "client", "own_script", "work", "owner", "lock_dirs_test_only"} {
				v := opParam(a, actx, k)
				if v == "" || strings.Contains(v, "{{") {
					out.bad = append(out.bad, fmt.Sprintf("the typed operations' %s resolves to %q", k, v))
				}
				out.opCfg[k] = v
			}
			g, err := genConfig(a, actx)
			if err != nil {
				out.bad = append(out.bad, err.Error())
			}
			out.gen = g
		}
		if fail == "master-spawn" && a.Action == "exec" && strings.Contains(a.Params["cmd"], " spawn ") && strings.Contains(a.Params["cmd"], "blockmaster") {
			return nil, fmt.Errorf("injected: port already listens")
		}
		if a.Action == fail {
			return nil, fmt.Errorf("injected")
		}
		return map[string]string{"value": "ok"}, nil
	}
	r := tr.NewRegistry()
	for _, p := range s.Phases {
		for _, a := range p.Actions {
			if a.Action == "v3_run_plan" {
				r.RegisterFunc(a.Action, tr.TierCore, func(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
					mu.Lock()
					out.steps = append(out.steps, act)
					mu.Unlock()
					return v3RunPlan(ctx, actx, act)
				})
			} else {
				r.RegisterFunc(a.Action, tr.TierCore, handler)
			}
		}
	}
	actx := &tr.ActionContext{Scenario: s, Vars: map[string]string{}, Log: func(string, ...interface{}) {}}
	out.status = tr.NewEngine(r, nil).Run(context.Background(), s, actx).Status
	return out
}

// The scenario parses, every action it names is registered by the packs the
// CLI loads, and every phase but teardown and judge is ordinary.
func TestM1ScenarioActionsRegisteredAndPhases(t *testing.T) {
	s, err := tr.ParseFile(m1Scenario)
	if err != nil {
		t.Fatal(err)
	}
	r := tr.NewRegistry()
	actions.RegisterSystemActions(r)
	RegisterPack(r)
	for _, p := range s.Phases {
		for _, a := range p.Actions {
			if !r.Has(a.Action) {
				t.Errorf("phase %s: action %s is not registered", p.Name, a.Action)
			}
		}
	}
	want := []string{"build", "store", "master", "random", "teardown", "judge"}
	var got []string
	for _, p := range s.Phases {
		got = append(got, p.Name)
		if p.Always != (p.Name == "teardown" || p.Name == "judge") {
			t.Errorf("phase %s: always=%v", p.Name, p.Always)
		}
	}
	if !slices.Equal(got, want) {
		t.Errorf("phases %v, want %v", got, want)
	}
}

// The wiring: v3_gen_run's params and the typed operations' configuration
// resolve from env and the run plan, the pool is the spec's, and teardown and
// the judge run when a step fails.
func TestM1ScenarioWiring(t *testing.T) {
	for _, fail := range []string{"", "v3_gen_run", "master-spawn", "v3_op_spec"} {
		t.Run("fail="+fail, func(t *testing.T) {
			r := renderM1(t, map[string]string{"run_id": "20261126-120000-ab12", "gen_seed": "77", "nightly_seed": "9001"}, fail)
			if (r.status == tr.StatusPass) != (fail == "") {
				t.Fatalf("status %s", r.status)
			}
			for _, b := range r.bad {
				t.Error(b)
			}
			n := len(r.steps)
			last := func(i int) string { return r.steps[n-i].Action }
			if n < 5 || last(5) != "exec" || !strings.Contains(r.steps[n-5].Params["cmd"], " teardown ") || last(4) != "v3_history_end" || last(3) != "collect_path" || last(1) != "exec" {
				t.Errorf("the run does not end with teardown, the history's end, log collection and the checkers")
			}
			if r.steps[0].Action != "v3_run_plan" || r.steps[1].Action != "exec" || !strings.Contains(r.steps[1].Params["cmd"], " claim ") {
				t.Errorf("the run does not start with its plan and then the claim of its work directory: %s, %s", r.steps[0].Action, r.steps[1].Action)
			}
			ran := map[string]bool{}
			for _, s := range r.steps {
				ran[s.Action] = true
			}
			if fail == "master-spawn" || fail == "v3_op_spec" {
				if ran["v3_gen_run"] {
					t.Error("the random run started after the master's setup failed")
				}
				return
			}
			if !ran["v3_gen_run"] {
				t.Fatal("v3_gen_run never ran")
			}
			work := "/root/m1-run-20261126120000ab12"
			want := map[string]string{"bin_dir": work + "/bin", "master": "127.0.0.1:21003", "filer": "127.0.0.1:31002", "chunk_path": "/m1",
				"root": work + "/vol", "client": "c1", "own_script": "/runner/scenarios/v3block/m0-own.sh", "work": work, "owner": "ok", "lock_dirs_test_only": "true"}
			for k, w := range want {
				if r.opCfg[k] != w {
					t.Errorf("the typed operations' %s is %q, want %q", k, r.opCfg[k], w)
				}
			}
			pool := strings.Fields(r.env["gen_pool"])
			if r.gen.Seed != 77 || r.gen.Ops != 300 || r.gen.Blocks != 16 || !slices.Equal(r.gen.Pool, pool) {
				t.Errorf("v3_gen_run config %+v", r.gen)
			}
			for _, s := range r.steps {
				switch s.Action {
				case "v3_gen_run":
					if s.Params["nightly_seed"] != "9001" {
						t.Errorf("nightly_seed reaches v3_gen_run as %q", s.Params["nightly_seed"])
					}
				case "v3_op_spec":
					// Every name the generator may use is declared to the cluster spec.
					if !slices.Equal(strings.Fields(s.Params["volumes"]), pool) {
						t.Errorf("the spec declares %q, the generator's pool is %q", s.Params["volumes"], pool)
					}
					if _, err := specVolumes(pool, 21000, s.Params["nqn_prefix"]); err != nil {
						t.Error(err)
					}
					if s.Params["base_port"] != "21000" || s.Params["nqn_prefix"] != "nqn.2026-11.io.seaweedfs:op-20261126120000ab12-" {
						t.Errorf("spec base_port %q nqn_prefix %q", s.Params["base_port"], s.Params["nqn_prefix"])
					}
				}
			}
		})
	}
}

// footprint is what one run holds on the host: its listening ports (with
// what holds each), the paths it owns, and its NQN prefixes.
type footprint struct {
	ports map[int]string
	paths []string
	nqns  []string
	vnqns []string // every volume's subsystem NQN
}

var portInText = regexp.MustCompile(`(?:127\.0\.0\.1:|port(?:\.grpc)?=)(\d+)\b`)

func footprintOf(t *testing.T, r *m1Render) footprint {
	t.Helper()
	f := footprint{ports: map[int]string{}}
	add := func(p int, who string) {
		if prev, ok := f.ports[p]; ok && prev != who {
			t.Errorf("port %d held by both %s and %s in one run", p, prev, who)
		}
		f.ports[p] = who
	}
	for _, s := range r.steps {
		for k, v := range s.Params {
			if s.Action == "exec" && strings.HasPrefix(k, "env.") {
				if p, err := strconv.Atoi(v); err == nil {
					add(p, k)
				}
			}
			for _, m := range portInText.FindAllStringSubmatch(v, -1) {
				p, _ := strconv.Atoi(m[1])
				add(p, "literal "+k)
			}
			switch k {
			case "env.WORK", "spec_path", "path":
				f.paths = append(f.paths, v)
			case "env.NQN", "nqn_prefix":
				f.nqns = append(f.nqns, v)
			}
		}
		if s.Action == "v3_op_spec" {
			base, _ := strconv.Atoi(s.Params["base_port"])
			vs, err := specVolumes(strings.Fields(s.Params["volumes"]), base, s.Params["nqn_prefix"])
			if err != nil {
				t.Fatal(err)
			}
			for _, v := range vs {
				f.vnqns = append(f.vnqns, v.NQN)
				for _, h := range v.Hosts {
					for role, a := range map[string]string{"data": h.Data, "ctrl": h.Ctrl, "status": h.Status, "nvme": h.NVMe} {
						p, _ := strconv.Atoi(portOf(a))
						add(p, h.Server+" "+role)
					}
				}
			}
		}
	}
	for _, k := range []string{"master", "filer"} {
		for _, m := range portInText.FindAllStringSubmatch(r.opCfg[k], -1) {
			p, _ := strconv.Atoi(m[1])
			f.ports[p] = "ops " + k
			if k == "filer" { // the typed ops read the filer's HTTP listing at its gRPC port - 10000
				f.ports[p-10000] = "ops filer http"
			}
		}
	}
	for _, k := range []string{"bin_dir", "root", "work"} {
		f.paths = append(f.paths, r.opCfg[k])
	}
	return f
}

func underPath(p, dir string) bool {
	return p == dir || strings.HasPrefix(p, strings.TrimRight(dir, "/")+"/")
}

// Concurrent runs (the nightly's four seeds plus a long run, on one host)
// share no port, no owned path and no NQN prefix: rendered with two port bases
// and two run tags, or two run ids, their footprints are disjoint.
func TestM1ParallelRunsShareNothing(t *testing.T) {
	for name, envs := range map[string][2]map[string]string{
		"port_base and run_tag": {
			{"run_id": "20261126-120000-ab12", "port_base": "21000", "run_tag": "n1", "gen_seed": "11"},
			{"run_id": "20261126-120000-ab12", "port_base": "22000", "run_tag": "n2", "gen_seed": "12"},
		},
		"tag from the run id": {
			{"run_id": "20261126-120000-ab12", "port_base": "20000"},
			{"run_id": "20261126-120000-cd34", "port_base": "24000"},
		},
		"prefix-like tags": {
			{"port_base": "21000", "run_tag": "n1"},
			{"port_base": "23000", "run_tag": "n12"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			a, b := renderM1(t, envs[0], ""), renderM1(t, envs[1], "")
			for _, r := range []*m1Render{a, b} {
				if r.status != tr.StatusPass || len(r.bad) > 0 {
					t.Fatalf("status %s, %v", r.status, r.bad)
				}
			}
			fa, fb := footprintOf(t, a), footprintOf(t, b)
			if len(fa.ports) < 4+8*len(strings.Fields(a.env["gen_pool"])) {
				t.Fatalf("only %d ports found in a run", len(fa.ports))
			}
			for p, who := range fa.ports {
				if other, ok := fb.ports[p]; ok {
					t.Errorf("port %d: run A %s, run B %s", p, who, other)
				}
			}
			workA, workB := a.opCfg["work"], b.opCfg["work"]
			if underPath(workA, workB) || underPath(workB, workA) {
				t.Errorf("work directories %s and %s overlap", workA, workB)
			}
			for _, x := range []struct {
				f    footprint
				work string
			}{{fa, workA}, {fb, workB}} {
				for _, p := range x.f.paths {
					if !underPath(p, x.work) {
						t.Errorf("owned path %s is outside the run's work directory %s", p, x.work)
					}
				}
			}
			for _, x := range []struct{ own, other footprint }{{fa, fb}, {fb, fa}} {
				for _, q := range x.own.nqns {
					if q != x.own.nqns[0] {
						t.Errorf("one run uses two NQN prefixes: %q, %q", x.own.nqns[0], q)
					}
					for _, v := range x.other.vnqns {
						if strings.HasPrefix(v, q) {
							t.Errorf("the other run's subsystem %s carries this run's prefix %s (its teardown would delete it)", v, q)
						}
					}
				}
			}
		})
	}
}

func TestRunPlan(t *testing.T) {
	v, err := runPlan(21000, 32, "n1", "/root/m1-run-", "nqn.2026-11.io.seaweedfs:op-")
	if err != nil {
		t.Fatal(err)
	}
	for k, w := range map[string]string{"work": "/root/m1-run-n1", "nqn_prefix": "nqn.2026-11.io.seaweedfs:op-n1-", "weed_master_port": "21000",
		"weed_volume_port": "21001", "weed_filer_port": "21002", "master_port": "21003", "holder_base_port": "21000",
		"weed_master_grpc_port": "31000", "weed_volume_grpc_port": "31001", "weed_filer_grpc_port": "31002"} {
		if v[k] != w {
			t.Errorf("%s = %q, want %q", k, v[k], w)
		}
	}
	for name, f := range map[string]func() error{
		"tag with a dash": func() error { _, err := runPlan(21000, 32, "n-1", "/r-", "nqn.x:"); return err },
		"empty tag":       func() error { _, err := runPlan(21000, 32, "", "/r-", "nqn.x:"); return err },
		"low port":        func() error { _, err := runPlan(80, 32, "n1", "/r-", "nqn.x:"); return err },
		"grpc over 65535": func() error { _, err := runPlan(60000, 32, "n1", "/r-", "nqn.x:"); return err },
		"relative work":   func() error { _, err := runPlan(21000, 32, "n1", "r-", "nqn.x:"); return err },
		"no volumes":      func() error { _, err := runPlan(21000, 0, "n1", "/r-", "nqn.x:"); return err },
	} {
		if f() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	act := tr.Action{Params: map[string]string{"port_base": "21000", "volumes": "a b", "run_tag": "n1"}}
	// A port fixed by hand is refused; the same value (run_tag passed in env) is not.
	actx := &tr.ActionContext{Vars: map[string]string{"master_port": "19600"}, Log: func(string, ...interface{}) {}}
	if _, err := v3RunPlan(context.Background(), actx, act); err == nil {
		t.Error("a fixed master_port was overridden silently")
	}
	actx = &tr.ActionContext{Vars: map[string]string{"run_tag": "n1"}, Log: func(string, ...interface{}) {}}
	if _, err := v3RunPlan(context.Background(), actx, act); err != nil || actx.Vars["work"] != "/root/m1-run-n1" {
		t.Errorf("%v, work %q", err, actx.Vars["work"])
	}
	// Without a tag, the run id without its dashes.
	actx = &tr.ActionContext{Vars: map[string]string{"run_id": "20261126-120000-ab12"}, Log: func(string, ...interface{}) {}}
	act.Params["run_tag"] = ""
	if _, err := v3RunPlan(context.Background(), actx, act); err != nil || actx.Vars["run_tag"] != "20261126120000ab12" {
		t.Errorf("%v, tag %q", err, actx.Vars["run_tag"])
	}
}

// The default pool is sized for the default run: against a product that
// answers ok to every ordinary operation, 300 operations end on the budget,
// not the pool, for every seed tried, with room to spare.
func TestM1DefaultPoolLastsTheDefaultRun(t *testing.T) {
	s, err := tr.ParseFile(m1Scenario)
	if err != nil {
		t.Fatal(err)
	}
	pool := strings.Fields(s.Env["gen_pool"])
	ops, _ := strconv.Atoi(s.Env["gen_ops"])
	blocks, _ := strconv.Atoi(s.Env["gen_blocks"])
	maxUsed, sum, slowest := 0, 0, 0
	for seed := uint64(1); seed <= 200; seed++ {
		proposals, g := run(t, GenConfig{Seed: seed, Ops: ops, Pool: pool, Blocks: blocks})
		used := map[string]bool{}
		for _, o := range proposals {
			if o.Action == "v3_op_create" {
				used[o.Volume] = true
			}
			if tv := o.Params["target_volume"]; tv != "" {
				used[tv] = true
			}
		}
		if g.Ended() != "ops" {
			t.Errorf("seed %d ended %q", seed, g.Ended())
		}
		maxUsed, sum = max(maxUsed, len(used)), sum+len(used)
		if perHour := g.Coverage()["estimated_ops_per_hour"].(int); perHour > 0 {
			slowest = max(slowest, ops*3600/perHour)
		}
	}
	if maxUsed*3 > len(pool)*2 {
		t.Errorf("up to %d of %d names used: less than a third to spare", maxUsed, len(pool))
	}
	t.Logf("names used in %d operations, seeds 1..200: max %d, mean %.1f, of %d; slowest estimate %d s", ops, maxUsed, float64(sum)/200, len(pool), slowest)
}

// The header names the nightly seed next to the run's seed when one is set.
func TestGenRunHeaderNamesTheNightlySeed(t *testing.T) {
	for _, ns := range []string{"", "9001", "x"} {
		f := &fakeProduct{}
		f.install(t)
		actx, p := genRunCtx(t)
		act := genAct("5")
		act.Params["nightly_seed"] = ns
		_, err := v3GenRun(context.Background(), actx, act)
		if ns == "x" {
			if err == nil {
				t.Error("a nightly seed that is not a number was accepted")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		_ = actx.History.Close()
		h := records(t, p)[0]
		got, has := h.Result["nightly_seed"]
		if h.Verb != "gen_header" || h.Result["seed"] != "5" || (ns == "") == has || (has && got != ns) {
			t.Errorf("nightly_seed %q: header %+v", ns, h.Result)
		}
	}
}
