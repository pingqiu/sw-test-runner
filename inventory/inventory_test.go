package inventory_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/pingqiu/sw-test-runner/inventory"
)

func TestUnknownTransportNeverSelectsHTTP(t *testing.T) {
	_, err := inventory.Decode([]byte(`{"schema":"seaweed.inventory/v1","state":"requested","run_id":"trial","target":"onprem","transport":"rdma"}`))
	if err == nil || !strings.Contains(err.Error(), "transport") {
		t.Fatalf("want explicit transport refusal, got %v", err)
	}
}

func fixture(t *testing.T) map[string]any {
	t.Helper()
	data, err := os.ReadFile("testdata/http.requested.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err = json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}
func bytesOf(t *testing.T, d map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func firstHost(d map[string]any) map[string]any { return d["hosts"].([]any)[0].(map[string]any) }
func TestRequestedHTTPAndRefusals(t *testing.T) {
	good := fixture(t)
	if _, err := inventory.Decode(bytesOf(t, good)); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(map[string]any){
		"schema":             func(d map[string]any) { d["schema"] = "seaweed.inventory/v2" },
		"state":              func(d map[string]any) { d["state"] = "approved" },
		"transport required": func(d map[string]any) { delete(d, "transport") },
		"resources required": func(d map[string]any) { delete(d, "managed_resource_ids") },
		"null resources":     func(d map[string]any) { d["managed_resource_ids"] = nil },
		"unknown field":      func(d map[string]any) { d["go"] = true },
		"role":               func(d map[string]any) { firstHost(d)["roles"] = []any{"admin"} },
		"duplicate role":     func(d map[string]any) { firstHost(d)["roles"] = []any{"master", "master"} },
		"endpoint command":   func(d map[string]any) { firstHost(d)["management"].(map[string]any)["address"] = "-oProxyCommand=bad" },
		"credential path": func(d map[string]any) {
			firstHost(d)["management"].(map[string]any)["credential_ref"] = "/home/operator/key"
		},
		"host key required":          func(d map[string]any) { delete(firstHost(d)["management"].(map[string]any), "host_key") },
		"run root traversal":         func(d map[string]any) { firstHost(d)["run_root"] = "/data/../etc" },
		"format permission required": func(d map[string]any) { delete(firstHost(d)["disks"].([]any)[0].(map[string]any), "format_allowed") },
		"wildcard disk": func(d map[string]any) {
			firstHost(d)["disks"].([]any)[0].(map[string]any)["device_id"] = "/dev/disk/by-id/*"
		},
		"implicit RDMA":    func(d map[string]any) { firstHost(d)["rdma"] = map[string]any{} },
		"HTTP marked RDMA": func(d map[string]any) { firstHost(d)["rdma"] = map[string]any{"status": "observed"} },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			d := fixture(t)
			change(d)
			if _, err := inventory.Decode(bytesOf(t, d)); err == nil {
				t.Fatal("invalid declaration accepted")
			}
		})
	}
}

func TestStrictByteAndIdentityRefusals(t *testing.T) {
	good := bytesOf(t, fixture(t))
	cases := map[string][]byte{
		"duplicate key":  []byte(strings.Replace(string(good), `"transport":"http"`, `"transport":"http","transport":"http"`, 1)),
		"trailing value": append(append([]byte{}, good...), []byte(` {}`)...),
		"invalid UTF8":   {0xff},
		"too large":      []byte(strings.Repeat(" ", inventory.MaxBytes) + string(good)),
	}
	for name, b := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := inventory.Decode(b); err == nil {
				t.Fatal("invalid bytes accepted")
			}
		})
	}
	for _, address := range []string{"0.0.0.0", "224.0.0.1"} {
		t.Run(address, func(t *testing.T) {
			d := fixture(t)
			firstHost(d)["management"].(map[string]any)["address"] = address
			if _, err := inventory.Decode(bytesOf(t, d)); err == nil {
				t.Fatal("invalid endpoint accepted")
			}
		})
	}
}
func pair(t *testing.T) ([]byte, map[string]any) {
	t.Helper()
	req := bytesOf(t, fixture(t))
	resolved := fixture(t)
	resolved["state"] = "resolved"
	h := firstHost(resolved)
	h["management"].(map[string]any)["host_key"] = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEB"
	h["data"].(map[string]any)["nic"] = "eth0"
	h["data"].(map[string]any)["mtu"] = float64(1500)
	h["disks"].([]any)[0].(map[string]any)["device_id"] = "/dev/disk/by-id/example-cache"
	digest := sha256.Sum256(req)
	resolved["observation"] = map[string]any{"requested_sha256": hex.EncodeToString(digest[:]), "hosts": []any{map[string]any{"host_id": "node1", "boot_id": "11111111-1111-1111-1111-111111111111", "captured_at": "2026-01-01T00:00:00Z", "evidence_sha256": strings.Repeat("b", 64)}}}
	return req, resolved
}
func TestJoinBindsRequestAndNeverExpandsAuthority(t *testing.T) {
	req, good := pair(t)
	if _, err := inventory.Join(req, bytesOf(t, good)); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(map[string]any){
		"foreign request digest": func(d map[string]any) {
			d["observation"].(map[string]any)["requested_sha256"] = strings.Repeat("c", 64)
		},
		"run changed":         func(d map[string]any) { d["run_id"] = "other" },
		"transport changed":   func(d map[string]any) { d["transport"] = "dc" },
		"artifact changed":    func(d map[string]any) { d["artifact_manifest_sha256"] = strings.Repeat("d", 64) },
		"endpoint changed":    func(d map[string]any) { firstHost(d)["management"].(map[string]any)["address"] = "192.0.2.11" },
		"credential changed":  func(d map[string]any) { firstHost(d)["management"].(map[string]any)["credential_ref"] = "another-key" },
		"root changed":        func(d map[string]any) { firstHost(d)["run_root"] = "/data/nvme/other" },
		"role expanded":       func(d map[string]any) { firstHost(d)["roles"] = []any{"master", "filer", "volume", "client"} },
		"format expanded":     func(d map[string]any) { firstHost(d)["disks"].([]any)[0].(map[string]any)["format_allowed"] = true },
		"mount changed":       func(d map[string]any) { firstHost(d)["disks"].([]any)[0].(map[string]any)["mount"] = "/mnt/other" },
		"host removed":        func(d map[string]any) { d["hosts"] = []any{} },
		"missing host pin":    func(d map[string]any) { firstHost(d)["management"].(map[string]any)["host_key"] = "" },
		"missing observation": func(d map[string]any) { delete(d, "observation") },
		"foreign observation host": func(d map[string]any) {
			d["observation"].(map[string]any)["hosts"].([]any)[0].(map[string]any)["host_id"] = "foreign"
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			req, d := pair(t)
			change(d)
			if _, err := inventory.Join(req, bytesOf(t, d)); err == nil {
				t.Fatal("unbound resolution accepted")
			}
		})
	}
}

