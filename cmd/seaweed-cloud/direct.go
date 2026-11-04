package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/pingqiu/sw-test-runner/packs/seaweed"
	"github.com/pingqiu/sw-test-runner/provision/smoke"
)

// directConfig is the controller-local input for an existing ssh host: the
// key that reaches it, its host-key pin, and the three pinned artifacts.
type directConfig struct {
	MasterWrites bool `json:"master_writes,omitempty"`
	ECStage1     bool `json:"ec_stage1,omitempty"`
	// KVSoak runs the two-host KV soak from SoakProfile (a profile input).
	KVSoak      bool                         `json:"kv_soak,omitempty"`
	KVLoad      bool                         `json:"kv_load,omitempty"` // the soak pipeline with the profile's client pattern
	SoakProfile string                       `json:"soak_profile,omitempty"`
	SoakRelease string                       `json:"soak_release,omitempty"` // the local release tree the profile pins
	SSHKey      string                       `json:"ssh_key"`
	HostKey     string                       `json:"host_key"`
	Files       map[string]seaweed.CloudFile `json:"files"`
}

// directResult is the one JSON line `direct` prints.
type directResult struct {
	Schema string `json:"schema"`
	Code   string `json:"code"` // PASS, FAIL or INPUT_REFUSED
	Rows   string `json:"rows,omitempty"`
	Output string `json:"output,omitempty"`
	Error  string `json:"error,omitempty"`
}

// runDirect runs the shared pack's cloud HTTP path against an existing host
// named by an onprem or docker inventory, over plain ssh: no cloud create or
// destroy, no S0b approval. The pack itself stops what it started and fails on
// an unclean postflight.
func runDirect(args []string, w io.Writer) int {
	out := directResult{Schema: "seaweed.direct-run/v1", Code: "INPUT_REFUSED"}
	fs := flag.NewFlagSet("seaweed-cloud direct", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	invPath := fs.String("inventory", "", "requested onprem/docker inventory JSON")
	cfgPath := fs.String("config", "", "direct config JSON: ssh_key, host_key, files")
	outDir := fs.String("out", "", "new local output directory")
	timeout := fs.Duration("timeout", 20*time.Minute, "bound for the whole run")
	err := fs.Parse(args)
	var inv []byte
	var c directConfig
	if err == nil && (fs.NArg() != 0 || *invPath == "" || *cfgPath == "" || *outDir == "" || *timeout <= 0) {
		err = errors.New("use: direct --inventory INV.json --config CONFIG.json --out NEW_DIR [--timeout 20m]")
	}
	if err == nil {
		inv, err = os.ReadFile(*invPath)
	}
	if err == nil {
		// direct only ever reaches an existing host it then cleans up; a cloud
		// VM is created and destroyed by plan/run/destroy, never here.
		var target struct {
			Target string `json:"target"`
		}
		if json.Unmarshal(inv, &target) != nil || target.Target != "onprem" && target.Target != "docker" {
			err = errors.New("direct runs only an onprem or docker inventory")
		}
	}
	if err == nil {
		var raw []byte
		if raw, err = os.ReadFile(*cfgPath); err == nil {
			err = smoke.StrictJSON(raw, &c)
		}
	}
	if err == nil {
		err = os.Mkdir(*outDir, 0o700) // a fresh directory: never mixes two runs' journals
	}
	if err != nil {
		out.Error = err.Error()
		_ = json.NewEncoder(w).Encode(out)
		return 1
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	ctx, stop := context.WithTimeout(ctx, *timeout)
	defer stop()
	rows, err := seaweed.RunCloudHTTP(ctx, inv, seaweed.CloudConfig{SSHKey: c.SSHKey, HostKey: c.HostKey, Output: *outDir, Files: c.Files, ECStage1: c.ECStage1, MasterWrites: c.MasterWrites, KVSoak: c.KVSoak, KVLoad: c.KVLoad, SoakProfile: c.SoakProfile, SoakRelease: c.SoakRelease})
	out.Code, out.Rows, out.Output = "PASS", rows, *outDir
	if err != nil {
		out.Code, out.Error = "FAIL", err.Error()
	}
	if e := json.NewEncoder(w).Encode(out); e != nil || err != nil {
		return 1
	}
	return 0
}
