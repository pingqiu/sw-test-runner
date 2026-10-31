//go:build linux

package seaweed

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pingqiu/sw-test-runner/inventory"
	"golang.org/x/crypto/ssh"
)

// Explicit opt-in only: discovery never unpacks artifacts or contacts a host.
// The only network peer is a loopback SSH server executing the production setup.
func TestCloudPinnedArtifactsOffline(t *testing.T) {
	cloudPinnedArtifactsOffline(t, false)
}

func TestCloudPreflightRejectsInvalidInnerPrepare(t *testing.T) {
	cloudPinnedArtifactsOffline(t, true)
}

func checkCloudPreflightSyntax(ctx context.Context, command string) error {
	out, err := exec.CommandContext(ctx, "sh", "-n", "-c", command).CombinedOutput()
	if err != nil {
		return fmt.Errorf("outer shell syntax: %s: %w", out, err)
	}
	for _, prefix := range []string{"timeout -k 5 120 sh -ec ", "timeout -k 2 55 sh -c "} {
		if !strings.HasPrefix(command, prefix) {
			continue
		}
		quoted := strings.TrimPrefix(command, prefix)
		if len(quoted) < 2 || quoted[0] != '\'' || quoted[len(quoted)-1] != '\'' {
			return fmt.Errorf("inner shell syntax: expected the production single-quoted script")
		}
		script := strings.ReplaceAll(quoted[1:len(quoted)-1], "'\"'\"'", "'")
		if "'"+strings.ReplaceAll(script, "'", "'\"'\"'")+"'" != quoted {
			return fmt.Errorf("inner shell syntax: noncanonical production quoting")
		}
		out, err := exec.CommandContext(ctx, "sh", "-n", "-c", script).CombinedOutput()
		if err != nil {
			return fmt.Errorf("inner shell syntax: %s: %w", out, err)
		}
		return nil
	}
	if strings.HasPrefix(command, "timeout ") {
		return fmt.Errorf("inner shell syntax: unknown production wrapper")
	}
	return nil
}

