//go:build linux

package seaweed

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// Real SSH boundary; only product responses are synthetic here. The normal
// CLI/container run is separately required for product qualification.
type ecDirectHost struct {
	directHost
	seed EC1aManifest
	lost int
	mode string
}

func (h *ecDirectHost) exec(cmd string, c ssh.Channel) uint32 {
	if strings.Contains(cmd, "systemctl kill") {
		h.lost++
	}
	if strings.HasPrefix(cmd, "cat >") {
		io.Copy(io.Discard, c)
		return 0
	}
	if strings.Contains(cmd, "ec_setup.py") && strings.Contains(cmd, "ready") {
		h.mu.Lock()
		h.commands = append(h.commands, cmd)
		h.mu.Unlock()
		if h.mode == "failed-step" {
			return 1
		}
		fmt.Fprintln(c, `{"ready":true}`)
		return 0
	}
	if strings.Contains(cmd, "ec_chaos.py") {
		if h.lost > 0 && strings.Contains(cmd, "--route volume") && !strings.Contains(cmd, fmt.Sprintf(":%d", 8080+h.lost)) {
			return 1
		}
		if strings.Contains(cmd, "--stage seed") {
			json.NewEncoder(c).Encode(h.seed)
			return 0
		}
		body := ecReadBody{RunID: h.seed.RunID}
		route := "s3"
		if strings.Contains(cmd, "--route volume") {
			route = "volume"
		}
		for i, o := range h.seed.Objects {
			body.Outcomes = append(body.Outcomes, ecOutcome{chaosOp: chaosOp{Op: i, Kind: "s3_get", Key: o.Key, Started: 1, Ended: 2, Exact: true}, Route: route, Status: 200, Length: o.Length, SHA256: o.SHA256})
		}
		if strings.Contains(cmd, "--plant-wrong-byte") {
			v := body.Outcomes[0]
			body.Outcomes[0].Original = &v
			payload := ecPayload(h.seed.RunID, v.Key)
			payload[0] ^= 1
			body.Outcomes[0].Exact = false
			body.Outcomes[0].SHA256 = hash(payload)
			body.Wrong = 1
			body.Control = "WRONG_BYTE_REJECTED"
		} else {
			if h.mode == "missing-terminal" {
				body.Outcomes = body.Outcomes[:63]
			}
			if h.mode == "wrong-byte" {
				body.Outcomes[0].SHA256 = strings.Repeat("0", 64)
			}
		}
		json.NewEncoder(c).Encode(body)
		return 0
	}
	if strings.Contains(cmd, "ec_capture.py") {
		holders, shards := []any{}, []any{}
		for i := 0; i < 14; i++ {
			role := fmt.Sprint("h", i)
			holders = append(holders, map[string]any{"Job": role, "PID": 100 + i, "starttime": uint64(i + 1), "boot_id": "boot"})
			shards = append(shards, map[string]any{"Volume": "1", "Shard": i, "Job": role, "SHA256": strings.Repeat("a", 64), "Bytes": 1, "Generation": 1, "index_sha256": strings.Repeat("b", 64), "vif_sha256": strings.Repeat("c", 64)})
		}
		m := h.seed
		m.Generation = `{"1":1}`
		json.NewEncoder(c).Encode(map[string]any{"manifest": m, "original_absent": true, "holders": holders, "shards": shards, "files": []any{map[string]any{"Name": "x"}}})
		return 0
	}
	if strings.Contains(cmd, "cat /proc/sys/kernel/random/boot_id") && !strings.Contains(cmd, "systemctl") {
		fmt.Fprintln(c, "boot")
		return 0
	}
	return h.directHost.exec(cmd, c)
}
func runECFake(t *testing.T, mode string) (*ecDirectHost, error) {
	t.Helper()
	h := &ecDirectHost{mode: mode}
	h.dirty = mode == "extra-holder-port"
	address, pin, key, _ := cloudSSH(t, "", h.exec)
	inv := directInput(t, address)
	var d struct {
		RunID string `json:"run_id"`
	}
	json.Unmarshal(inv, &d)
	h.seed.RunID = d.RunID
	for i := 0; i < 64; i++ {
		k := fmt.Sprint("ec-", i)
		h.seed.Objects = append(h.seed.Objects, EC1aObject{Key: k, FID: "1,abcdef", Length: 262144, SHA256: hash(ecPayload(d.RunID, k)), ACK: true})
	}
	out := t.TempDir()
	p := filepath.Join(t.TempDir(), "weed")
	os.WriteFile(p, []byte("weed"), 0600)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, e := RunCloudHTTP(ctx, inv, CloudConfig{ECStage1: true, SSHKey: key, HostKey: pin, Output: out, Files: map[string]CloudFile{"weed": {Path: p, SHA256: hash([]byte("weed"))}}})
	return h, e
}
func TestDirectECMandatoryRowsAndCleanup(t *testing.T) {
	for _, mode := range []string{"positive", "failed-step", "missing-terminal", "wrong-byte", "extra-holder-port"} {
		t.Run(mode, func(t *testing.T) {
			h, e := runECFake(t, mode)
			if (e == nil) != (mode == "positive") {
				t.Fatalf("mode %s: %v", mode, e)
			}
			a, b, c := h.index("journalctl"), h.index("systemctl stop"), h.index("systemctl list-units")
			if a < 0 || b <= a || c <= b {
				t.Fatalf("cleanup missing/out of order: %d %d %d", a, b, c)
			}
			if !strings.Contains(h.commands[c], "8093|") || !strings.Contains(h.commands[c], "18093)") {
				t.Fatalf("fourteenth holder ports absent: %s", h.commands[c])
			}
			if h.index("kv_smoke") >= 0 {
				t.Fatal("EC ran KV phase")
			}
			prepare := h.commands[h.index("sudo install -d")]
			if strings.Contains(prepare, "/master") || strings.Contains(prepare, "/filer") {
				t.Fatal("prepare precreated exclusive EC service directories")
			}
		})
	}
}
