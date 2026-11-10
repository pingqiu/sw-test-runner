//go:build linux

package seaweed

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/pingqiu/sw-test-runner/inventory"
	"golang.org/x/crypto/ssh"
)

func TestMaintenanceTaskCallerUsesRemainingOriginalPhase(t *testing.T) {
	for _, remaining := range []time.Duration{570 * time.Second, 600 * time.Second, 26 * time.Second, -time.Second} {
		t.Run(remaining.String(), func(t *testing.T) {
			commands := make(chan string, 1)
			address, pin, key, _ := cloudSSH(t, "", func(cmd string, ch ssh.Channel) uint32 {
				commands <- cmd
				zero := 0
				b, _ := json.Marshal(maintenanceTaskResult{Job: "run-maintenance", Attempt: "1", Terminal: "succeeded", Exit: &zero, Started: 1, Ended: 2, Identity: maintenanceIdentity{PID: 1, Start: "1", Boot: "boot", Task: "volume.balance", RunID: "run", ConfigSHA: "config"}})
				fmt.Fprint(ch, string(b))
				return 0
			})
			var d inventory.Document
			json.Unmarshal(directInput(t, address), &d)
			d.RunID = "run"
			d.Deadlines = inventory.Deadlines{Work: time.Now().Add(1200 * time.Second).UTC().Format(time.RFC3339Nano), Hard: time.Now().Add(1500 * time.Second).UTC().Format(time.RFC3339Nano)}
			d.Hosts[0].ID = "meta"
			c := CloudConfig{SSHKey: key, HostKeys: map[string]string{"meta": pin}, Output: t.TempDir()}
			connect, cancelConnect := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancelConnect()
			hosts, nodes, release, err := chaosConnect(connect, d, c, []string{"meta"})
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			m := &maintenanceRuntime{d: d, c: CloudConfig{MaintenanceTask: "volume.balance"}, hosts: hosts, base: map[string]any{"config_sha256": "config"}}
			m.r = &chaosRun{dir: c.Output, nodes: nodes, docs: map[string]inventory.Document{"meta": d}, maintenance: m}
			deadline := time.Now().Add(remaining)
			ctx, cancel := context.WithDeadline(context.Background(), deadline)
			defer cancel()
			err = m.startTask(ctx)
			if remaining < 27*time.Second {
				if err == nil {
					t.Fatal("short phase did not refuse synchronously before task dispatch")
				}
				select {
				case cmd := <-commands:
					t.Fatalf("short phase dispatched %s", cmd)
				default:
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-m.done:
			case <-time.After(3 * time.Second):
				t.Fatal("task caller did not join")
			}
			if _, err = m.taskSnapshot(); err != nil {
				t.Fatalf("remaining original phase %s refused real step dispatch: %v", remaining, err)
			}
			cmd := <-commands
			read := func(pattern string) int {
				t.Helper()
				matches := regexp.MustCompile(pattern).FindAllStringSubmatch(cmd, -1)
				if len(matches) == 0 {
					t.Fatalf("missing lifetime %s in %s", pattern, cmd)
				}
				n, _ := strconv.Atoi(matches[len(matches)-1][1])
				return n
			}
			step := read(`timeout -k 5 (\d+) sh`)
			unit := read(`--property=RuntimeMaxSec=(\d+)`)
			task := read(`SW_MAINTENANCE_TASK_SECONDS=(\d+)`)
			if step > 590 || unit != step-10 || task != unit-10 || task <= 0 || time.Now().Add(time.Duration(step+5)*time.Second).After(deadline) {
				t.Fatalf("lifetimes escape original deadline: step=%d unit=%d task=%d deadline=%s", step, unit, task, deadline)
			}
			if got, _ := ctx.Deadline(); got != deadline {
				t.Fatal("original phase reset")
			}
		})
	}
}