func TestRequiredRDMACannotUseHTTPOrIncompleteObservation(t *testing.T) {
	for _, transport := range []string{"rc", "dc"} {
		t.Run(transport, func(t *testing.T) {
			requested := fixture(t)
			requested["transport"] = transport
			firstHost(requested)["rdma"] = map[string]any{"status": "requested", "capabilities": []any{transport}}
			raw := bytesOf(t, requested)
			if _, err := inventory.Decode(raw); err != nil {
				t.Fatal(err)
			}
			_, resolved := pair(t)
			resolved["transport"] = transport
			rdma := map[string]any{"status": "observed", "device": "mlx5_0", "port": float64(1), "gid_index": float64(3), "gid": "00000000000000000000ffff0a000001", "link_layer": "ethernet", "active_mtu": float64(4096), "capabilities": []any{transport}, "fabric_evidence_sha256": strings.Repeat("c", 64)}
			firstHost(resolved)["rdma"] = rdma
			digest := sha256.Sum256(raw)
			resolved["observation"].(map[string]any)["requested_sha256"] = hex.EncodeToString(digest[:])
			if _, err := inventory.Join(raw, bytesOf(t, resolved)); err != nil {
				t.Fatal(err)
			}
			for _, field := range []string{"device", "port", "gid_index", "gid", "link_layer", "active_mtu", "fabric_evidence_sha256"} {
				t.Run("missing_"+field, func(t *testing.T) {
					saved := rdma[field]
					delete(rdma, field)
					if _, err := inventory.Decode(bytesOf(t, resolved)); err == nil {
						t.Fatal("incomplete required RDMA accepted")
					}
					rdma[field] = saved
				})
			}
			rdma["status"] = "not_applicable"
			if _, err := inventory.Decode(bytesOf(t, resolved)); err == nil {
				t.Fatal("required RDMA silently became HTTP")
			}
		})
	}
}
func TestPinnedDiskAndRDMAPrescriptionCannotChange(t *testing.T) {
	requested := fixture(t)
	requested["transport"] = "dc"
	h := firstHost(requested)
	h["rdma"] = map[string]any{"status": "requested", "device": "mlx5_0", "capabilities": []any{"dc"}}
	h["disks"].([]any)[0].(map[string]any)["device_id"] = "/dev/disk/by-id/approved"
	raw := bytesOf(t, requested)
	_, resolved := pair(t)
	resolved["transport"] = "dc"
	firstHost(resolved)["rdma"] = map[string]any{"status": "observed", "device": "mlx5_1", "port": float64(1), "gid_index": float64(3), "gid": "00000000000000000000ffff0a000001", "link_layer": "ethernet", "active_mtu": float64(4096), "capabilities": []any{"dc"}, "fabric_evidence_sha256": strings.Repeat("c", 64)}
	digest := sha256.Sum256(raw)
	resolved["observation"].(map[string]any)["requested_sha256"] = hex.EncodeToString(digest[:])
	if _, err := inventory.Join(raw, bytesOf(t, resolved)); err == nil {
		t.Fatal("prescribed selection changed")
	}
}

func TestSameCloudResourceCannotMasqueradeAsTwoHosts(t *testing.T) {
	d := fixture(t)
	d["target"] = "azure"
	id := "/subscriptions/example/resourceGroups/test/providers/Microsoft.Compute/virtualMachines/node1"
	d["managed_resource_ids"] = []any{id}
	firstHost(d)["resource_id"] = id
	other := fixture(t)
	h := firstHost(other)
	h["id"] = "node2"
	h["resource_id"] = id
	h["management"].(map[string]any)["address"] = "192.0.2.11"
	h["data"].(map[string]any)["address"] = "192.0.2.11"
	d["hosts"] = append(d["hosts"].([]any), h)
	if _, err := inventory.Decode(bytesOf(t, d)); err == nil {
		t.Fatal("same VM counted as distinct hosts")
	}
}
func TestTwoDiskNamesCannotAliasOnePhysicalDevice(t *testing.T) {
	d := fixture(t)
	h := firstHost(d)
	disk := h["disks"].([]any)[0].(map[string]any)
	disk["device_id"] = "/dev/disk/by-id/shared"
	h["disks"] = append(h["disks"].([]any), map[string]any{"id": "alias", "device_id": "/dev/disk/by-id/shared", "mount": "/mnt/alias", "format_allowed": false})
	if _, err := inventory.Decode(bytesOf(t, d)); err == nil {
		t.Fatal("same device counted twice")
	}
}