func cloudPinnedArtifactsOffline(t *testing.T, invalidPrepare bool) {
	artifacts := os.Getenv("SEAWEED_CLOUD_ARTIFACT_DIR")
	if artifacts == "" {
		t.Skip("set SEAWEED_CLOUD_ARTIFACT_DIR for the exact af8c offline preflight")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	os.Mkdir(bin, 0700)
	// Parse real service argv using the real binary's help; never run systemd.
	os.WriteFile(filepath.Join(bin, "sudo"), []byte("#!/bin/sh\n[ \"$1\" = systemd-run ] || exit 91\nshift\nwhile [ \"${1#--}\" != \"$1\" ]; do shift; done\ncase \"$1\" in *weed-volume) exec \"$@\" --help;; *) exec \"$@\" -h;; esac\n"), 0700)
	os.WriteFile(filepath.Join(bin, "python3"), []byte("#!/bin/sh\nexec /usr/bin/python3 \"$@\" --dry-run\n"), 0700)
	var remoteRoot string
	var mu sync.Mutex
	counts := map[string]int{}
	note := func(s string) { mu.Lock(); counts[s]++; mu.Unlock(); t.Log(s) }
	address, pin, key, _ := cloudSSH(t, "", func(command string, channel ssh.Channel) uint32 {
		if invalidPrepare && strings.Contains(command, "sudo install -d") {
			command = strings.TrimSuffix(command, "'") + "; then'"
		}
		if e := checkCloudPreflightSyntax(ctx, command); e != nil {
			fmt.Fprint(channel.Stderr(), e)
			return 90
		}
		command = strings.ReplaceAll(command, remoteRoot, root)
		note("PASS: production command syntax")
		if strings.Contains(command, "cloud-init status --wait") {
			fmt.Fprint(channel, cleanCloudInit)
			note("NOT_RUN: VM-only cloud-init; production JSON fixture")
			return 0
		}
		if strings.Contains(command, "sudo install -d") {
			for _, dir := range []string{"worker", "master", "volume", "filer"} {
				if e := os.Mkdir(filepath.Join(root, dir), 0700); e != nil {
					fmt.Fprint(channel.Stderr(), e)
					return 1
				}
			}
			note("NOT_RUN: cloud-init status / sudo ownership; VM-only; prepare syntax checked")
			return 0
		}
		if strings.Contains(command, "until curl") {
			note("NOT_RUN: live HTTP readiness; syntax checked")
			return 0
		}
		if strings.Contains(command, "journalctl") {
			note("NOT_RUN: live service journal; syntax checked")
			return 0
		}
		worker := strings.Contains(command, "kv_smoke.py") && !strings.HasPrefix(command, "cat >")
		if worker {
			if !strings.Contains(command, `"native_dir":"`+root+`"`) {
				fmt.Fprint(channel.Stderr(), "worker native_dir does not bind extracted package parent")
				return 92
			}
		}
		cmd := exec.CommandContext(ctx, "sh", "-c", command)
		cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
		if strings.HasPrefix(command, "cat >") {
			cmd.Stdin, cmd.Stdout, cmd.Stderr = channel, channel, channel.Stderr()
			if e := cmd.Run(); e != nil {
				fmt.Fprint(channel.Stderr(), e)
				return 1
			}
			note("PASS: production upload")
			return 0
		}
		out, err := cmd.CombinedOutput()
		fmt.Fprint(channel, string(out))
		if strings.Contains(command, "sudo systemd-run") {
			if strings.Contains(string(out), "GLIBC_") && strings.Contains(string(out), "not found") {
				note("NOT_RUN: pinned volume CLI requires newer glibc than offline host; VM ABI unqualified")
				return 0
			}
			if strings.Contains(string(out), "Usage:") && !strings.Contains(string(out), "flag provided but not defined") && !strings.Contains(string(out), "unexpected argument") {
				note("PASS: production service argv parsed by pinned binary; live systemd NOT_RUN")
				return 0
			}
		}
		if err != nil {
			fmt.Fprint(channel.Stderr(), err)
			return 1
		}
		if worker {
			note("PASS: production worker argv dry-run; live KV NOT_RUN")
			note("NOT_RUN: Python3.12 native import; dry-run does not import the released extension")
			fmt.Fprint(channel.Stderr(), "OFFLINE_STOP_BEFORE_LIVE_KV")
			return 77 // Deliberate stop, never fabricate an ACK/exact PASS summary.
		}
		note("PASS: production artifact/configuration shell")
		return 0
	})
	raw := cloudInput(t, address)
	var doc inventory.Document
	json.Unmarshal(raw, &doc)
	remoteRoot = doc.Hosts[0].RunRoot
	cfg := CloudConfig{SSHKey: key, HostKey: pin, Output: t.TempDir(), Files: map[string]CloudFile{}}
	for _, f := range []struct{ name, file, sha string }{
		{"weed", "weed", "8ad4acae47ac3bf4dba946aa65d47ce0cf8f241455fbce45decfa006dc985535"},
		{"weed-volume", "weed-volume", "338c64f44af8931c01d7e5a8d88d25a9c8438839fd357ad77b1b1ba9e6bce889"},
		{"native.tar", "native.tar.gz", "6c12ead3eccc7316f398b1d1fb73db64a36b40fac5e9121ed1cea1a6156ce089"},
	} {
		cfg.Files[f.name] = CloudFile{Path: filepath.Join(artifacts, f.file), SHA256: f.sha}
	}
	_, e := RunCloudHTTP(ctx, raw, cfg)
	if invalidPrepare {
		if e == nil || !strings.Contains(e.Error(), "cloud step prepare") || !strings.Contains(e.Error(), "shell syntax") {
			t.Fatalf("invalid inner prepare passed preflight: %v", e)
		}
		return
	}
	if e == nil || !strings.Contains(e.Error(), "OFFLINE_STOP_BEFORE_LIVE_KV") {
		t.Fatalf("offline setup did not reach deliberate live-workload boundary: %v", e)
	}
	mu.Lock()
	defer mu.Unlock()
	if counts["PASS: production command syntax"] != 23 || counts["PASS: production upload"] != 6 || counts["PASS: production artifact/configuration shell"] != 4 || counts["PASS: production worker argv dry-run; live KV NOT_RUN"] != 1 {
		t.Fatalf("setup coverage: %v", counts)
	}
	if _, e := os.Stat(filepath.Join(root, "seaweedkv_native", "__init__.py")); e != nil {
		t.Fatal(e)
	}
}
