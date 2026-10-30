//go:build linux

package testrunner

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestApprovedGORequiresIndependentPinnedBytes(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(home, ".s0b-go-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	file, cancel := filepath.Join(dir, "go.json"), filepath.Join(dir, "cancel.json")
	raw := []byte(`{"version":"s0b-go-v1","source_id":"0123456789abcdef01234567","request_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","binding":{"request_id":"r1"},"cleanup":[]}`)
	if err = os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	pin := FilePin{Path: file, SHA256: fmt.Sprintf("%x", sha256.Sum256(raw))}
	goRecord, err := ReadApprovedGO(pin, cancel, uint32(os.Getuid()))
	if err != nil || goRecord.Binding.RequestID != "r1" {
		t.Fatalf("independent GO: %#v %v", goRecord, err)
	}
	if err = os.WriteFile(file, append(raw, ' '), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = ReadApprovedGO(pin, cancel, uint32(os.Getuid())); err == nil {
		t.Fatal("a claimed ID beside changed approval bytes authorized work")
	}
	if err = os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(cancel, []byte("cancel"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = ReadApprovedGO(pin, cancel, uint32(os.Getuid())); err == nil {
		t.Fatal("cancel record ignored")
	}
}
