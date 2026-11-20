//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
