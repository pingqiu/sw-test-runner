package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDashboardSubmitDisabledByDefault(t *testing.T) {
	s := &server{root: t.TempDir()}
	req := httptest.NewRequest(http.MethodPost, "/api/rdma/submit", strings.NewReader(`{"mono_ref":"main"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleSubmitRDMA(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestDashboardControllerSubmitWritesQueue(t *testing.T) {
	tmp := t.TempDir()
	s := dashboardTestServer(tmp)
	s.controller.now = func() time.Time { return time.Date(2026, 6, 30, 13, 0, 0, 123, time.UTC) }
	if err := s.ensureControlDirs(); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/rdma/submit", strings.NewReader(`{"mono_ref":"rdma/api","run_by":"dash-test"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleSubmitRDMA(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	var resp controlSubmitResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json: %v", err)
	}
	raw, err := os.ReadFile(resp.QueuePath)
	if err != nil {
		t.Fatalf("read queue: %v", err)
	}
	text := string(raw)
	for _, want := range []string{
		"REQUEST_ID='20260630-130000-000000123-rdma_api-",
		"TESTOPS_MONO_REF='rdma/api'",
		"TESTOPS_RUN_BY='dash-test'",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in:\n%s", want, text)
		}
	}
}

func TestDashboardControllerIndexRenders(t *testing.T) {
	tmp := t.TempDir()
	s := dashboardTestServer(tmp)
	if err := s.ensureControlDirs(); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if err := os.WriteFile(filepath.Join(s.controller.queueDir, "queued.env"), []byte("x=1\n"), 0o644); err != nil {
		t.Fatalf("write queue: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	s.handleIndex(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"RDMA CI Controller", "Queue RDMA Gate", "queued"} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q", want)
		}
	}
}

func dashboardTestServer(root string) *server {
	return &server{
		root: filepath.Join(root, "results"),
		controller: &controlConfig{
			queueDir: filepath.Join(root, "queue"),
			stateDir: filepath.Join(root, "state"),
			logDir:   filepath.Join(root, "logs"),
			now:      time.Now,
		},
	}
}
