//go:build linux

package seaweed

import (
	"context"
	"encoding/json"
	"fmt"
	tr "github.com/pingqiu/sw-test-runner"
	"golang.org/x/crypto/ssh"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDirectReadCaseGate(t *testing.T) {
	h := &soakHost{}
	address, _, key, calls := cloudSSH(t, "", h.exec)
	for _, name := range []string{"one-host", "unknown", "mixed", "profile"} {
		t.Run(name, func(t *testing.T) {
			var c CloudConfig
			json.Unmarshal([]byte(`{"read_case":"volume-restart"}`), &c)
			c.SSHKey, c.Output, c.SoakRelease, c.SoakProfile = key, t.TempDir(), t.TempDir(), "/nonexistent/read-case-profile.json"
			raw := directInput(t, address)
			if name != "one-host" {
				var d map[string]any
				json.Unmarshal(raw, &d)
				hosts := d["hosts"].([]any)
				second := map[string]any{}
				b, _ := json.Marshal(hosts[0])
				json.Unmarshal(b, &second)
				second["id"] = "m02"
				second["management"].(map[string]any)["port"] = hosts[0].(map[string]any)["management"].(map[string]any)["port"].(float64) + 1
				second["data"].(map[string]any)["address"] = "192.0.2.77"
				hosts[0].(map[string]any)["id"] = "m01"
				d["hosts"] = append(hosts, second)
				raw, _ = json.Marshal(d)
			}
			want := "read_case requires"
			if name == "unknown" {
				json.Unmarshal([]byte(`{"read_case":"anything"}`), &c)
			}
			if name == "mixed" {
				c.KVSoak = true
			}
			if name == "profile" {
				want = "no such file"
			}
			if _, err := RunCloudHTTP(soakCtx(t), raw, c); err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("%s: %v want %s", name, err, want)
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatal("refused input touched a host")
	}
}

type readSSH struct {
	commandsRead           []string
	stopDone, contTooEarly bool
	*soakHost
	muRead         sync.Mutex
	seed, dc, http []byte
	row            map[string]any
	mode           string
	pre            float64
	replaced       bool
	cancel         context.CancelFunc
	cont           int
	role           string
}

func (h *readSSH) exec(cmd string, ch ssh.Channel) uint32 {
	plain := strings.NewReplacer(`'"'"'`, "", "'", "").Replace(cmd)
	h.muRead.Lock()
	defer h.muRead.Unlock()
	h.commandsRead = append(h.commandsRead, cmd)
	switch {
	case strings.Contains(plain, "kill -STOP"):
		h.pre, _ = controllerMono()
		if h.mode == "stop-unknown" {
			return 23
		}
		if h.cancel != nil {
			h.cancel()
		}
		if h.mode == "cancel-stop-tail" {
			h.muRead.Unlock()
			time.Sleep(200 * time.Millisecond)
			h.muRead.Lock()
			h.stopDone = true
		}
		fmt.Fprintln(ch, "PID 4242 66\nSTOPPED")
		return 0
	case strings.Contains(plain, "kill -CONT"):
		h.cont++
		if h.mode == "cancel-stop-tail" && !h.stopDone {
			h.contTooEarly = true
		}
		if h.mode == "cont-unknown" {
			return 23
		}
		fmt.Fprintln(ch, "LIVE")
		return 0
	case strings.Contains(plain, "kill -KILL"):
		h.pre, _ = controllerMono()
		h.replaced = true
		fmt.Fprintln(ch, "OLD 4242 66\nGONE")
		return 0
	case strings.Contains(plain, "MainPID"):
		pid, start := 4242, 66
		if h.replaced {
			pid, start = 5151, 77
		}
		if h.mode == "bad-identity" {
			start = 0
		}
		fmt.Fprintf(ch, "%d %d boot-m02 %s\n", pid, start, strings.Repeat("a", 64))
		return 0
	case strings.Contains(plain, "ready-step"):
		id := tr.ProcessIdentity{PID: 4242, StartTime: 66, BootID: "boot-m02", ExecutableSHA256: strings.Repeat("a", 64)}
		if h.replaced {
			id.PID, id.StartTime = 5151, 77
		}
		if h.mode == "bad-ready" {
			id.PID++
		}
		b, _ := json.Marshal(map[string]any{"status": "READY", "processes": map[string]any{h.role: map[string]any{"scope_identity": id}}})
		fmt.Fprint(ch, string(b))
		return 0
	case strings.Contains(plain, "ExecMainStatus"):
		row := h.row
		if h.mode == "before-P" {
			shiftRestartTrace(row, -4)
		}
		shiftRestartTrace(row, h.pre-30)
		b, _ := json.Marshal(row)
		if h.mode == "bad-terminal" {
			b = []byte("{}")
		}
		fmt.Fprintf(ch, "0\n%s", b)
		return 0
	case strings.Contains(plain, "--stage seed"):
		if h.mode == "bad-seed" {
			fmt.Fprint(ch, "{}")
		} else {
			fmt.Fprint(ch, string(h.seed))
		}
		return 0
	case strings.Contains(plain, "--stage recovery"):
		body := h.dc
		if strings.Contains(plain, "--read-path http") {
			body = h.http
		}
		fmt.Fprint(ch, string(body))
		return 0
	}
	return h.soakHost.exec(cmd, ch)
}

func readDirectFixture(t *testing.T, name, mode string) (*directSoak, *readSSH, *soakHost, context.Context) {
	t.Helper()
	m01, m02 := &soakHost{}, &soakHost{}
	s := directSoakFixture(t, m01, m02)
	host := &readSSH{soakHost: &soakHost{}, role: "volume", mode: mode, seed: pythonReadOutput(t, "seed", "dc"), dc: pythonReadOutput(t, "recovery", "dc"), http: pythonReadOutput(t, "recovery", "http"), row: windowBody(t, readConfig())}
	if mode == "wrong-byte" {
		rows := strings.Split(string(host.seed), "\n")
		var first map[string]any
		json.Unmarshal([]byte(rows[0]), &first)
		first["sha256"] = strings.Repeat("0", 64)
		b, _ := json.Marshal(first)
		rows[0] = string(b)
		host.seed = []byte(strings.Join(rows, "\n"))
	}
	if mode == "dirty" {
		host.soakHost.dirty = true
	}
	if name == "filer-restart" {
		host.role = "filer"
	}
	a1, pin1, key, _ := cloudSSH(t, "", host.exec)
	a2, pin2, _, _ := cloudSSH(t, "", host.exec)
	for i, pair := range [][2]string{{a1, pin1}, {a2, pin2}} {
		address, port, _ := strings.Cut(pair[0], ":")
		s.d.Hosts[i].Management.Address = address
		fmt.Sscan(port, &s.d.Hosts[i].Management.Port)
		s.d.Hosts[i].Management.HostKey = &pair[1]
	}
	s.c.SSHKey, s.c.ReadCase = key, name
	s.b.Plan.Soak = nil
	s.d.Observation = profileFixture(t).Observation
	for i := range s.d.Observation.Hosts {
		s.d.Observation.Hosts[i].BootID = "boot-" + s.d.Observation.Hosts[i].HostID
	}
	os.MkdirAll(filepath.Join(s.c.SoakRelease, "bin"), 0700)
	os.WriteFile(filepath.Join(s.c.SoakRelease, "bin/weed"), []byte("weed"), 0700)
	os.WriteFile(filepath.Join(s.c.SoakRelease, "bin/weed-volume"), []byte("volume"), 0700)
	s.in.Files["m02"]["/opt/seaweed/rel/bin/weed-volume"] = strings.Repeat("a", 64)
	s.in.Files["m02"]["/opt/seaweed/rel/bin/weed"] = strings.Repeat("a", 64)
	s.b.Restarts["read-case"] = s.b.Restarts["E1"]
	c := readConfig()
	body, _ := json.Marshal(c)
	for _, v := range []struct{ label, stage, route string }{{"read-seed", "seed", "dc"}, {"read-load", "load", "dc"}, {"read-dc", "recovery", "dc"}, {"read-http", "recovery", "http"}} {
		s.b.Jobs[v.label] = tr.JobPin{Node: "m01", Executable: "python3", Argv: []string{"-I", "kv_smoke.py", "--config", string(body), "--stage", v.stage, "--read-path", v.route}}
	}
	s.b.Jobs["read-http-before"] = s.b.Jobs["read-http"]
	s.b.Jobs["read-ready-m02"] = s.b.Jobs["ready-m02"]
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	t.Cleanup(cancel)
	s.deadline = time.Now().Add(80 * time.Second)
	s.d.Deadlines.Work = s.deadline.UTC().Format(time.RFC3339Nano)
	s.d.Deadlines.Hard = time.Now().Add(90 * time.Second).UTC().Format(time.RFC3339Nano)
	if mode == "cancel-stop" || mode == "cancel-stop-tail" {
		host.cancel = cancel
	}
	if mode == "no-overlap" {
		host.row["faults"], host.row["exact"] = []any{}, 6
		times := [][2]float64{{1, 2}, {10, 11}, {60, 61}, {70, 71}, {80, 81}, {119, 120}}
		for i, v := range host.row["outcomes"].([]any) {
			r := v.(map[string]any)
			r["started"], r["ended"], r["exact"] = times[i][0], times[i][1], true
		}
		a := host.row["after"].(map[string]any)
		a["read_errors"] = 0
		l := a["loader"].(map[string]any)
		l["read_objects"], l["direct_dc_reads"], l["read_bytes"], l["direct_dc_bytes"] = 6, 6, 6*c.Length, 6*c.Length
	}
	if mode == "no-post-R" {
		host.row["outcomes"].([]any)[5].(map[string]any)["started"], host.row["outcomes"].([]any)[5].(map[string]any)["ended"] = 40., 41.
		host.row["last_exact"] = 41.
	}
	if name == "volume-freeze" && mode != "no-overlap" && mode != "no-post-R" {
		outcomes := host.row["outcomes"].([]any)
		faults := host.row["faults"].([]any)
		for i, times := range [][2]float64{{33, 54}, {55, 56}} {
			outcomes[i+2].(map[string]any)["started"], outcomes[i+2].(map[string]any)["ended"] = times[0], times[1]
			faults[i].(map[string]any)["started"], faults[i].(map[string]any)["ended"] = times[0], times[1]
		}
		outcomes[4].(map[string]any)["started"], outcomes[4].(map[string]any)["ended"] = 57., 58.
	}
	_ = m01
	_ = m02
	return s, host, host.soakHost, ctx
}

func TestDirectReadCasesActualCallerAndCleanup(t *testing.T) {
	old := chaosSettle
	chaosSettle = 0
	defer func() { chaosSettle = old }()
	for _, tc := range []struct {
		name, mode string
		ok         bool
	}{
		{"volume-restart", "positive", true}, {"filer-restart", "positive", true},
		{"volume-restart", "bad-seed", false}, {"volume-restart", "wrong-byte", false}, {"volume-restart", "dirty", false}, {"volume-restart", "bad-terminal", false},
		{"volume-restart", "before-P", false}, {"volume-restart", "bad-ready", false}, {"volume-restart", "bad-identity", false},
		{"volume-freeze", "positive", true}, {"volume-freeze", "no-overlap", false}, {"volume-freeze", "no-post-R", false}, {"volume-freeze", "cancel-stop", false}, {"volume-freeze", "cancel-stop-tail", false}, {"volume-freeze", "stop-unknown", false}, {"volume-freeze", "cont-unknown", false},
	} {
		t.Run(tc.name+"/"+tc.mode, func(t *testing.T) {
			s, h, _, ctx := readDirectFixture(t, tc.name, tc.mode)
			file, err := s.run(ctx)
			if (err == nil) != tc.ok {
				t.Fatalf("file%s err%v", file, err)
			}
			if tc.ok {
				b, e := os.ReadFile(file)
				if e != nil || !strings.Contains(string(b), `"status": "PASS"`) {
					t.Fatalf("missing result %s %v", b, e)
				}
			}
			h.muRead.Lock()
			defer h.muRead.Unlock()
			if tc.name == "volume-freeze" && h.cont != 1 {
				t.Fatalf("CONT calls=%d err%v", h.cont, err)
			}
			for _, cmd := range h.commandsRead {
				if out, e := exec.Command("sh", "-n", "-c", cmd).CombinedOutput(); e != nil {
					t.Fatalf("invalid shell %v %s", e, out)
				}
			}
			if h.contTooEarly {
				t.Fatal("CONT dispatched before STOP reply settled")
			}
			if tc.mode == "bad-identity" && h.replaced {
				t.Fatal("invalid identity reached Kill")
			}
			if tc.mode == "cont-unknown" && !strings.Contains(fmt.Sprint(err), "UNKNOWN") {
				t.Fatalf("UNKNOWN lost: %v", err)
			}
			if (tc.mode == "no-overlap" || tc.mode == "no-post-R") && !strings.Contains(fmt.Sprint(err), "NOT_COVERED") {
				t.Fatalf("coverage disposition: %v", err)
			}
			for _, id := range []string{"m01", "m02"} {
				for _, file := range []string{"cloud-cleanup.json", "cloud-postflight.json"} {
					if _, e := os.Stat(filepath.Join(s.c.Output, id, file)); e != nil {
						t.Fatalf("%s/%s cleanup absent: %v", id, file, e)
					}
				}
			}
		})
	}
}

func TestDirectReadJobsReuseProfileAndWorker(t *testing.T) {
	for _, name := range []string{"volume-restart", "filer-restart", "volume-freeze"} {
		s := &directSoak{d: profileFixture(t), c: CloudConfig{ReadCase: name}, b: ProfileBundle{Plan: Plan{Smoke: SmokeSettings{HelperDir: "/opt/profile/worker", NativeDir: "/opt/profile/native", Keys: 4, Length: 1048576}, Deploy: []Step{{Host: "m02", Role: "volume", Job: "v"}, {Host: "m02", Role: "filer", Job: "f"}}}, Jobs: map[string]tr.JobPin{"v": {Node: "m02", Executable: "/usr/bin/python3.12"}, "f": {Node: "m02", Executable: "/usr/bin/python3.12"}}, Restarts: map[string]tr.GenerationRestartPlan{}}, in: ProfileInput{Files: map[string]map[string]string{"m01": {}, "m02": {"/opt/profile/bin/weed": strings.Repeat("a", 64), "/opt/profile/bin/weed-volume": strings.Repeat("b", 64)}}}}
		if err := s.readJobs(); err != nil {
			t.Fatal(err)
		}
		role := "volume"
		if name == "filer-restart" {
			role = "filer"
		}
		args := s.b.Jobs["read-g2"].Argv
		if args[len(args)-1] != "--replacement-"+role {
			t.Fatal(args)
		}
		var cfg ProfileConfig
		json.Unmarshal([]byte(args[3]), &cfg)
		if cfg.Root != s.d.Hosts[1].RunRoot || cfg.BootID != observedBoot(s.d, "m02") {
			t.Fatal("replacement escaped profile")
		}
		for _, label := range []string{"read-seed", "read-load", "read-dc", "read-http"} {
			j := s.b.Jobs[label]
			if j.Node != "m01" || !strings.HasSuffix(j.Argv[1], "/kv_smoke.py") {
				t.Fatalf("new worker path: %+v", j)
			}
		}
	}
}
