package v3block

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	tr "github.com/pingqiu/sw-test-runner"
)

func TestR4DeadDisconnectPrecedesStopReconnectAndReadback(t *testing.T) {
	for _, rf := range []string{"3", "2"} {
		for _, disconnectFails := range []bool{false, true} {
			t.Run(fmt.Sprintf("rf=%s/disconnectFails=%v", rf, disconnectFails), func(t *testing.T) {
				s, err := tr.ParseFile("../../scenarios/v3block/r4-node-loss.yaml")
				if err != nil {
					t.Fatal(err)
				}
				s.Env["run_id"] = "disconnect-test"
				s.Env["rf"] = rf
				dir := t.TempDir()
				log := filepath.Join(dir, "nvme-argv")
				r4File(t, filepath.Join(dir, "sudo"), "#!/bin/sh\nexec \"$@\"\n")
				r4File(t, filepath.Join(dir, "nvme"), `#!/bin/sh
printf '%s\n' "$*" >> "$R4_DISCONNECT_LOG"
[ "$#" = 3 ] && [ "$1" = disconnect ] && [ "$2" = -n ] && [ "$3" = "$R4_EXPECT_NQN" ] || exit 99
[ "$R4_DISCONNECT_FAILS" != true ] || exit 19
`)
				var order []string
				fault, disconnected, stopped := false, false, false
				handler := func(_ context.Context, _ *tr.ActionContext, a tr.Action) (map[string]string, error) {
					cmd := a.Params["cmd"]
					if strings.Contains(cmd, "cleanup $T") {
						return nil, nil
					}
					if a.Action == "cloud_vm_power" {
						fault = true
						order = append(order, "poweroff")
					}
					if fault && strings.Contains(cmd, "nvme disconnect") {
						order = append(order, "disconnect")
						wantCmd := `NQN=nqn.2026-11.io.seaweedfs:r4-disconnect-test-rf` + rf + `; sudo nvme disconnect -n "$NQN"`
						if cmd != wantCmd {
							return nil, fmt.Errorf("wrong documented disconnect command: %s", cmd)
						}
						if runtime.GOOS == "linux" {
							shell := exec.Command("bash", "-c", cmd)
							shell.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"),
								"R4_DISCONNECT_LOG="+log, "R4_EXPECT_NQN=nqn.2026-11.io.seaweedfs:r4-disconnect-test-rf"+rf,
								fmt.Sprintf("R4_DISCONNECT_FAILS=%v", disconnectFails))
							if out, err := shell.CombinedOutput(); err != nil {
								return nil, fmt.Errorf("dead-controller disconnect refused: %w %s", err, out)
							}
							if got := strings.TrimSpace(mustRead(t, log)); got != "disconnect -n nqn.2026-11.io.seaweedfs:r4-disconnect-test-rf"+rf {
								return nil, fmt.Errorf("wrong disconnect argv: %s", got)
							}
						} else if disconnectFails {
							return nil, fmt.Errorf("dead-controller disconnect refused")
						}
						disconnected = true
					}
					if strings.Contains(cmd, "r4.sh writer-stop $T") {
						if !disconnected {
							return nil, fmt.Errorf("writer stop before dead-controller disconnect")
						}
						stopped = true
						order = append(order, "writer-stop")
					}
					if fault && strings.Contains(cmd, "r4.sh connect ") {
						if !disconnected || !stopped {
							return nil, fmt.Errorf("reconnect before disconnect and stop")
						}
						order = append(order, "reconnect")
					}
					if fault && strings.Contains(cmd, "auto-p2.py verify ") {
						order = append(order, "readback")
					}
					if strings.Contains(cmd, "auto-p2.py wait ") {
						return map[string]string{"value": "test-promotion-token"}, nil
					}
					return nil, nil
				}
				reg := tr.NewRegistry()
				for _, name := range []string{"exec", "upload_path", "collect_path", "cloud_vm_power", "cloud_vm_power_restore"} {
					reg.RegisterFunc(name, tr.TierCore, handler)
				}
				result := tr.NewEngine(reg, nil).Run(context.Background(), s, &tr.ActionContext{Scenario: s, Vars: map[string]string{}, Log: func(string, ...interface{}) {}})
				want := "poweroff disconnect writer-stop reconnect readback"
				if disconnectFails {
					if result.Status != tr.StatusFail || strings.Join(order, " ") != "poweroff disconnect" || !strings.Contains(result.Error, "dead-controller disconnect refused") {
						t.Fatalf("disconnect failure did not gate later I/O: %s %v %s", result.Status, order, result.Error)
					}
				} else if result.Status != tr.StatusPass || strings.Join(order, " ") != want {
					t.Fatalf("wrong fault/reconnect order: %s %v %s", result.Status, order, result.Error)
				}
			})
		}
	}
}
