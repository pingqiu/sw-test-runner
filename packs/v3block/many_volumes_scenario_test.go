package v3block

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"

	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	tr "github.com/pingqiu/sw-test-runner"
)

// The committed scenario through the real engine with recording handlers:
// every parameter the typed actions get is resolved (no "{{" left), the master
// restart gets the first master's pid, teardown runs when a phase fails, and a
// failing operation fails the run. It proves the wiring, not the product.
func TestManyVolumesScenarioWiring(t *testing.T) {
	for _, fail := range []string{"", "v3_vs_move", "v3_vs_objects"} {
		t.Run("fail="+fail, func(t *testing.T) {
			s, err := tr.ParseFile("../../scenarios/v3block/many-volumes.yaml")
			if err != nil {
				t.Fatal(err)
			}
			s.Env["product_src"], s.Env["runner_src"], s.Env["weed"] = "/src", "/runner", "/bin/weed"
			var mu sync.Mutex
			var calls []tr.Action
			handler := func(_ context.Context, actx *tr.ActionContext, a tr.Action) (map[string]string, error) {
				mu.Lock()
				calls = append(calls, a)
				mu.Unlock()
				if a.Action == fail && a.Params["mode"] != "record" {
					return nil, fmt.Errorf("injected")
				}
				if a.SaveAs == "counted" {
					return map[string]string{"value": "0"}, nil
				}
				if a.Action == "v3_start_blockmaster" && a.SaveAs != "" {
					actx.Vars[a.SaveAs+"_pid"] = fmt.Sprint(100 + len(calls))
				}
				if a.Action == "v3_vs_create" {
					actx.Vars["create_p50_ms"], actx.Vars["create_p99_ms"], actx.Vars["create_max_ms"] = "1", "2", "3"
				}
				return map[string]string{"value": "ok"}, nil
			}
			r := tr.NewRegistry()
			for _, name := range []string{"exec", "emit_provenance", "v3_start_blockmaster", "v3_vs_spec", "v3_vs_create", "v3_vs_attach", "v3_vs_write",
				"v3_vs_verify", "v3_vs_snapshot", "v3_vs_snapshot_delete", "v3_vs_objects", "v3_vs_restart", "v3_vs_serving", "v3_vs_move", "v3_vs_host", "v3_vs_stop"} {
				r.RegisterFunc(name, tr.TierCore, handler)
			}
			actx := &tr.ActionContext{Scenario: s, Vars: map[string]string{}, Log: func(string, ...interface{}) {}}
			res := tr.NewEngine(r, nil).Run(context.Background(), s, actx)
			if (res.Status == tr.StatusPass) != (fail == "") {
				t.Fatalf("status %s error %q", res.Status, res.Error)
			}
			seen := map[string]int{}
			var masterPIDs []string
			for _, a := range calls {
				seen[a.Action]++
				for k, v := range a.Params {
					// The provenance rows read figures an action may not have produced
					// (a failed run); every other parameter must be resolved.
					if strings.Contains(v, "{{") && !(a.Action == "emit_provenance" && strings.HasPrefix(k, "__vs_")) {
						t.Errorf("%s param %s unresolved: %q", a.Action, k, v)
					}
				}
				switch a.Action {
				case "v3_vs_spec":
					if a.Params["filer"] != "127.0.0.1:29388" || a.Params["master"] != "127.0.0.1:19400" || a.Params["count"] != "4" {
						t.Errorf("spec params %v", a.Params)
					}
				case "exec":
					if p := a.Params["env.PID"]; p != "" {
						masterPIDs = append(masterPIDs, p)
					}
					if strings.HasPrefix(a.Params["cmd"], "kill ") {
						masterPIDs = append(masterPIDs, strings.Fields(a.Params["cmd"])[1])
					}
				}
			}
			if seen["v3_vs_stop"] != 1 {
				t.Errorf("teardown ran %d times", seen["v3_vs_stop"])
			}
			if fail == "" {
				if seen["v3_start_blockmaster"] != 2 || len(masterPIDs) != 2 || masterPIDs[0] == masterPIDs[1] {
					t.Errorf("master restart: starts %d, killed pids %v (want the first, then the second)", seen["v3_start_blockmaster"], masterPIDs)
				}
				if seen["emit_provenance"] != 1 {
					t.Errorf("provenance emitted %d times", seen["emit_provenance"])
				}
			} else if seen["emit_provenance"] != 0 {
				t.Errorf("a failed run emitted provenance")
			}
		})
	}
}

