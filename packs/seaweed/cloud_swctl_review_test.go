package seaweed

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/pingqiu/sw-test-runner/inventory"
)

func TestSWCtlGPUUnknownRequiresExactAbsentHostDeclaration(t *testing.T) {
	for _, tc := range []struct {
		name, code, status string
		hosts              []string
		pass               bool
		preinstall         bool
	}{
		{"declared", "GPU_OBSERVATION_UNKNOWN", "UNKNOWN", []string{"fixture"}, true, false},
		{"undeclared", "GPU_OBSERVATION_UNKNOWN", "UNKNOWN", nil, false, false},
		{"foreign-host", "GPU_OBSERVATION_UNKNOWN", "UNKNOWN", []string{"foreign"}, false, true},
		{"duplicate", "GPU_OBSERVATION_UNKNOWN", "UNKNOWN", []string{"fixture", "fixture"}, false, true},
		{"different-code", "UNIT_OBSERVATION_UNKNOWN", "UNKNOWN", []string{"fixture"}, false, false},
		{"shape-fail", "LMCACHE_SHAPE", "FAIL", []string{"fixture"}, false, false},
		{"other-row", "GPU_OBSERVATION_UNKNOWN", "UNKNOWN", []string{"fixture"}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, c, host, calls := swctlAdapterFixture(t, "success")
			declared, _ := json.Marshal(map[string]any{"absent_gpu_hosts": tc.hosts})
			if e := json.Unmarshal(declared, c.SWCtl); e != nil {
				t.Fatal(e)
			}
			program, e := os.ReadFile(c.SWCtl.Binary.Path)
			if e != nil {
				t.Fatal(e)
			}
			gate := fmt.Sprintf(" for i in rows:\n  if i['id']=='lmcache.shape':i.update(status=%q,code=%q)\n", tc.status, tc.code)
			if tc.name == "other-row" {
				gate += " rows[0].update(status='UNKNOWN',code='RDMA_UNREADABLE')\n"
			}
			program = []byte(strings.Replace(string(program), " r['items']=rows;r['status']='UNKNOWN';exit=2", gate+" r['items']=rows;r['status']='UNKNOWN';exit=2", 1))
			if e = os.WriteFile(c.SWCtl.Binary.Path, program, 0700); e != nil {
				t.Fatal(e)
			}
			c.SWCtl.Binary.SHA256 = fmt.Sprintf("%x", sha256.Sum256(program))
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			_, e = RunCloudHTTP(ctx, raw, c)
			if (e == nil) != tc.pass {
				t.Fatalf("pass=%t got %v", tc.pass, e)
			}
			data, _ := os.ReadFile(calls)
			if tc.preinstall && strings.Contains(string(data), `"Action": "install"`) {
				t.Fatal("bad absence declaration reached install")
			}
			ran := false
			for _, cmd := range host.commands {
				ran = ran || strings.Contains(cmd, "kv_smoke.py") && strings.Contains(cmd, "--config")
			}
			if ran != tc.pass {
				t.Fatalf("smoke dispatch=%t expected=%t", ran, tc.pass)
			}
			if tc.pass {
				for _, n := range []string{"swctl-doctor-before.json", "swctl-doctor.json"} {
					b, err := os.ReadFile(filepath.Join(c.Output, n))
					if err != nil || !strings.Contains(string(b), "GPU_OBSERVATION_UNKNOWN") {
						t.Fatalf("uncertainty lost: %s %v", b, err)
					}
				}
			}
		})
	}
}

// The real postflight call produces a command; run it through the same actual
// sh -ec boundary as SSH, faking only host command outputs, never its verdict.
type shellPostflightHost struct {
	dir      string
	commands []string
}

func (h *shellPostflightHost) ConnectContext(context.Context) error { return nil }
func (h *shellPostflightHost) Close()                               {}
func (h *shellPostflightHost) Upload(string, string) error          { return nil }
func (h *shellPostflightHost) Run(ctx context.Context, s string) (string, string, int, error) {
	h.commands = append(h.commands, s)
	cmd := exec.CommandContext(ctx, "/bin/sh", "-ec", s)
	cmd.Env = append(os.Environ(), "PATH="+h.dir+":/usr/bin:/bin")
	out, e := cmd.CombinedOutput()
	rc := 0
	if e != nil {
		rc = 1
		if x, ok := e.(*exec.ExitError); ok {
			rc = x.ExitCode()
		}
	}
	return string(out), "", rc, nil
}
func TestSWCtlPostflightRealShellDistinguishesEmptyFromFailedRead(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux SSH shell boundary")
	}
	for _, tc := range []struct {
		name, units, sockets, grep string
		pass                       bool
	}{
		{"clean", "", "", "", true},
		{"owned-unit", "swctl-fixture-cluster-volume.service loaded active running", "", "", false},
		{"owned-port", "", "LISTEN 0 4096 0.0.0.0:8080 0.0.0.0:*", "", false},
		{"unrelated-port", "", "LISTEN 0 4096 0.0.0.0:8081 0.0.0.0:*", "", true},
		{"unit-read-failure", "EXIT3", "", "", false},
		{"socket-read-failure", "", "EXIT3", "", false},
		{"grep-error", "", "", "exit 2", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			put := func(n, b string) {
				t.Helper()
				if e := os.WriteFile(filepath.Join(dir, n), []byte("#!/bin/sh\n"+b+"\n"), 0700); e != nil {
					t.Fatal(e)
				}
			}
			put("sudo", `if [ "$1" = "-n" ]; then shift; fi; exec "$@"`)
			script := func(v string) string {
				if v == "EXIT3" {
					return "exit 3"
				}
				return "printf '%s' " + cloudQuote(v)
			}
			put("systemctl", script(tc.units))
			put("ss", script(tc.sockets))
			if tc.grep != "" {
				put("grep", tc.grep)
			}
			h := &shellPostflightHost{dir: dir}
			d := inventory.Document{Hosts: []inventory.Host{{ID: "fixture", Services: []inventory.Service{{Name: "volume", Port: 8080}}}}}
			c := CloudConfig{Output: t.TempDir(), swctlNodes: func(inventory.Host) swctlSmokeNode { return h }}
			e := swctlPostflight(context.Background(), d, c, "fixture-cluster")
			if (e == nil) != tc.pass {
				t.Fatalf("pass=%t error=%v", tc.pass, e)
			}
			if len(h.commands) != 1 {
				t.Fatal("postflight command count", h.commands)
			}
		})
	}
}
