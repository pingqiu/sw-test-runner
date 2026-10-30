//go:build linux

package seaweed

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/infra"
	"github.com/pingqiu/sw-test-runner/inventory"
)

// Static build data: no address, key, boot, UID or deployment-descriptor hash.
func fixturePayload(f *cpuFixture) string {
	files := []map[string]any{}
	for _, d := range f.policy.Deployments {
		for _, file := range d.Files {
			if contains(file.Roles, "product") || contains(file.Roles, "native") {
				files = append(files, map[string]any{"node": d.NodeID, "path": file.Path, "sha256": file.SHA256})
			}
		}
	}
	raw, _ := json.Marshal(map[string]any{"version": "seaweed.payload/v1", "files": files})
	return string(raw)
}

func setPayloadInventory(t *testing.T, f *cpuFixture) {
	t.Helper()
	var req, obs inventory.Document
	json.Unmarshal([]byte(f.plan.Requested), &req)
	json.Unmarshal([]byte(f.plan.Resolved), &obs)
	req.ArtifactManifestSHA256 = hash([]byte(f.plan.Payload))
	obs.ArtifactManifestSHA256 = req.ArtifactManifestSHA256
	raw, _ := json.Marshal(req)
	out, err := inventory.Resolve(raw, obs)
	if err != nil {
		t.Fatal(err)
	}
	f.plan.Requested, f.plan.Resolved = string(raw), string(out)
}

func TestPayloadStrictEntryStaticIdentityAndCoverage(t *testing.T) {
	for _, mode := range []string{"static", "changed", "missing", "duplicate", "foreign", "empty", "legacy", "archive-only", "policy-extra", "readback-changed", "digest", "host", "key", "boot", "uid", "tree"} {
		t.Run(mode, func(t *testing.T) {
			f := newCPUFixture(t)
			f.plan.Payload = fixturePayload(f)
			setPayloadInventory(t, f)
			original := f.plan.Requested
			var p map[string]any
			json.Unmarshal([]byte(f.plan.Payload), &p)
			files := p["files"].([]any)
			switch mode {
			case "changed":
				files[0].(map[string]any)["sha256"] = strings.Repeat("1", 64)
			case "missing":
				p["files"] = files[1:]
			case "duplicate":
				p["files"] = append(files, files[0])
			case "foreign":
				files[0].(map[string]any)["node"] = "foreign"
			case "empty":
				p["files"] = []any{}
			case "archive-only":
				files[0].(map[string]any)["path"] = "/opt/seaweed/native.tar.gz"
			case "policy-extra":
				d := &f.policy.Deployments[0]
				extra := d.Files[3]
				extra.Path += "-extra"
				d.Files = append(d.Files, extra)
				f.binding.Artifacts = f.write(t, "artifacts.json", tr.ApprovedArtifacts{Version: "s0b-artifacts-v1", Deployments: f.policy.Deployments})
			case "digest":
				// Valid but byte-different P, without changing requested inventory.
			case "readback-changed", "host", "key", "boot", "uid", "tree":
				read := f.runtime.Readback
				f.runtime.Readback = func(ctx context.Context, d infra.DeploymentPolicy) (infra.DeploymentReceipt, error) {
					r, err := read(ctx, d)
					switch mode {
					case "host":
						r.Host = "192.0.2.99:22"
					case "key":
						r.HostKey = "foreign"
					case "boot":
						r.BootID = "foreign"
					case "uid":
						r.WorkloadUID++
					case "tree":
						return r, errors.New("readback refused foreign installation tree")
					default:
						r.Files["/opt/seaweed/product"] = strings.Repeat("1", 64)
					}
					return r, err
				}
			}
			raw, _ := json.Marshal(p)
			f.plan.Payload = string(raw)
			if mode == "digest" {
				f.plan.Payload += " "
			}
			if mode == "legacy" {
				f.plan.Payload = ""
			}
			// Re-approve malformed P and its matching inventory too: echoing a
			// claimed digest cannot substitute for the complete readback join.
			if mode != "static" && mode != "digest" {
				setPayloadInventory(t, f)
			}
			f.publish(t, nil)
			result, _, err := tr.RunApprovedRequest(context.Background(), f.request, f.profile, RegisterPack, &f.runtime, nil)
			pass := err == nil && result != nil && result.Status == tr.StatusPass
			if pass != (mode == "static") || mode != "static" && f.node.authorized != 0 {
				t.Fatalf("mode=%s pass=%v jobs=%d err=%v result=%+v", mode, pass, f.node.authorized, err, result)
			}
			if mode == "static" && f.plan.Requested != original {
				t.Fatal("requested bytes rewritten")
			}
		})
	}
}

func TestPayloadSameBytesAcrossDifferentApprovedHostDeployments(t *testing.T) {
	var payload, descriptor string
	for generation := 0; generation < 2; generation++ {
		f := newCPUFixture(t)
		if generation == 1 {
			var req, obs inventory.Document
			json.Unmarshal([]byte(f.plan.Requested), &req)
			json.Unmarshal([]byte(f.plan.Resolved), &obs)
			for i := range f.binding.Hosts {
				h := &f.binding.Hosts[i]
				h.Host = strings.Replace(h.Host, "192.0.2.", "198.51.100.", 1)
				h.HostKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIO/wV7u8XGl0w5JsN5y6N4xkPO6UOv+cgO0yQc1JpJxR"
				d := &f.policy.Deployments[i]
				d.Host, d.HostKey = h.Host, h.HostKey
				d.ExpectedBootID = "22222222-2222-2222-2222-222222222222"
				d.WorkloadUID = 1001
				f.node.boots[h.Node] = d.ExpectedBootID
				for _, doc := range []*inventory.Document{&req, &obs} {
					doc.Hosts[i].Management.Address = strings.Split(h.Host, ":")[0]
					key := h.HostKey
					doc.Hosts[i].Management.HostKey = &key
				}
				obs.Observation.Hosts[i].BootID = d.ExpectedBootID
				obs.Observation.Hosts[i].CapturedAt = time.Now().UTC().Format(time.RFC3339Nano)
			}
			f.binding.Inventory = f.write(t, "inventory.json", tr.ApprovedInventory{Version: "s0b-inventory-v1", Hosts: f.binding.Hosts})
			f.binding.Artifacts = f.write(t, "artifacts.json", tr.ApprovedArtifacts{Version: "s0b-artifacts-v1", Deployments: f.policy.Deployments})
			raw, _ := json.Marshal(req)
			resolved, err := inventory.Resolve(raw, obs)
			if err != nil {
				t.Fatal(err)
			}
			f.plan.Requested, f.plan.Resolved = string(raw), string(resolved)
		}
		if generation == 0 {
			payload, descriptor = f.plan.Payload, f.binding.Artifacts.SHA256
		}
		if f.plan.Payload != payload || generation == 1 && f.binding.Artifacts.SHA256 == descriptor {
			t.Fatal("fixture did not separate P and D")
		}
		original := f.plan.Requested
		f.publish(t, nil)
		result, _, err := tr.RunApprovedRequest(context.Background(), f.request, f.profile, RegisterPack, &f.runtime, nil)
		if err != nil || result == nil || result.Status != tr.StatusPass || f.node.authorized != 11 {
			t.Fatalf("generation=%d result=%+v err=%v jobs=%d", generation, result, err, f.node.authorized)
		}
		if f.plan.Requested != original {
			t.Fatal("original requested bytes changed")
		}
	}
}