// Every volume needs a holder-routed delete after the moves, including the
// volume whose earlier snapshot was deleted while stopped. That delete kicks
// cleaning; the scenario must not wait for the hourly maintenance timer.
func TestManyVolumesKicksCleaningAfterMoves(t *testing.T) {
	s, err := tr.ParseFile("../../scenarios/v3block/many-volumes.yaml")
	if err != nil {
		t.Fatal(err)
	}
	snap, deleted, checked := false, false, false
	for _, phase := range s.Phases {
		if phase.Name != "snapshot-delete-and-cleaning" {
			continue
		}
		for _, a := range phase.Actions {
			if a.Action == "v3_vs_snapshot" && a.Params["name"] == "s2" {
				snap = true
			}
			if a.Action == "v3_vs_snapshot_delete" && a.Params["name"] == "s2" {
				if !snap {
					t.Fatal("delete precedes final snapshot")
				}
				deleted = true
			}
			if a.Action == "v3_vs_objects" && a.Params["mode"] == "live" {
				checked = true
				seconds, err := strconv.Atoi(a.Params["bound_seconds"])
				if !deleted || err != nil || seconds <= 0 || seconds > 300 {
					t.Fatalf("need kicked cleaning with <=300s observation: deleted=%v bound=%q", deleted, a.Params["bound_seconds"])
				}
				limit, err := time.ParseDuration(a.Timeout)
				if err != nil || limit <= time.Duration(seconds)*time.Second {
					t.Fatalf("action timeout %q must exceed observation", a.Timeout)
				}
			}
		}
	}
	if !checked {
		t.Fatal("missing live-set observation")
	}
}

func TestManyVolumesSamplesThroughoutGuestIO(t *testing.T) {
	s, err := tr.ParseFile("../../scenarios/v3block/many-volumes.yaml")
	if err != nil {
		t.Fatal(err)
	}
	active, started, stopped, cleanup := false, false, false, false
	for _, p := range s.Phases {
		for _, a := range p.Actions {
			if a.Action == "v3_vs_host" {
				switch a.Params["mode"] {
				case "start":
					if started {
						t.Fatal("sampler started twice")
					}
					active = true
					started = true
				case "stop":
					if !active {
						t.Fatal("sampler stopped without start")
					}
					active = false
					stopped = true
				case "cleanup":
					if !p.Always {
						t.Fatal("sampler cleanup is not always")
					}
					cleanup = true
				default:
					t.Fatal("unbounded one-shot sample")
				}
			}
			if a.Action == "v3_vs_create" || a.Action == "v3_vs_attach" || a.Action == "v3_vs_write" || a.Action == "v3_vs_move" || a.Action == "v3_vs_verify" || a.Action == "v3_vs_restart" {
				if !active || p.Parallel {
					t.Fatalf("%s is outside continuous sampling", a.Action)
				}
			}
			if a.Action == "emit_provenance" && (!stopped || active) {
				t.Fatal("report precedes sampler settlement")
			}
		}
	}
	if !started || !stopped || !cleanup {
		t.Fatal("missing sampler lifecycle")
	}
}

// Execute the real admission command: a smoke can never claim the counted gate.
func TestManyVolumesCountedClass(t *testing.T) {
	shell, err := exec.LookPath("bash")
	if runtime.GOOS == "windows" {
		if _, err := os.Stat("C:/Program Files/Git/bin/bash.exe"); err == nil {
			shell = "C:/Program Files/Git/bin/bash.exe"
			err = nil
		}
	}
	if shell == "" || err != nil {
		t.Skip("bash unavailable")
	}
	s, err := tr.ParseFile("../../scenarios/v3block/many-volumes.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cmd := s.Phases[0].Actions[0].Params["cmd"]
	for _, c := range []struct {
		class, n, m, p, want string
		pass                 bool
	}{
		{"smoke", "4", "2", "4", "0", true}, {"counted", "200", "10", "200", "1", true},
		{"counted", "4", "10", "200", "", false}, {"counted", "200", "2", "200", "", false},
		{"counted", "200", "10", "16", "", false}, {"other", "200", "10", "200", "", false},
	} {
		p := exec.Command(shell, "-c", cmd)
		p.Env = append(os.Environ(), "CLASS="+c.class, "N="+c.n, "M="+c.m, "P="+c.p)
		b, e := p.Output()
		if (e == nil) != c.pass || (c.pass && strings.TrimSpace(string(b)) != c.want) {
			t.Fatalf("%+v: %s %v", c, b, e)
		}
	}
	var prov tr.Action
	for _, p := range s.Phases {
		for _, a := range p.Actions {
			if a.Action == "emit_provenance" {
				prov = a
			}
		}
	}
	if prov.Params["gate_pass"] != "{{ counted }}" || prov.Params["tested_sha"] != "{{ product_head }}" {
		t.Fatal("provenance is not bound to counted shape and source pin")
	}

}
