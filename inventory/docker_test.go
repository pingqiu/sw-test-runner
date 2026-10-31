package inventory_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/pingqiu/sw-test-runner/inventory"
	"os"
	"testing"
)

func dockerFixture(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile("../examples/inventory/docker.requested.json")
	if err != nil {
		t.Fatal(err)
	}
	var d map[string]any
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	return d
}
func dockerPair(t *testing.T, rc bool) ([]byte, map[string]any) {
	t.Helper()
	d := dockerFixture(t)
	if rc {
		d["transport"] = "rc"
		p := d["docker"].(map[string]any)
		p["rdma_mode"] = "rxe"
		p["unavailable_transports"].([]any)[0].(map[string]any)["reason"] = "rxe_no_dc"
		for _, v := range d["hosts"].([]any) {
			h := v.(map[string]any)
			role := h["roles"].([]any)[0]
			if role == "client" || role == "volume" {
				h["rdma"] = map[string]any{"status": "requested", "capabilities": []any{"rc"}}
			}
			if role == "volume" {
				h["services"] = append(h["services"].([]any), map[string]any{"name": "rdma", "port": 18515}, map[string]any{"name": "rdma_control", "port": 18516})
			}
		}
	}
	req := bytesOf(t, d)
	var obs map[string]any
	if err := json.Unmarshal(req, &obs); err != nil {
		t.Fatal(err)
	}
	obs["state"] = "resolved"
	p := obs["docker"].(map[string]any)
	parent := p["parent"].(map[string]any)
	parent["boot_id"] = "11111111-1111-1111-1111-111111111111"
	parent["root_device"] = "2049"
	parent["root_inode"] = "8000"
	p["network"].(map[string]any)["id"] = fmt.Sprintf("%064x", 999)
	seen := []any{}
	for i, v := range obs["hosts"].([]any) {
		h := v.(map[string]any)
		h["management"].(map[string]any)["host_key"] = parent["management"].(map[string]any)["host_key"]
		h["data"].(map[string]any)["nic"] = "eth0"
		h["data"].(map[string]any)["mtu"] = float64(1500)
		c := h["container"].(map[string]any)
		c["id"] = fmt.Sprintf("%064x", i+1)
		c["netns_inode"] = fmt.Sprint(10000 + i)
		for _, m := range c["mounts"].([]any) {
			m.(map[string]any)["source_device"] = "2049"
			m.(map[string]any)["source_inode"] = fmt.Sprint(9000 + i)
		}
		r := h["rdma"].(map[string]any)
		if r["status"] == "requested" {
			h["rdma"] = map[string]any{"status": "observed", "capabilities": []any{"rc"}, "device": "rxe0", "port": float64(1), "gid_index": float64(0), "gid": fmt.Sprintf("%032x", i+1), "link_layer": "ethernet", "active_mtu": float64(1024), "fabric_evidence_sha256": fmt.Sprintf("%064x", 88)}
		}
		seen = append(seen, map[string]any{"host_id": h["id"], "boot_id": parent["boot_id"], "captured_at": "2026-10-30T00:00:00Z", "evidence_sha256": fmt.Sprintf("%064x", i+88)})
	}
	sum := sha256.Sum256(req)
	obs["observation"] = map[string]any{"requested_sha256": hex.EncodeToString(sum[:]), "hosts": seen}
	return req, obs
}
func TestDockerHTTPRequestedAndResolved(t *testing.T) {
	req, obs := dockerPair(t, false)
	if _, err := inventory.Decode(req); err != nil {
		t.Fatal(err)
	}
	if _, err := inventory.Decode(bytesOf(t, obs)); err != nil {
		t.Fatal(err)
	}
	if _, err := inventory.Join(req, bytesOf(t, obs)); err != nil {
		t.Fatal(err)
	}
}

