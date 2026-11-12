package seaweed

import (
	"strings"
	"testing"

	"github.com/pingqiu/sw-test-runner/inventory"
)

func TestMaintenanceLookupCannotAliasThePostOwnerSet(t *testing.T) {
	for _, raw := range []string{`null`, `{}`, `{"locations":null}`, `{"locations":[null]}`, `{"locations":[{"url":null}]}`, `{"locations":[{"url":7}]}`, `{"locations":[],"Locations":[{"url":"a"}]}`, `{"locations":[{"url":"a","URL":"b"}]}`, `{"locations":[],"locations":[{"url":"a"}]}`, `{"locations":[],"error":"unknown VID"}`, `{"locations":[],"error":null}`, `{"locations":[]} {"locations":[]}`, `{"locations":[],"x":NaN}`} {
		if urls, err := maintenanceLookupURLs([]byte(raw)); err == nil {
			t.Fatalf("accepted lookup %s -> %v", raw, urls)
		}
	}
	urls, err := maintenanceLookupURLs([]byte(`{"volumeId":"47","locations":[{"url":"a:1","publicUrl":"a:1"},{"url":"b:1","dataCenter":"rack"}]}`))
	if err != nil || len(urls) != 2 || urls[0] != "a:1" || urls[1] != "b:1" {
		t.Fatalf("%v %v", urls, err)
	}
}

func TestMaintenanceTerminalNeedsItsOwnExplicitCompletion(t *testing.T) {
	m := maintenanceRuntime{d: inventory.Document{RunID: "run"}, c: CloudConfig{MaintenanceTask: "volume.balance"}, base: map[string]any{"config_sha256": strings.Repeat("a", 64)}}
	for _, tc := range []struct {
		name   string
		change func(*maintenanceTaskResult)
		valid  bool
	}{
		{"own completion", func(*maintenanceTaskResult) {}, true},
		{"foreign job", func(r *maintenanceTaskResult) { r.Job = "other-maintenance" }, false},
		{"retried attempt", func(r *maintenanceTaskResult) { r.Attempt = "2" }, false},
		{"foreign cluster", func(r *maintenanceTaskResult) { r.Identity.RunID = "other" }, false},
		{"missing exit", func(r *maintenanceTaskResult) { r.Exit = nil }, false},
		{"nonzero success", func(r *maintenanceTaskResult) { x := 73; r.Exit = &x }, false},
		{"foreign configuration", func(r *maintenanceTaskResult) { r.Identity.ConfigSHA = strings.Repeat("b", 64) }, false},
		{"old clock", func(r *maintenanceTaskResult) { r.Ended = 1 }, false},
		{"unbounded completion", func(r *maintenanceTaskResult) { r.Ended = 1000 }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			zero := 0
			r := maintenanceTaskResult{Job: "run-maintenance", Attempt: "1", Terminal: "succeeded", Exit: &zero, Started: 10, Ended: 20, Identity: maintenanceIdentity{PID: 12, Start: "3", Boot: "boot", Task: "volume.balance", RunID: "run", ConfigSHA: strings.Repeat("a", 64)}}
			tc.change(&r)
			if err := m.validateTaskResult(r); (err == nil) != tc.valid {
				t.Fatalf("%+v: %v", r, err)
			}
		})
	}
}

func TestMaintenanceMixedWrongBytesRemainFailWhileTransportIsUnknown(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		fail bool
	}{
		{`{"status":"FAIL","code":"CHAOS_LOAD_FAILED","error":"AssertionError: WRONG bytes for key"}`, true},
		{`{"status":"FAIL","code":"CHAOS_LOAD_FAILED","error":"TimeoutError: read did not complete"}`, false},
		{`{"status":"PASS","code":"CHAOS_LOAD_FAILED","error":"AssertionError: WRONG bytes for key"}`, false},
		{`{"status":"FAIL","code":"CHAOS_LOAD_FAILED","error":"AssertionError: WRONG bytes for key","status":"PASS"}`, false},
	} {
		m := &maintenanceRuntime{}
		m.observeMixed(tc.raw)
		if m.mixedDataFailed() != tc.fail {
			t.Fatalf("%s", tc.raw)
		}
	}
}
