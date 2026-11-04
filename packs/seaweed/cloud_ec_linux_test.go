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
	seed   EC1aManifest
	lost   int
	killed map[int]bool
	mode   string
}

func (h *ecDirectHost) exec(cmd string, c ssh.Channel) uint32 {
	if h.mode == "rust-positive" && strings.Contains(cmd, "ec_service.py") && strings.Contains(cmd, `"role":`) {
		holder := strings.Contains(cmd, `"role":"h`)
		if holder && (!strings.Contains(cmd, `/weed-volume"`) || strings.Contains(cmd, `"-config_dir=`) || strings.Contains(cmd, `"volume",`)) {
			return 23
		}
		if !holder && !strings.Contains(cmd, `/weed"`) {
			return 23
		}
	}
	if h.mode == "rust-positive" && strings.Contains(cmd, "ec_capture.py") && strings.Contains(cmd, `"volume_exe":`) && !strings.Contains(cmd, `/weed-volume"`) {
		return 23
	}
	if strings.Contains(cmd, "systemctl kill") {
		h.lost++
		if h.killed == nil {
			h.killed = map[int]bool{}
		}
		for i := 0; i < 14; i++ {
			if strings.Contains(cmd, fmt.Sprintf("-h%d'", i)) {
				h.killed[i] = true
			}
		}
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
		port := 8080 + h.lost
		if strings.HasPrefix(h.mode, "under-") {
			for i := 0; i < 14; i++ {
				if !h.killed[i] {
					port = 8080 + i
					break
				}
			}
		}
		if h.lost > 0 && strings.Contains(cmd, "--route volume") && !strings.Contains(cmd, fmt.Sprintf(":%d", port)) {
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
		if h.lost == 5 && strings.HasPrefix(h.mode, "under-") {
			v := &body.Outcomes[0]
			v.Status = 503
			v.Exact = false
			v.Length = 0
			v.SHA256 = hash(nil)
			if h.mode == "under-target200" {
				v.Status = 200
				v.Exact = true
				v.Length = h.seed.Objects[0].Length
				v.SHA256 = h.seed.Objects[0].SHA256
			}
			if h.mode == "under-control500" {
				v = &body.Outcomes[1]
				v.Status = 503
				v.Exact = false
				v.Length = 0
				v.SHA256 = hash(nil)
			}
			if h.mode == "under-wrong200" {
				v = &body.Outcomes[0]
				v.Status = 200
				v.Exact = false
				v.Length = h.seed.Objects[0].Length
				v.SHA256 = strings.Repeat("0", 64)
				body.Wrong = 1
			}
		}
		json.NewEncoder(c).Encode(body)
		if h.lost == 5 && h.mode == "under-run-error" {
			return 23
		}
		return 0
	}
	if strings.Contains(cmd, "ec_capture.py") {
		holders, shards := []any{}, []any{}
		for i := 0; i < 14; i++ {
			role := fmt.Sprint("h", i)
			holders = append(holders, map[string]any{"Job": role, "PID": 100 + i, "starttime": uint64(i + 1), "boot_id": "boot"})
			shardRole := role
			if h.mode == "under-rotated" {
				shardRole = fmt.Sprint("h", (i+3)%14)
			}
			if h.mode == "five-holder-topology" && i >= 4 {
				shardRole = "h4"
			}
			shards = append(shards, map[string]any{"Volume": "1", "Shard": i, "Job": shardRole, "SHA256": strings.Repeat("a", 64), "Bytes": 1, "Generation": 1, "index_sha256": strings.Repeat("b", 64), "vif_sha256": strings.Repeat("c", 64)})
		}
		m := h.seed
		m.Generation = `{"1":1}`
		snapshot := map[string]any{"manifest": m, "original_absent": true, "holders": holders, "shards": shards, "files": []any{map[string]any{"Name": "x"}}}
		if strings.HasPrefix(h.mode, "under-") {
			intervals := map[string]any{}
			for i, o := range m.Objects {
				offset := int64(2<<20) + int64(i)*262200
				if i == 0 && h.mode != "under-no-target" {
					offset = 16
				}
				intervals[o.Key] = []int64{offset, 262200}
			}
			snapshot["superblock"] = "0300000000000000"
			snapshot["vif_versions"] = map[string]int{"3": 14, "4": 0}
			snapshot["layout"] = []int64{3, 2 << 20, 20 << 20}
			snapshot["intervals"] = intervals
		}
		json.NewEncoder(c).Encode(snapshot)
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
		h.seed.Objects = append(h.seed.Objects, EC1aObject{Key: k, FID: fmt.Sprintf("1,%xaabbccdd", i+1), Length: 262144, SHA256: hash(ecPayload(d.RunID, k)), ACK: true})
	}
	out := t.TempDir()
	p := filepath.Join(t.TempDir(), "weed")
	os.WriteFile(p, []byte("weed"), 0600)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	config := CloudConfig{ECStage1: true, SSHKey: key, HostKey: pin, Output: out, Files: map[string]CloudFile{"weed": {Path: p, SHA256: hash([]byte("weed"))}}}
	if strings.HasPrefix(mode, "under-") {
		json.Unmarshal([]byte(`{"ec_under_quorum":true}`), &config)
	}
	if strings.HasPrefix(mode, "rust-") || mode == "go-explicit" {
		impl := "rust"
		if mode == "rust-invalid" {
			impl = "bad"
		}
		if mode == "go-explicit" {
			impl = "go"
		}
		json.Unmarshal([]byte(fmt.Sprintf(`{"ec_volume_impl":%q}`, impl)), &config)
		if mode == "rust-positive" {
			v := filepath.Join(t.TempDir(), "weed-volume")
			os.WriteFile(v, []byte("rust"), 0600)
			config.Files["weed-volume"] = CloudFile{Path: v, SHA256: hash([]byte("rust"))}
		}
	}
	_, e := RunCloudHTTP(ctx, inv, config)
	return h, e
}
func TestDirectECMandatoryRowsAndCleanup(t *testing.T) {
	for _, mode := range []string{"positive", "failed-step", "missing-terminal", "wrong-byte", "extra-holder-port", "five-holder-topology"} {
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

func TestDirectECUnderQuorumCaller(t *testing.T) {
	for _, mode := range []string{"under-positive", "under-rotated", "under-target200", "under-control500", "under-wrong200", "under-no-target", "under-run-error"} {
		t.Run(mode, func(t *testing.T) {
			h, err := runECFake(t, mode)
			if (err == nil) != (mode == "under-positive" || mode == "under-rotated") {
				t.Errorf("%s err=%v", mode, err)
			}
			if mode == "under-wrong200" && !strings.Contains(fmt.Sprint(err), "P1") {
				t.Errorf("wrong200 not P1: %v", err)
			}
			if (mode == "under-positive" || mode == "under-rotated") && h.lost != 5 {
				t.Errorf("only%d holders killed, not9 survivors", h.lost)
			}
			if h.index("systemctl stop") < 0 || h.index("systemctl list-units") < 0 {
				t.Fatal("cleanup/postflight skipped")
			}
		})
	}
}

func TestDirectRustVolumeSelectorAndCleanup(t *testing.T) {
	for _, mode := range []string{"rust-positive", "rust-missing", "rust-invalid", "go-explicit"} {
		t.Run(mode, func(t *testing.T) {
			h, e := runECFake(t, mode)
			positive := mode == "rust-positive" || mode == "go-explicit"
			if (e == nil) != positive {
				t.Errorf("%s: %v", mode, e)
			}
			if positive && (h.lost != 4 || h.index("systemctl stop") < 0 || h.index("systemctl list-units") < 0) {
				t.Fatal("run/cleanup/postflight missing")
			}
			if !positive && (h.lost != 0 || h.index("sudo install -d") >= 0) {
				t.Fatal("invalid selector/artifact reached effects")
			}
		})
	}
}
