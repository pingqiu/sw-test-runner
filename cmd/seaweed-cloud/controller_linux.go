//go:build linux

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// controllerIdentity binds a running `direct` to one process. A PID alone can
// be reused, and a command line can be matched by any other process: a launch
// shell still holding "seaweed-cloud direct" in its argv outlived the
// controller it started and kept a pgrep watcher reporting the run alive for
// four hours after it had failed.
type controllerIdentity struct {
	Schema    string `json:"schema"`
	RunID     string `json:"run_id"`
	PID       int    `json:"pid"`
	StartTime string `json:"starttime"` // /proc/PID/stat field 22, clock ticks since boot
	BootID    string `json:"boot_id"`
	Exe       string `json:"exe"`
	ExeSHA256 string `json:"exe_sha256"`
}

const controllerFile, resultFile = "controller.json", "result.json"

func bootID() (string, error) {
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	return strings.TrimSpace(string(b)), err
}

// processIdentity is what /proc says PID is now: its start time, its exe
// link and the sha256 of the executable it runs.
func processIdentity(pid int) (controllerIdentity, error) {
	id := controllerIdentity{Schema: "seaweed.direct-controller/v1", PID: pid}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return id, err
	}
	i := strings.LastIndexByte(string(b), ')')
	f := strings.Fields(string(b[i+1:]))
	if i < 0 || len(f) < 20 {
		return id, errors.New("unreadable proc stat")
	}
	id.StartTime = f[19] // fields after the comm start at field 3
	if id.Exe, err = os.Readlink(fmt.Sprintf("/proc/%d/exe", pid)); err != nil {
		return id, err
	}
	exe, err := os.Open(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		return id, err
	}
	defer exe.Close()
	h := sha256.New()
	if _, err = io.Copy(h, exe); err != nil {
		return id, err
	}
	id.ExeSHA256 = hex.EncodeToString(h.Sum(nil))
	id.BootID, err = bootID()
	return id, err
}

// recordController writes this process's identity into the new output
// directory before the run has any effect.
func recordController(outDir, runID string) error {
	id, err := processIdentity(os.Getpid())
	if err != nil {
		return err
	}
	id.RunID = runID
	body, _ := json.Marshal(id)
	return writeNew(filepath.Join(outDir, controllerFile), body)
}

// writeResult records the run's terminal line, independent of where stdout
// was redirected.
func writeResult(outDir string, r directResult) error {
	body, _ := json.Marshal(r)
	return writeNew(filepath.Join(outDir, resultFile), body)
}

func writeNew(name string, body []byte) error {
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(append(body, '\n'))
	return errors.Join(err, f.Sync(), f.Close())
}

// directStatus answers whether the controller recorded in DIR is still the
// running process: RUNNING (exit 0) only when the same boot, PID, start time,
// exe and exe digest all still match; EXITED (exit 10) with the recorded
// result once result.json exists; GONE_NO_RESULT (exit 11) when the recorded
// process is gone or replaced without a result; INPUT_REFUSED (exit 2).
func directStatus(args []string, w io.Writer) int {
	out := struct {
		Schema string `json:"schema"`
		State  string `json:"state"`
		RunID  string `json:"run_id,omitempty"`
		PID    int    `json:"pid,omitempty"`
		Code   string `json:"code,omitempty"`
		Reason string `json:"reason,omitempty"`
	}{Schema: "seaweed.direct-status/v1", State: "INPUT_REFUSED"}
	emit := func(rc int) int { _ = json.NewEncoder(w).Encode(out); return rc }
	fs := flag.NewFlagSet("seaweed-cloud direct-status", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dir := fs.String("out", "", "the direct run's output directory")
	var rec controllerIdentity
	err := fs.Parse(args)
	if err == nil && (fs.NArg() != 0 || *dir == "") {
		err = errors.New("use: direct-status --out DIR")
	}
	var body []byte
	if err == nil {
		body, err = os.ReadFile(filepath.Join(*dir, controllerFile))
	}
	if err == nil {
		err = json.Unmarshal(body, &rec)
	}
	if err == nil && (rec.Schema != "seaweed.direct-controller/v1" || rec.PID <= 0 || rec.StartTime == "" || rec.Exe == "" || rec.ExeSHA256 == "" || rec.BootID == "") {
		err = errors.New("controller record incomplete")
	}
	if err != nil {
		out.Reason = err.Error()
		return emit(2)
	}
	out.RunID, out.PID = rec.RunID, rec.PID
	if body, err := os.ReadFile(filepath.Join(*dir, resultFile)); err == nil {
		var r directResult
		if err = json.Unmarshal(body, &r); err != nil || r.Code == "" {
			out.Reason = "result record unreadable"
			return emit(2)
		}
		out.State, out.Code = "EXITED", r.Code
		return emit(10)
	}
	now, err := processIdentity(rec.PID)
	switch {
	case err != nil:
		out.Reason = "recorded process gone: " + err.Error()
	case now.BootID != rec.BootID:
		out.Reason = "host rebooted since the record"
	case now.StartTime != rec.StartTime:
		out.Reason = "PID reused by another process"
	case now.Exe != rec.Exe || now.ExeSHA256 != rec.ExeSHA256:
		out.Reason = "PID runs another executable"
	default:
		out.State = "RUNNING"
		return emit(0)
	}
	out.State = "GONE_NO_RESULT"
	return emit(11)
}
