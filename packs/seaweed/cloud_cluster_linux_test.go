//go:build linux

package seaweed

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/pingqiu/sw-test-runner/inventory"
	"golang.org/x/crypto/ssh"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func clusterInput(t *testing.T, fail bool, variants ...string) ([]byte, CloudConfig, *atomic.Int32) {
	variant := ""
	if len(variants) > 0 {
		variant = variants[0]
	}
	cfg := CloudConfig{Output: t.TempDir(), HostKeys: map[string]string{}, Files: map[string]CloudFile{}}
	for _, name := range []string{"weed", "weed-volume", "native.tar"} {
		p := filepath.Join(t.TempDir(), name)
		os.WriteFile(p, []byte(name), 0600)
		cfg.Files[name] = CloudFile{Path: p, SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(name)))}
	}
	var d inventory.Document
	address, _, _, _ := cloudSSH(t, "")
	json.Unmarshal(cloudInput(t, address), &d)
	base := d.Hosts[0]
	d.Hosts = nil
	d.ManagedResourceIDs = nil
	loads := &atomic.Int32{}
	for i, id := range []string{"meta", "vol1", "vol2", "vol3", "vol4", "vol5", "vol6", "client1", "client2", "client3"} {
		id, i := id, i
		snap := &atomic.Int32{}
		address, pin, key, _ := cloudSSH(t, "", func(cmd string, ch ssh.Channel) uint32 {
			if strings.HasPrefix(cmd, "cat >") {
				io.Copy(io.Discard, ch)
				return 0
			}
			if fail && id == "vol6" && strings.Contains(cmd, "systemd-run") {
				fmt.Fprint(ch.Stderr(), "volume failed")
				return 9
			}
			if (variant == "collect-timeout" || variant == "collect-starvation") && strings.Contains(cmd, "sudo journalctl") {
				os.WriteFile(filepath.Join(cfg.Output, "observed-collect-"+id), []byte("started"), 0600)
				for {
					time.Sleep(200 * time.Millisecond)
					if _, e := ch.Write([]byte(".")); e != nil {
						return 1
					}
				}
			}
			if strings.Contains(cmd, "cloud_batch.py") && strings.Contains(cmd, "python3") {
				loads.Add(1)
				for v := 1; v <= 6; v++ {
					fmt.Fprintf(ch, "{\"event\":\"OWNER\",\"volume\":\"%d\",\"url\":\"10.84.1.%d:8080\"}\n", v, v+10)
				}
				for k := 0; k < 512; k++ {
					for _, event := range []string{"ACK", "EXACT"} {
						digest := strings.Repeat("a", 64)
						if variant == "wrong-kv" && id == "client3" && k == 0 && event == "EXACT" {
							digest = strings.Repeat("c", 64)
						}
						fmt.Fprintf(ch, "{\"event\":\"%s\",\"key\":\"%s-%d\",\"length\":1048576,\"sha256\":\"%s\",\"fids\":[\"%d,abc\"]}\n", event, id, k, digest, k%6+1)
					}
				}
				for k := 0; k < 32; k++ {
					for _, event := range []string{"S3_ACK", "S3_EXACT"} {
						if variant == "missing-s3" && id == "client3" && k == 0 && event == "S3_EXACT" {
							continue
						}
						fmt.Fprintf(ch, "{\"event\":\"%s\",\"key\":\"%s-s3-%d\",\"length\":4194304,\"sha256\":\"%s\"}\n", event, id, k, strings.Repeat("b", 64))
					}
				}
				fmt.Fprintln(ch, `{"status":"PASS","ack":512,"exact":512,"wrong":0,"s3_ack":32,"s3_exact":32}`)
			}
			if strings.Contains(cmd, "curl -fsS --max-time 10") {
				if id == "meta" {
					metaSnapshot := snap.Add(1)
					nodes := []map[string]string{}
					for v := 1; v <= 6; v++ {
						if variant == "missing-before-owner" && metaSnapshot == 1 && v == 6 {
							continue
						}
						nodes = append(nodes, map[string]string{"Url": fmt.Sprintf("10.84.1.%d:8080", v+10)})
					}
					b, _ := json.Marshal(map[string]any{"Topology": map[string]any{"DataCenters": []any{map[string]any{"Racks": []any{map[string]any{"DataNodes": nodes}}}}}})
					ch.Write(b)
				} else {
					n := snap.Add(1)
					if variant == "idle-owner" && id == "vol6" {
						n = 1
					}
					fmt.Fprintf(ch, `{"Volumes":[{"Id":%d,"Size":%d,"FileCount":%d,"ReadOnly":%t}]}`, i, (n-1)*1048576, (n-1)*512, variant == "readonly-before" && id == "vol6" && n == 1)
				}
			}
			return 0
		})
		h := base
		h.ID = id
		h.ResourceID = "vm-" + id
		h.Data.Address = fmt.Sprintf("10.84.1.%d", 10+i)
		h.Management.Address, _, _ = net.SplitHostPort(address)
		_, port, _ := net.SplitHostPort(address)
		fmt.Sscan(port, &h.Management.Port)
		switch {
		case id == "meta":
			h.Roles = []string{"master", "filer", "s3"}
			h.Services = []inventory.Service{{Name: "master_http", Port: 9333}, {Name: "master_grpc", Port: 19333}, {Name: "filer_http", Port: 8888}, {Name: "filer_grpc", Port: 18888}, {Name: "s3_http", Port: 8333}}
		case strings.HasPrefix(id, "vol"):
			h.Roles = []string{"volume"}
			h.Services = []inventory.Service{{Name: "volume_http", Port: 8080}, {Name: "volume_grpc", Port: 18080}}
		default:
			h.Roles = []string{"client"}
			h.Services = []inventory.Service{}
		}
		cfg.SSHKey = key
		cfg.HostKeys[id] = pin
		d.Hosts = append(d.Hosts, h)
		d.ManagedResourceIDs = append(d.ManagedResourceIDs, h.ResourceID)
	}
	b, _ := json.Marshal(d)
	return b, cfg, loads
}