func TestDockerRXEAndRefusals(t *testing.T) {
	req, obs := dockerPair(t, true)
	if _, err := inventory.Join(req, bytesOf(t, obs)); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(map[string]any){
		"missing profile":         func(d map[string]any) { delete(d, "docker") },
		"null profile":            func(d map[string]any) { d["docker"] = nil },
		"DC requested":            func(d map[string]any) { d["transport"] = "dc" },
		"DC N_A absent":           func(d map[string]any) { d["docker"].(map[string]any)["unavailable_transports"] = []any{} },
		"ambiguous software mode": func(d map[string]any) { d["docker"].(map[string]any)["rdma_mode"] = "hardware" },
		"wrong metadata store":    func(d map[string]any) { d["docker"].(map[string]any)["metadata_store"] = "leveldb2" },
		"foreign parent": func(d map[string]any) {
			d["hosts"].([]any)[0].(map[string]any)["container"].(map[string]any)["parent_id"] = "m02"
		},
		"privileged": func(d map[string]any) {
			d["hosts"].([]any)[0].(map[string]any)["container"].(map[string]any)["privileged"] = true
		},
		"missing image digest": func(d map[string]any) {
			delete(d["hosts"].([]any)[0].(map[string]any)["container"].(map[string]any), "image_digest")
		},
		"short container ID": func(d map[string]any) {
			d["hosts"].([]any)[0].(map[string]any)["container"].(map[string]any)["id"] = "1234"
		},
		"duplicate container ID": func(d map[string]any) {
			hs := d["hosts"].([]any)
			hs[1].(map[string]any)["container"].(map[string]any)["id"] = hs[0].(map[string]any)["container"].(map[string]any)["id"]
		},
		"shared netns": func(d map[string]any) {
			hs := d["hosts"].([]any)
			hs[1].(map[string]any)["container"].(map[string]any)["netns_inode"] = hs[0].(map[string]any)["container"].(map[string]any)["netns_inode"]
		},
		"missing parent generation": func(d map[string]any) { d["docker"].(map[string]any)["parent"].(map[string]any)["boot_id"] = "" },
		"short network ID":          func(d map[string]any) { d["docker"].(map[string]any)["network"].(map[string]any)["id"] = "1234" },
		"wrong observation parent": func(d map[string]any) {
			d["observation"].(map[string]any)["hosts"].([]any)[0].(map[string]any)["boot_id"] = "22222222-2222-2222-2222-222222222222"
		},
		"filers split metadata": func(d map[string]any) { d["hosts"].([]any)[3].(map[string]any)["metadata_host_id"] = "another-mysql" },
		"metadata missing":      func(d map[string]any) { d["docker"].(map[string]any)["metadata_host_id"] = "missing" },
		"foreign bind filesystem": func(d map[string]any) {
			m := d["hosts"].([]any)[5].(map[string]any)["container"].(map[string]any)["mounts"].([]any)[0].(map[string]any)
			m["source_device"] = "9999"
		},
		"parent root reused as cache": func(d map[string]any) {
			m := d["hosts"].([]any)[5].(map[string]any)["container"].(map[string]any)["mounts"].([]any)[0].(map[string]any)
			m["source_inode"] = d["docker"].(map[string]any)["parent"].(map[string]any)["root_inode"]
		},
		"outside bind source": func(d map[string]any) {
			m := d["hosts"].([]any)[5].(map[string]any)["container"].(map[string]any)["mounts"].([]any)[0].(map[string]any)
			m["source_path"] = "/data/someone-else"
		},
		"bind identity absent": func(d map[string]any) {
			m := d["hosts"].([]any)[5].(map[string]any)["container"].(map[string]any)["mounts"].([]any)[0].(map[string]any)
			m["source_inode"] = ""
		},
		"format": func(d map[string]any) {
			d["hosts"].([]any)[5].(map[string]any)["disks"].([]any)[0].(map[string]any)["format_allowed"] = true
		},
		"block device exposure": func(d map[string]any) {
			d["hosts"].([]any)[5].(map[string]any)["disks"].([]any)[0].(map[string]any)["device_id"] = "/dev/disk/by-id/physical-disk"
		},
		"rxe claims DC": func(d map[string]any) {
			d["hosts"].([]any)[5].(map[string]any)["rdma"].(map[string]any)["capabilities"] = []any{"rc", "dc"}
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			_, d := dockerPair(t, true)
			change(d)
			if _, err := inventory.Decode(bytesOf(t, d)); err == nil {
				t.Fatal("invalid Docker declaration accepted")
			}
		})
	}
}
func TestDockerTopologyCountsDistinctContainers(t *testing.T) {
	for _, role := range []string{"master", "filer", "volume", "client", "metadata"} {
		t.Run(role, func(t *testing.T) {
			d := dockerFixture(t)
			hosts := d["hosts"].([]any)
			kept := []any{}
			owned := d["managed_resource_ids"].([]any)
			removed := ""
			for _, v := range hosts {
				h := v.(map[string]any)
				if removed == "" && h["roles"].([]any)[0] == role {
					removed = h["resource_id"].(string)
					continue
				}
				kept = append(kept, h)
			}
			d["hosts"] = kept
			ids := []any{}
			for _, v := range owned {
				if v != removed {
					ids = append(ids, v)
				}
			}
			d["managed_resource_ids"] = ids
			if _, err := inventory.Decode(bytesOf(t, d)); err == nil {
				t.Fatal("missing topology member accepted")
			}
		})
	}
	d := dockerFixture(t)
	d["hosts"].([]any)[0].(map[string]any)["roles"] = []any{"master", "client"}
	if _, err := inventory.Decode(bytesOf(t, d)); err == nil {
		t.Fatal("multi-role container counted twice")
	}
}
func TestDockerJoinPreservesPlacementAndGeneration(t *testing.T) {
	changes := map[string]func(map[string]any){
		"image": func(d map[string]any) {
			d["hosts"].([]any)[0].(map[string]any)["container"].(map[string]any)["image_digest"] = "sha256:" + fmt.Sprintf("%064x", 999)
		},
		"parent executor": func(d map[string]any) {
			d["docker"].(map[string]any)["parent"].(map[string]any)["executor_sha256"] = fmt.Sprintf("%064x", 999)
		},
		"parent user": func(d map[string]any) {
			d["docker"].(map[string]any)["parent"].(map[string]any)["management"].(map[string]any)["user"] = "other"
		},
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			req, d := dockerPair(t, false)
			change(d)
			if _, err := inventory.Decode(bytesOf(t, d)); err != nil {
				t.Fatalf("control must be valid observed data: %v", err)
			}
			if _, err := inventory.Join(req, bytesOf(t, d)); err == nil {
				t.Fatal("changed placement accepted")
			}
		})
	}
	for _, field := range []string{"container", "netns", "network", "parent boot", "bind inode"} {
		t.Run(field, func(t *testing.T) {
			req, obs := dockerPair(t, false)
			var r map[string]any
			if err := json.Unmarshal(req, &r); err != nil {
				t.Fatal(err)
			}
			h := r["hosts"].([]any)[5].(map[string]any)
			o := obs["hosts"].([]any)[5].(map[string]any)
			switch field {
			case "container":
				h["container"].(map[string]any)["id"] = o["container"].(map[string]any)["id"]
				o["container"].(map[string]any)["id"] = fmt.Sprintf("%064x", 9999)
			case "netns":
				h["container"].(map[string]any)["netns_inode"] = o["container"].(map[string]any)["netns_inode"]
				o["container"].(map[string]any)["netns_inode"] = "9999"
			case "network":
				r["docker"].(map[string]any)["network"].(map[string]any)["id"] = obs["docker"].(map[string]any)["network"].(map[string]any)["id"]
				obs["docker"].(map[string]any)["network"].(map[string]any)["id"] = fmt.Sprintf("%064x", 9999)
			case "parent boot":
				r["docker"].(map[string]any)["parent"].(map[string]any)["boot_id"] = obs["docker"].(map[string]any)["parent"].(map[string]any)["boot_id"]
				b := "22222222-2222-2222-2222-222222222222"
				obs["docker"].(map[string]any)["parent"].(map[string]any)["boot_id"] = b
				for _, v := range obs["observation"].(map[string]any)["hosts"].([]any) {
					v.(map[string]any)["boot_id"] = b
				}
			case "bind inode":
				x := h["container"].(map[string]any)["mounts"].([]any)[0].(map[string]any)
				y := o["container"].(map[string]any)["mounts"].([]any)[0].(map[string]any)
				x["source_inode"] = y["source_inode"]
				y["source_inode"] = "9999"
			}
			req = bytesOf(t, r)
			sum := sha256.Sum256(req)
			obs["observation"].(map[string]any)["requested_sha256"] = hex.EncodeToString(sum[:])
			if _, err := inventory.Decode(bytesOf(t, obs)); err != nil {
				t.Fatalf("control must otherwise be valid: %v", err)
			}
			if _, err := inventory.Join(req, bytesOf(t, obs)); err == nil {
				t.Fatal("changed pinned generation accepted")
			}
		})
	}
}
func TestDockerExtensionDoesNotQualifyOtherTargets(t *testing.T) {
	for _, target := range []string{"onprem", "docker-sriov"} {
		d := dockerFixture(t)
		d["target"] = target
		if _, err := inventory.Decode(bytesOf(t, d)); err == nil {
			t.Fatal("undeclared target/adapter qualified")
		}
	}
}
