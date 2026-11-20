//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const settleVMID = "/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/rg-test/providers/Microsoft.Compute/virtualMachines/vm-test"

func settleEvent(operation, status, id, stamp string) string {
	return fmt.Sprintf(`{"category":{"value":"Administrative"},"operationName":{"value":%q},"status":{"value":%q},"operationId":%q,"resourceId":%q,"eventTimestamp":%q}`, operation, status, id, settleVMID, stamp)
}

func settleFake(t *testing.T, activity, resources, disks string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{"activity": activity, "resources": resources, "disks": disks} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	script := `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$SETTLE_FIXTURE/calls"
case "$1 $2" in
  'monitor activity-log') [ "$3" = list ]; name=activity;;
  'resource list') name=resources;;
  'disk list') name=disks;;
  *) exit 99;;
esac
[ ! -f "$SETTLE_FIXTURE/fail-$name" ] || exit 7
cat "$SETTLE_FIXTURE/$name"
`
	if err := os.WriteFile(filepath.Join(dir, "az"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SETTLE_FIXTURE", dir)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	return dir
}

func invokeSettle(t *testing.T) (map[string]any, int) {
	t.Helper()
	var out bytes.Buffer
	rc := run([]string{"settle", "--resource-group", "rg-test", "--vm", "vm-test", "--subscription", "11111111-1111-1111-1111-111111111111"}, &out)
	var result map[string]any
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatalf("output=%q: %v", out.String(), err)
	}
	return result, rc
}

func TestSettleCleanUsesOnlyThreeReadOnlyAzureQueries(t *testing.T) {
	activity := "[" + settleEvent("Microsoft.Compute/virtualMachines/delete", "Succeeded", "delete-1", "2026-11-20T18:00:00Z") + "]"
	dir := settleFake(t, activity, "[]", "[]")
	result, rc := invokeSettle(t)
	if rc != 0 || result["code"] != "SETTLED-CLEAN" {
		t.Fatalf("rc=%d result=%v", rc, result)
	}
	calls, err := os.ReadFile(filepath.Join(dir, "calls"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(calls)), "\n")
	if len(lines) != 3 {
		t.Fatalf("calls=%q", calls)
	}
	for i, prefix := range []string{"monitor activity-log list ", "resource list ", "disk list "} {
		if !strings.HasPrefix(lines[i], prefix) || !strings.Contains(lines[i], "--resource-group rg-test") || !strings.Contains(lines[i], "--subscription 11111111-1111-1111-1111-111111111111") {
			t.Fatalf("unexpected argv=%q", lines[i])
		}
	}
	if !strings.Contains(lines[0], "--offset 1h") {
		t.Fatal("activity log window is not one hour")
	}
}

func TestSettleClassifiesAzureEvidenceConservatively(t *testing.T) {
	deleted := settleEvent("Microsoft.Compute/virtualMachines/delete", "Succeeded", "delete-1", "2026-11-20T18:10:00Z")
	started := settleEvent("Microsoft.Compute/virtualMachines/delete", "Started", "delete-1", "2026-11-20T18:09:00Z")
	write := settleEvent("Microsoft.Compute/virtualMachines/write", "Succeeded", "create-2", "2026-11-20T18:11:00Z")
	diskID := strings.Replace(settleVMID, "virtualMachines/vm-test", "disks/os-disk", 1)
	resources := fmt.Sprintf(`[{"id":%q}]`, settleVMID)
	disks := fmt.Sprintf(`[{"id":%q}]`, diskID)
	for _, tc := range []struct {
		name, activity, resources, disks, want string
		rc, count                              int
	}{
		{"unordered-terminal", "[" + deleted + "," + started + "]", "[]", "[]", "SETTLED-CLEAN", 0, 0},
		{"remaining-vm", "[" + write + "]", resources, "[]", "SETTLED-EXISTS", 1, 1},
		{"disk-only", "[" + deleted + "]", "[]", disks, "SETTLED-EXISTS", 1, 1},
		{"deduplicated", "[" + write + "]", resources, resources, "SETTLED-EXISTS", 1, 1},
		{"empty-is-not-proof", "[]", "[]", "[]", "UNKNOWN", 2, 0},
		{"pending-delete", "[" + started + "]", "[]", "[]", "UNKNOWN", 2, 0},
		{"pending-with-resource", "[" + started + "]", resources, "[]", "UNKNOWN", 2, 0},
		{"new-write-after-delete", "[" + deleted + "," + write + "]", "[]", "[]", "UNKNOWN", 2, 0},
		{"unmatched-operation", "[" + deleted + "," + strings.Replace(started, "delete-1", "delete-2", 1) + "]", "[]", "[]", "UNKNOWN", 2, 0},
		{"wrong-vm-delete", "[" + strings.ReplaceAll(deleted, "vm-test", "other-vm") + "]", "[]", "[]", "UNKNOWN", 2, 0},
		{"null-resource-list", "[" + deleted + "]", "null", "[]", "UNKNOWN", 2, 0},
		{"missing-disk-id", "[" + deleted + "]", "[]", "[{}]", "UNKNOWN", 2, 0},
		{"invalid-activity", "not-json", "[]", "[]", "UNKNOWN", 2, 0},
		{"null-activity", "null", "[]", "[]", "UNKNOWN", 2, 0},
		{"unknown-status", "[" + strings.Replace(deleted, "Succeeded", "Maybe", 1) + "]", "[]", "[]", "UNKNOWN", 2, 0},
		{"missing-operation-id", "[" + strings.Replace(deleted, `"operationId":"delete-1"`, `"operationId":""`, 1) + "]", "[]", "[]", "UNKNOWN", 2, 0},
		{"foreign-resource", "[" + deleted + "]", strings.ReplaceAll(resources, "rg-test", "foreign"), "[]", "UNKNOWN", 2, 0},
		{"foreign-activity", "[" + strings.ReplaceAll(deleted, "rg-test", "foreign") + "]", "[]", "[]", "UNKNOWN", 2, 0},
		{"truncated-log", "[" + strings.Repeat(deleted+",", 999) + deleted + "]", "[]", "[]", "UNKNOWN", 2, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settleFake(t, tc.activity, tc.resources, tc.disks)
			result, rc := invokeSettle(t)
			ids, ok := result["resources"].([]any)
			if rc != tc.rc || result["code"] != tc.want || !ok || len(ids) != tc.count {
				t.Fatalf("rc=%d result=%v", rc, result)
			}
		})
	}
}