func TestCloudTenNodeEvidenceCannotLoseAFailedMember(t *testing.T) {
	for _, variant := range []string{"wrong-kv", "missing-s3", "idle-owner", "missing-client", "missing-pin", "collect-file"} {
		t.Run(variant, func(t *testing.T) {
			raw, c, _ := clusterInput(t, false, variant)
			if variant == "missing-client" {
				var d inventory.Document
				json.Unmarshal(raw, &d)
				d.Hosts = d.Hosts[:9]
				raw, _ = json.Marshal(d)
			}
			if variant == "missing-pin" {
				delete(c.HostKeys, "vol6")
			}
			if variant == "collect-file" {
				dir := filepath.Join(c.Output, "collect")
				os.Mkdir(dir, 0700)
				os.WriteFile(filepath.Join(dir, "meta"), []byte("not a directory"), 0600)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if _, e := RunCloudHTTP(ctx, raw, c); e == nil {
				t.Fatal("false PASS for " + variant)
			}
		})
	}
}

func TestCloudCollectionHasOneMinuteBudget(t *testing.T) {
	raw, c, _ := clusterInput(t, false, "collect-timeout")
	ctx, cancel := context.WithTimeout(context.Background(), 70*time.Second)
	defer cancel()
	start := time.Now()
	_, e := RunCloudHTTP(ctx, raw, c)
	if e == nil || time.Since(start) > 65*time.Second {
		t.Fatalf("unbounded collection: %v elapsed=%s", e, time.Since(start))
	}
}

func TestCloudTenNodeRunsEveryClientAndPreservesFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			raw, c, loads := clusterInput(t, fail)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			bundle, e := RunCloudHTTP(ctx, raw, c)
			if fail {
				if e == nil || loads.Load() != 0 {
					t.Fatalf("failed owner allowed load: %v %d", e, loads.Load())
				}
				return
			}
			if e != nil || bundle == "" || loads.Load() != 3 {
				t.Fatalf("%s %v loads=%d", bundle, e, loads.Load())
			}
		})
	}
}
