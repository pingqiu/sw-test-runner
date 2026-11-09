//go:build linux

package seaweed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pingqiu/sw-test-runner/inventory"
	"golang.org/x/crypto/ssh"
)

func TestMaintenanceNoCandidatesReadsDataBeforeCleanup(t *testing.T) {
	for _, mode := range []string{"exact", "loss", "wrong", "resurrected", "unreadable", "omitted", "malformed", "save-failed", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			reads, cleanupCalls := 0, 0
			remove := false
			serve := func(cmd string, ch ssh.Channel) uint32 {
				c, ok := maintenanceCommandConfig(maintenanceInner(cmd))
				if !ok {
					return 0
				}
				emit := func(v any) uint32 { b, _ := json.Marshal(v); fmt.Fprint(ch, string(b)); return 0 }
				switch c["stage"] {
				case "readback":
					reads++
					if mode == "unreadable" {
						return 73
					}
					if mode == "malformed" {
						fmt.Fprint(ch, "null")
						return 0
					}
					rows := []maintenanceData{{Key: "live", Observed: true, Status: 200, ActualLength: 3, ActualSHA256: strings.Repeat("a", 64)}, {Key: "deleted", Observed: true, Status: 404}}
					switch mode {
					case "loss":
						rows[0].Status = 404
					case "wrong":
						rows[0].ActualSHA256 = strings.Repeat("b", 64)
					case "resurrected":
						rows[1].Status = 200
					case "omitted":
						rows = rows[:1]
					}
					return emit(rows)
				case "preserve":
					return emit(map[string]any{"roots": map[string]any{}})
				case "cleanup":
					cleanupCalls++
					remove = c["remove"] == true
					if reads != 1 && mode != "deadline" {
						return 74
					}
					status := "STOPPED_EVIDENCE_RETAINED"
					if remove {
						status = "CLEAN"
					}
					return emit(map[string]any{"status": status, "removed": []string{}, "units": 0, "listeners": 0, "run_processes": 0})
				}
				return 75
			}
			a, pin, key, _ := cloudSSH(t, "", serve)
			var d inventory.Document
			json.Unmarshal(directInput(t, a), &d)
			d.Hosts[0].ID = "meta"
			d.Hosts[0].Management.HostKey = &pin
			c := CloudConfig{MaintenanceTask: "ec_vacuum", SSHKey: key, Output: t.TempDir(), HostKey: pin, HostKeys: map[string]string{"meta": pin}}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			hosts, nodes, release, err := chaosConnect(ctx, d, c, []string{"meta"})
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			m := newMaintenanceRuntime(ctx, d, c)
			m.budget = maintenanceBudget{work: time.Now().Add(2 * time.Minute), hard: time.Now().Add(3 * time.Minute)}
			m.bind(&chaosRun{dir: c.Output, nodes: nodes, docs: map[string]inventory.Document{"meta": d}}, hosts)
			m.manifest.Objects = []maintenanceObject{{Key: "live", SHA: strings.Repeat("a", 64), Length: 3}, {Key: "deleted", Deleted: true}}
			if mode == "deadline" {
				m.budget.work = time.Now().Add(time.Second)
			}
			if mode == "save-failed" {
				if err := os.Mkdir(filepath.Join(c.Output, "maintenance-no-candidates-verdict.json"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			err = m.noCandidates(ctx)
			if mode != "deadline" && reads != 1 {
				t.Fatalf("zero candidates skipped independent oracle: reads=%d error=%v", reads, err)
			}
			var outcome *MaintenanceOutcomeError
			if !errors.As(err, &outcome) {
				t.Fatalf("missing outcome: %v", err)
			}
			want := "UNKNOWN"
			if mode == "exact" {
				want = "NOT_COVERED"
			}
			if mode == "loss" || mode == "wrong" || mode == "resurrected" {
				want = "FAIL"
			}
			if outcome.Status != want {
				t.Fatalf("want %s: %v", want, err)
			}
			if m.retain != (mode != "exact") {
				t.Fatalf("unsafe retention: %s retain=%t", mode, m.retain)
			}
			if mode != "deadline" && mode != "save-failed" {
				if _, err := os.ReadFile(filepath.Join(c.Output, "maintenance-no-candidates-verdict.json")); err != nil {
					t.Fatal(err)
				}
			}
			cleanupErr := m.cleanupHost(ctx, maintenanceHostGroup{Host: hosts["meta"]})
			if cleanupCalls != 1 || remove != (mode == "exact") || (cleanupErr == nil) != (mode == "exact") {
				t.Fatalf("oracle/cleanup join: mode=%s remove=%t calls=%d err=%v", mode, remove, cleanupCalls, cleanupErr)
			}
		})
	}
}