func TestSettleQueryFailureNeverInfersCleanOrReleasesClaim(t *testing.T) {
	for _, query := range []string{"activity", "resources", "disks"} {
		t.Run(query, func(t *testing.T) {
			dir := settleFake(t, "[]", "[]", "[]")
			t.Setenv("HOME", dir)
			claim := filepath.Join(dir, ".local", "state", "seaweed-cloud", "claims", "11111111-1111-1111-1111-111111111111__rg-test.json")
			if err := os.MkdirAll(filepath.Dir(claim), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(claim, []byte("unresolved-create"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "fail-"+query), nil, 0600); err != nil {
				t.Fatal(err)
			}
			result, rc := invokeSettle(t)
			if rc != 2 || result["code"] != "UNKNOWN" {
				t.Fatalf("rc=%d result=%v", rc, result)
			}
			if b, err := os.ReadFile(claim); err != nil || string(b) != "unresolved-create" {
				t.Fatal("read-only settle modified the claim")
			}
			calls, err := os.ReadFile(filepath.Join(dir, "calls"))
			if err != nil || strings.Count(string(calls), "\n") != 3 {
				t.Fatalf("queries=%q err=%v", calls, err)
			}
		})
	}
}

func TestSettleReportsExactSortedResidue(t *testing.T) {
	diskID := strings.Replace(settleVMID, "virtualMachines/vm-test", "disks/os-disk", 1)
	settleFake(t, "[]", fmt.Sprintf(`[{"id":%q},{"id":%q}]`, settleVMID, diskID), fmt.Sprintf(`[{"id":%q}]`, diskID))
	result, rc := invokeSettle(t)
	if rc != 1 || result["code"] != "SETTLED-EXISTS" || !reflect.DeepEqual(result["resources"], []any{diskID, settleVMID}) {
		t.Fatalf("rc=%d result=%v", rc, result)
	}
}

func TestSettleRefusesInvalidScopeBeforeAzure(t *testing.T) {
	dir := settleFake(t, "[]", "[]", "[]")
	for _, args := range [][]string{nil, {"--resource-group", "rg-test"}, {"--resource-group", "--other", "--vm", "vm-test"}, {"--resource-group", "rg-test", "--vm", "a/b"}, {"--resource-group", "rg-test", "--vm", "vm-test", "--subscription", "wrong"}} {
		var out bytes.Buffer
		if run(append([]string{"settle"}, args...), &out) != 2 {
			t.Fatalf("accepted args=%v", args)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "calls")); !os.IsNotExist(err) {
		t.Fatal("invalid scope contacted Azure")
	}
}
