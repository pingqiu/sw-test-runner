//go:build linux

package seaweed

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pingqiu/sw-test-runner/inventory"
)

func TestECMetricsShellIdentityAndListener(t *testing.T) {
	if _, err := exec.LookPath("ss"); err != nil {
		t.Fatal("ss is required for the metrics boundary control")
	}
	var reads atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reads.Add(1)
		fmt.Fprintf(w, "%s{result=\"success\"} 7\n", ecMetric)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())
	boot, _ := os.ReadFile("/proc/sys/kernel/random/boot_id")
	stat, _ := os.ReadFile("/proc/self/stat")
	start, _ := strconv.ParseUint(strings.Fields(string(stat)[strings.LastIndex(string(stat), ") ")+2:])[19], 10, 64)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sudo"), []byte("#!/bin/sh\n[ \"$1\" = -n ] && shift\nexec \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"positive", "wrong-start", "wrong-boot", "foreign-listener"} {
		t.Run(mode, func(t *testing.T) {
			pid, stamp, b := os.Getpid(), start, strings.TrimSpace(string(boot))
			if mode == "wrong-start" {
				stamp++
			}
			if mode == "wrong-boot" {
				b = "foreign"
			}
			if mode == "foreign-listener" {
				pid = os.Getppid()
				s, e := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
				if e != nil {
					t.Fatal(e)
				}
				stamp, _ = strconv.ParseUint(strings.Fields(string(s)[strings.LastIndex(string(s), ") ")+2:])[19], 10, 64)
			}
			var snap ecSnapshot
			json.Unmarshal([]byte(fmt.Sprintf(`{"holders":[{"job":"h0","pid":%d,"starttime":%d,"boot_id":%q}]}`, pid, stamp, b)), &snap)
			h := inventory.Host{Services: []inventory.Service{{Name: "volume_http", Port: port - 20000}}}
			run := func(_ string, cmd string) (string, error) {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				c := exec.CommandContext(ctx, "sh", "-ec", cmd)
				c.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"))
				raw, err := c.CombinedOutput()
				return string(raw), err
			}
			before := reads.Load()
			v, err := ecScrape(h, snap, nil, mode, run)
			if mode == "positive" {
				if err != nil || v["h0"] != [2]float64{7, 0} || reads.Load() != before+1 {
					t.Fatalf("v=%v err=%v", v, err)
				}
			} else if err == nil || reads.Load() != before {
				t.Fatalf("identity refusal failed: v=%v err=%v requests=%d", v, err, reads.Load()-before)
			}
		})
	}
}
