package main

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestClassifyPowerBillingOnlyWhenNotDeallocated(t *testing.T) {
	view := func(code string) []byte {
		return []byte(`{"instanceView":{"statuses":[{"code":"ProvisioningState/succeeded"},{"code":"` + code + `"}]}}`)
	}
	for _, c := range []struct{ code, state, billing string }{
		{"PowerState/deallocated", "deallocated", "no"},
		{"PowerState/running", "running", "yes"},
		{"PowerState/stopped", "stopped", "yes"}, // OS shutdown: allocated, compute still billing
		{"PowerState/weird", "weird", "UNKNOWN"},
	} {
		if s, b := classifyPower(view(c.code)); s != c.state || b != c.billing {
			t.Fatalf("%s: got %s/%s, want %s/%s", c.code, s, b, c.state, c.billing)
		}
	}
	if s, b := classifyPower([]byte(`{"instanceView":{"statuses":[]}}`)); s != "UNKNOWN" || b != "UNKNOWN" {
		t.Fatalf("no power status: got %s/%s", s, b)
	}
	if s, _ := classifyPower([]byte(`not json`)); s != "UNKNOWN" {
		t.Fatalf("bad json: got %s", s)
	}
}

func TestStandingRefusesBeforeAnyToolCall(t *testing.T) {
	ok := []string{"--name", "c04-standing", "--resource-group", "rg-claude04-standing-20261120"}
	for _, c := range []struct {
		args []string
		code string
	}{
		{nil, "USAGE"},
		{append([]string{"reboot"}, ok...), "USAGE"},
		{[]string{"status", "--name", "c04-standing", "--resource-group", "rg-codex02-azure-test-20261029"}, "USAGE"}, // shared run RG
		{[]string{"status", "--name", "Bad_Name", "--resource-group", "rg-claude04-standing-20261120"}, "USAGE"},
		{append([]string{"plan"}, ok...), "USAGE"},                                       // no --module/--vars
		{append([]string{"apply"}, ok...), "REFUSED"},                                    // no token
		{append(append([]string{"destroy"}, ok...), "--launch-token", "xyz"), "REFUSED"}, // bad token
	} {
		var out bytes.Buffer
		if rc := run(append([]string{"standing"}, c.args...), &out); rc != 2 {
			t.Fatalf("%v: rc %d, want 2", c.args, rc)
		}
		var r standingResult
		if err := json.Unmarshal(out.Bytes(), &r); err != nil || r.Code != c.code || r.Schema != "seaweed.cloud-standing/v1" {
			t.Fatalf("%v: got %+v (%v), want code %s", c.args, r, err, c.code)
		}
	}
}

func TestPlanAddresses(t *testing.T) {
	got := planAddresses([]byte(`{"resource_changes":[{"address":"azurerm_linux_virtual_machine.box","change":{"actions":["create"]}},{"address":"azurerm_public_ip.box","change":{"actions":["delete","create"]}}]}`))
	if len(got) != 2 || got[0] != "create azurerm_linux_virtual_machine.box" || got[1] != "delete+create azurerm_public_ip.box" {
		t.Fatalf("got %v", got)
	}
}
