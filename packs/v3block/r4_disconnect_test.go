package v3block

import (
	"context"
	"fmt"
	"strings"
	"testing"

	tr "github.com/pingqiu/sw-test-runner"
)

func TestR4DeadDisconnectPrecedesStopReconnectAndReadback(t *testing.T) {
	for _, disconnectFails := range []bool{false, true} {
		t.Run(fmt.Sprintf("disconnectFails=%v", disconnectFails), func(t *testing.T) {
			s, err := tr.ParseFile("../../scenarios/v3block/r4-node-loss.yaml")
			if err != nil {
				t.Fatal(err)
			}
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
					if disconnectFails {
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
