//go:build linux

package seaweed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/pingqiu/sw-test-runner/inventory"
	"golang.org/x/crypto/ssh"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

type masterWriteHost struct {
	directHost
	cfg    SmokeConfig
	mode   string
	rows   []byte
	cancel context.CancelFunc
}

func (h *masterWriteHost) exec(cmd string, c ssh.Channel) uint32 {
	if out, err := exec.Command("sh", "-n", "-c", cmd).CombinedOutput(); err != nil {
		fmt.Fprint(c, string(out))
		return 99
	}
	if strings.Contains(cmd, "systemctl show") && !strings.Contains(cmd, "sudo -n systemctl show") {
		return 23
	}
	var output string
	switch {
	case strings.Contains(cmd, "sudo -n systemctl start"):
		return 23 // a reset transient unit has no definition to start
	case strings.Contains(cmd, "--stage") && strings.Contains(cmd, "write-load"):
		if h.cancel != nil {
			h.cancel()
		}
		if !strings.Contains(cmd, "--read-path") || !strings.Contains(cmd, "http") || !strings.Contains(cmd, `"length":65536`) {
			return 23
		}
	case strings.Contains(cmd, "echo GONE"):
		output = "104.5\nOLD 100 1\nGONE\n"
		if h.mode == "lost-kill" || h.mode == "failed-kill-readback" {
			return 23
		}
		if h.mode == "bad-clock" {
			output = "NaN\nOLD 100 1\nGONE\n"
		}
	case strings.Contains(cmd, "echo \"NEW"):
		if !strings.Contains(cmd, "-master-g2") {
			return 23
		}
		if h.mode == "failed-ready-readback" {
			return 23
		}
		output = "106.5\nNEW 101 2\n"
		if h.mode == "same-generation" {
			output = "106.5\nNEW 100 1\n"
		}
	case strings.Contains(cmd, "until test -f"):
		if h.mode == "failed-worker" {
			return 23
		}
		output = string(h.rows)
		if h.mode == "missing-terminal" {
			output = strings.Join(strings.Split(strings.TrimSpace(output), "\n")[:13], "\n")
		}
	default:
		return h.directHost.exec(cmd, c)
	}
	h.mu.Lock()
	h.commands = append(h.commands, cmd)
	h.mu.Unlock()
	fmt.Fprint(c, output)
	return 0
}
func TestDirectMasterWritesReportAndCleanup(t *testing.T) {
	old := masterWriteSettle
	masterWriteSettle = 0
	defer func() { masterWriteSettle = old }()
	for _, mode := range []string{"positive", "failed-worker", "missing-terminal", "same-generation", "bad-clock", "lost-kill", "cancelled", "failed-kill-readback", "failed-ready-readback"} {
		t.Run(mode, func(t *testing.T) {
			h := &masterWriteHost{mode: mode}
			address, pin, key, _ := cloudSSH(t, "", h.exec)
			inv := directInput(t, address)
			var d inventory.Document
			if err := json.Unmarshal(inv, &d); err != nil {
				t.Fatal(err)
			}
			d.State = "resolved"
			cfg, err := smokeConfig(&d, d.Hosts[0].ID, SmokeSettings{NativeDir: d.Hosts[0].RunRoot, HelperDir: d.Hosts[0].RunRoot + "/worker", Keys: 16, Length: 1 << 20})
			if err != nil {
				t.Fatal(err)
			}
			raw := fixtureRows(t, func(_, _ []map[string]any, end map[string]any) { end["transport"] = "http" })
			var bound []byte
			for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
				var row map[string]any
				json.Unmarshal([]byte(line), &row)
				if k, ok := row["key"].(string); ok {
					row["key"] = strings.Replace(k, "m01", cfg.Client, 1)
				}
				if _, ok := row["run_id"]; ok {
					row["run_id"], row["client"], row["root"] = cfg.RunID, cfg.Client, cfg.Root
				}
				b, _ := json.Marshal(row)
				bound = append(append(bound, b...), '\n')
			}
			raw = bound
			h.rows = raw
			c := CloudConfig{MasterWrites: true, SSHKey: key, HostKey: pin, Output: t.TempDir(), Files: map[string]CloudFile{}}
			for _, name := range []string{"weed", "weed-volume", "native.tar"} {
				p := filepath.Join(t.TempDir(), name)
				os.WriteFile(p, []byte(name), 0600)
				c.Files[name] = CloudFile{Path: p, SHA256: hash([]byte(name))}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if mode == "cancelled" {
				h.cancel = cancel
				masterWriteSettle = time.Hour
			} else {
				masterWriteSettle = 0
			}
			out, err := RunCloudHTTP(ctx, inv, c)
			if (err == nil) != (mode == "positive") {
				t.Fatalf("mode%s: %v", mode, err)
			}
			if err == nil {
				var report WriteReport
				b, e := os.ReadFile(out)
				if e != nil {
					t.Fatal(e)
				}
				if json.Unmarshal(b, &report) != nil || report.Verdict != "REPORT_ONLY" || report.Acked != 9 || report.Sets[1].Failed != 1 {
					t.Fatalf("report%s", b)
				}
			}
			a, b, z := h.index("journalctl"), h.index("systemctl stop"), h.index("systemctl list-units")
			if a < 0 || b <= a || z <= b {
				t.Fatalf("cleanup %d %d %d", a, b, z)
			}
			if mode == "cancelled" && h.index("echo GONE") >= 0 {
				t.Fatal("Kill after cancellation")
			}
			if mode == "positive" {
				command := cloudServices(d.Hosts[0], d.Hosts[0], d.Hosts[0].RunRoot, false, "")["master"]
				found := false
				for _, cmd := range h.commands {
					if strings.Contains(cmd, cloudQuote(cloudStart(d, d.Hosts[0].RunRoot, "master-g2", command))) {
						found = true
					}
				}
				if !found {
					t.Fatal("replacement did not reuse cloudStart and the original master argv/mdir")
				}
			}
			if h.index("-v=1 filer") < 0 {
				t.Fatal("filer assign diagnostics absent")
			}
		})
	}
}
func TestHTTPWriteReportCannotBorrowDCTransport(t *testing.T) {
	c := writeCfg()
	c.Transport = "http"
	if _, err := writeReport(fixtureRows(t, nil), c, 104.5, 106.5); err == nil {
		t.Fatal("DC terminal accepted for HTTP")
	}
	raw := fixtureRows(t, func(_, _ []map[string]any, end map[string]any) { end["transport"] = "http" })
	if r, err := writeReport(raw, c, 104.5, 106.5); err != nil || r.Verdict != "REPORT_ONLY" {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestDirectMasterWritesRefusesCombinedCases(t *testing.T) {
	for _, mode := range []string{"ec", "chaos", "replication", "chaos-set"} {
		t.Run(mode, func(t *testing.T) {
			inv := directInput(t, "127.0.0.1:22")
			c := CloudConfig{MasterWrites: true, Files: map[string]CloudFile{"weed": {}, "weed-volume": {}, "native.tar": {}}}
			switch mode {
			case "ec":
				c.ECStage1 = true
				delete(c.Files, "weed-volume")
				delete(c.Files, "native.tar")
			case "chaos":
				c.Chaos = true
			case "replication":
				c.Replication = "001"
			case "chaos-set":
				c.ChaosSet = "A"
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if _, err := RunCloudHTTP(ctx, inv, c); err == nil || !strings.Contains(err.Error(), "master writes requires") {
				t.Fatalf("mode%s: %v", mode, err)
			}
		})
	}
}

func TestMainPIDReadbackSudoAndFailure(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "sudo"), []byte("#!/bin/sh\ntest \"$1\" = -n || exit 90\nshift\nexec \"$@\"\n"), 0700)
	os.WriteFile(filepath.Join(dir, "systemctl"), []byte(fmt.Sprintf("#!/bin/sh\ncase $READBACK in failed) exit 23;; empty) exit 0;; esac\necho %d\n", os.Getpid())), 0700)
	for _, mode := range []string{"good", "failed", "empty"} {
		t.Run(mode, func(t *testing.T) {
			cmd := exec.Command("sh", "-ec", mainPID("unit")+"echo VERIFIED")
			cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "READBACK="+mode)
			out, err := cmd.CombinedOutput()
			if (err == nil) != (mode == "good") || strings.Contains(string(out), "VERIFIED") != (mode == "good") {
				t.Fatalf("mode%s err%v output%s", mode, err, out)
			}
		})
	}
}

