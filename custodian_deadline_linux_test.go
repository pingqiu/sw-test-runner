//go:build linux

package testrunner

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestCustodyAcceptsSettlementAfterWorkDeadline(t *testing.T) {
	t.Setenv("S0A_TEST_CUSTODIAN", "")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "owned-custodian")
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = input.Close(); _ = cmd.Wait() }()
	a := testAdmission()
	a.WorkDeadline = time.Now().Add(250 * time.Millisecond)
	a.CleanupReserve = time.Second
	a.HardDeadline = a.WorkDeadline.Add(time.Second)
	a.OutputDir = t.TempDir()
	spec := JobSpec{Node: "local", Executable: exe, SHA256: strings.Repeat("a", 64), Argv: []string{"s0a-empty-workload"}, StdoutPath: a.OutputDir + "/out", StderrPath: a.OutputDir + "/err"}
	e, d := json.NewEncoder(input), json.NewDecoder(output)
	if err = e.Encode(custodyRequest{Sequence: 1, Operation: "enroll", Admission: a, Spec: spec}); err != nil {
		t.Fatal(err)
	}
	var r custodyReply
	if err = d.Decode(&r); err != nil {
		t.Fatal(err)
	}
	fd, err := unix.PidfdOpen(r.Identity.PID, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0); _ = unix.Close(fd) }()
	if err = e.Encode(custodyRequest{Sequence: 2, Operation: "authorize", Identity: r.Identity}); err != nil {
		t.Fatal(err)
	}
	if err = d.Decode(&r); err != nil || r.Error != "" {
		t.Fatalf("authorize: %v, %s", err, r.Error)
	}
	time.Sleep(time.Until(a.WorkDeadline.Add(100 * time.Millisecond)))
	if err = e.Encode(custodyRequest{Sequence: 3, Operation: "settle"}); err != nil {
		t.Fatal(err)
	}
	if err = d.Decode(&r); err != nil || r.Error != "" || !r.Exit.Settled {
		t.Fatalf("no cleanup receipt after work deadline: %+v, %v", r, err)
	}
}
