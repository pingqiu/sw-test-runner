package inventory_test

import (
	"crypto/sha256"
	"encoding/hex"
	"github.com/pingqiu/sw-test-runner/inventory"
	"strings"
	"testing"
)

func TestCaseExactFieldsAtDecodeAndJoin(t *testing.T) {
	for _, c := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"envelope override", func(d map[string]any) { d["transport"] = "typo"; d["Transport"] = "http" }},
		{"nested override", func(d map[string]any) { firstHost(d)["disks"].([]any)[0].(map[string]any)["Format_Allowed"] = true }},
		{"noncanonical optional", func(d map[string]any) { firstHost(d)["rdma"].(map[string]any)["Device"] = "" }},
	} {
		t.Run(c.name, func(t *testing.T) {
			req, obs := pair(t)
			d := fixture(t)
			c.change(d)
			if _, err := inventory.Decode(bytesOf(t, d)); err == nil {
				t.Fatal("case alias accepted at Decode")
			}
			if _, err := inventory.Join(bytesOf(t, d), bytesOf(t, obs)); err == nil {
				t.Fatal("case alias accepted as requested Join input")
			}
			c.change(obs)
			if _, err := inventory.Join(req, bytesOf(t, obs)); err == nil {
				t.Fatal("case alias accepted as resolved Join input")
			}
		})
	}
}
func TestRequestedRDMAOptionalNullAgreement(t *testing.T) {
	for _, key := range []string{"device", "port", "gid", "link_layer", "active_mtu", "fabric_evidence_sha256"} {
		t.Run(key, func(t *testing.T) {
			d := fixture(t)
			d["transport"] = "dc"
			firstHost(d)["services"] = append(firstHost(d)["services"].([]any), map[string]any{"name": "rdma", "port": float64(18516)})
			r := map[string]any{"status": "requested", "capabilities": []any{"dc"}}
			firstHost(d)["rdma"] = r
			if _, err := inventory.Decode(bytesOf(t, d)); err != nil {
				t.Fatalf("omitted optional: %v", err)
			}
			r[key] = nil
			if _, err := inventory.Decode(bytesOf(t, d)); err == nil {
				t.Fatalf("explicit null %s accepted", key)
			}
		})
	}
	// Preserve explicitly nullable declarations in the published schema.
	d := fixture(t)
	firstHost(d)["rdma"].(map[string]any)["capabilities"] = nil
	if _, err := inventory.Decode(bytesOf(t, d)); err != nil {
		t.Fatal(err)
	}
	d["transport"] = "dc"
	firstHost(d)["services"] = append(firstHost(d)["services"].([]any), map[string]any{"name": "rdma", "port": float64(18516)})
	firstHost(d)["rdma"] = map[string]any{"status": "requested", "capabilities": []any{"dc"}, "gid_index": nil}
	if _, err := inventory.Decode(bytesOf(t, d)); err != nil {
		t.Fatal(err)
	}
}

func aliasAfterCanonical(raw []byte, nested bool) []byte {
	if nested {
		return []byte(strings.Replace(string(raw), `"format_allowed":false`, `"format_allowed":false,"Format_Allowed":true`, 1))
	}
	return []byte(strings.Replace(string(raw), `"transport":"http"`, `"transport":"typo","Transport":"http"`, 1))
}
func TestReviewOverrideWireOrder(t *testing.T) {
	for _, nested := range []bool{false, true} {
		t.Run(map[bool]string{false: "envelope", true: "nested"}[nested], func(t *testing.T) {
			if d, err := inventory.Decode(aliasAfterCanonical(bytesOf(t, fixture(t)), nested)); err == nil {
				t.Fatalf("wire override accepted: %+v", d)
			}
		})
	}
}
func TestJoinRejectsCaseAliasInMatchingPair(t *testing.T) {
	for _, nested := range []bool{false, true} {
		t.Run(map[bool]string{false: "envelope", true: "nested"}[nested], func(t *testing.T) {
			_, obs := pair(t)
			req := aliasAfterCanonical(bytesOf(t, fixture(t)), nested)
			sum := sha256.Sum256(req)
			obs["observation"].(map[string]any)["requested_sha256"] = hex.EncodeToString(sum[:])
			if _, err := inventory.Join(req, aliasAfterCanonical(bytesOf(t, obs), nested)); err == nil {
				t.Fatal("matching case-aliased request/resolution accepted")
			}
		})
	}
}