// Execute the actual generated leader predicate against the saved real product
// schemas over HTTP; only replace the retry sleep by a one-poll refusal.
func TestMasterReadyRealHTTPResponseSchema(t *testing.T) {
	raw, err := os.ReadFile("testdata/master-leader-real-response.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture map[string]json.RawMessage
	if err = json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	old := masterWriteSettle
	masterWriteSettle = 0
	defer func() { masterWriteSettle = old }()
	for _, mode := range []string{"leader", "not-leader", "wrong-schema", "string-true"} {
		t.Run(mode, func(t *testing.T) {
			requested := make(chan string, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requested <- r.URL.Path
				if r.URL.Path != "/cluster/status" {
					w.Write(fixture["dir_status"])
					return
				}
				switch mode {
				case "not-leader":
					fmt.Fprint(w, `{"IsLeader":false}`)
				case "wrong-schema":
					w.Write(fixture["dir_status"])
				case "string-true":
					fmt.Fprint(w, `{"IsLeader":"true"}`)
				default:
					w.Write(fixture["cluster_status"])
				}
			}))
			defer server.Close()
			u, _ := url.Parse(server.URL)
			port, _ := strconv.Atoi(u.Port())
			var d inventory.Document
			json.Unmarshal(directInput(t, "127.0.0.1:22"), &d)
			h := d.Hosts[0]
			h.Data.Address = u.Hostname()
			for i := range h.Services {
				if h.Services[i].Name == "master_http" {
					h.Services[i].Port = port
				}
			}
			bin := t.TempDir()
			os.WriteFile(filepath.Join(bin, "sudo"), []byte("#!/bin/sh\ntest \"$1\" = -n || exit 90\nshift\nexec \"$@\"\n"), 0700)
			os.WriteFile(filepath.Join(bin, "systemctl"), []byte(fmt.Sprintf("#!/bin/sh\necho %d\n", os.Getpid())), 0700)
			c := writeCfg()
			c.Transport = "http"
			config, _ := json.Marshal(c)
			observed := false
			stop := errors.New("test stops after real readiness probe")
			_, got := runDirectMasterWrites(context.Background(), d, h, CloudConfig{}, []string{"-I", "worker", "--config", string(config)}, func(name, cmd string) (string, error) {
				switch name {
				case "master-kill":
					return "104.5 OLD 1 1 GONE", nil
				case "master-ready":
					observed = true
					cmd = strings.Replace(cmd, "do sleep 1; done", "do exit 77; done", 1)
					call := exec.Command("sh", "-ec", cmd)
					call.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
					out, err := call.CombinedOutput()
					if (err == nil) != (mode == "leader") {
						t.Errorf("mode%s: %v %s", mode, err, out)
					}
					return "", stop
				}
				return "", nil
			})
			path := ""
			select {
			case path = <-requested:
			default:
			}
			if !observed || !errors.Is(got, stop) || path != "/cluster/status" {
				t.Fatalf("probe=%v error=%v URL=%s", observed, got, path)
			}
		})
	}
}
