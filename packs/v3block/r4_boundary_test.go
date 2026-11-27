package v3block

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestR4LedgerAllowsOnlyAckOrLaterUnackedContent(t *testing.T) {
	dev, ledger := r4Device(t)
	a, b, c := bytes.Repeat([]byte{1}, 4096), bytes.Repeat([]byte{2}, 4096), bytes.Repeat([]byte{3}, 4096)
	ha, hb := fmt.Sprintf("%x", sha256.Sum256(a)), fmt.Sprintf("%x", sha256.Sum256(b))
	r4File(t, ledger, fmt.Sprintf("intent a before 0 4096 %s\nack a before 0 4096 %s\nintent b during-loss 0 4096 %s\n", ha, ha, hb))
	put := func(data []byte) {
		t.Helper()
		f, e := os.OpenFile(dev, os.O_WRONLY, 0)
		if e != nil {
			t.Fatal(e)
		}
		defer f.Close()
		if _, e = f.WriteAt(data, 0); e != nil {
			t.Fatal(e)
		}
	}
	for _, data := range [][]byte{a, b} {
		put(data)
		if out, e := r4(t, "verify", dev, ledger, "before"); e != nil {
			t.Fatalf("allowed content rejected: %v %s", e, out)
		}
	}
	put(c)
	if out, e := r4(t, "verify", dev, ledger, "before"); e == nil || !strings.Contains(out, "MISMATCH") {
		t.Fatalf("unrelated bytes passed: %v %s", e, out)
	}
	f, e := os.OpenFile(ledger, os.O_APPEND|os.O_WRONLY, 0)
	if e != nil {
		t.Fatal(e)
	}
	fmt.Fprintf(f, "ack b during-loss 0 4096 %s\n", hb)
	f.Close()
	put(a)
	if out, e := r4(t, "verify", dev, ledger, "during-loss"); e == nil {
		t.Fatalf("later ACK did not replace older allowed values: %s", out)
	}
	put(b)
	if out, e := r4(t, "verify", dev, ledger, "before", "during-loss"); e != nil {
		t.Fatalf("latest ACK rejected: %v %s", e, out)
	}
	r4File(t, ledger, fmt.Sprintf("ack missing before 0 4096 %s\n", ha))
	if out, e := r4(t, "verify", dev, ledger); e == nil || !strings.Contains(out, "unmatched") {
		t.Fatalf("ACK without intent passed: %v %s", e, out)
	}
}

func TestR4StreamRecordsIntentBeforeEveryDeviceWrite(t *testing.T) {
	dev, ledger := r4Device(t)
	stub := r4File(t, filepath.Join(t.TempDir(), "checked-dd"), "#!/bin/sh\ntail -n 1 \"$R4_EXPECT_LEDGER\" | grep -q '^intent ' || exit 2\nexec dd \"$@\"\n")
	out, e := r4Env(t, []string{"R4_DD=" + stub, "R4_EXPECT_LEDGER=" + ledger}, "stream", dev, ledger, "during-loss", "3")
	if e != nil {
		t.Fatalf("stream: %v %s", e, out)
	}
	lines := strings.Split(strings.TrimSpace(mustRead(t, ledger)), "\n")
	if len(lines) != 6 {
		t.Fatalf("want 3 intent/ACK pairs: %v", lines)
	}
	for i := 0; i < 6; i += 2 {
		intent, ack := strings.Fields(lines[i]), strings.Fields(lines[i+1])
		if intent[0] != "intent" || ack[0] != "ack" || intent[1] != ack[1] {
			t.Fatalf("unordered ledger: %v", lines)
		}
	}
	if out, e = r4(t, "verify", dev, ledger, "during-loss"); e != nil {
		t.Fatalf("%v %s", e, out)
	}
}

func TestR4OldPrimaryAtOldEpochFailsBeforeLaterReplicaReady(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "seen")
	stub := r4File(t, filepath.Join(t.TempDir(), "status"), fmt.Sprintf("#!/bin/sh\nif [ ! -f '%s' ]; then touch '%s'; echo '{\"AuthorityRole\":\"primary\",\"Epoch\":1,\"Healthy\":true,\"ReplicationRole\":\"none\"}'; else echo '{\"AuthorityRole\":\"unknown\",\"Epoch\":2,\"Healthy\":false,\"ReplicationRole\":\"replica_ready\"}'; fi\n", marker, marker))
	if out, e := r4(t, "wait-role", stub, "replica", "2", "2"); e == nil || !strings.Contains(out, "old node is primary and healthy at epoch 1") {
		t.Fatalf("old primary escaped observation: %v %s", e, out)
	}
}

func TestR4EveryConnectBoundsControllerLoss(t *testing.T) {
	dir := t.TempDir()
	sys, devices := filepath.Join(dir, "sys"), filepath.Join(dir, "dev")
	ours := filepath.Join(sys, "nvme-subsys1")
	r4File(t, filepath.Join(ours, "subsysnqn"), "our-nqn\n")
	r4File(t, filepath.Join(ours, "nvme7", "state"), "live\n")
	r4File(t, filepath.Join(ours, "nvme7", "address"), "traddr=10.2.3.4\n")
	r4File(t, filepath.Join(ours, "nvme7n1"), "")
	r4File(t, filepath.Join(devices, "nvme7n1"), "")
	log := filepath.Join(dir, "argv")
	r4File(t, filepath.Join(dir, "nvme"), "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$R4_ARGV_LOG\"\n")
	env := []string{"PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH"), "R4_ARGV_LOG=" + log, "R4_SYS=" + sys, "R4_DEVDIR=" + devices}
	for _, port := range []string{"4420", "4421"} {
		out, err := r4Env(t, env, "connect", "our-nqn", "10.2.3.4", port, "0")
		if err != nil || !strings.Contains(out, "--ctrl-loss-tmo=180 --reconnect-delay=2") {
			t.Fatalf("connect/settings receipt: %v %s", err, out)
		}
	}
	connects := 0
	for _, line := range strings.Split(strings.TrimSpace(mustRead(t, log)), "\n") {
		if !strings.HasPrefix(line, "connect ") {
			continue
		}
		connects++
		for _, flag := range []string{"--ctrl-loss-tmo=180", "--reconnect-delay=2"} {
			if !strings.Contains(" "+line+" ", " "+flag+" ") {
				t.Fatalf("connect missing %s: %s", flag, line)
			}
		}
		if strings.Contains(line, "fast_io_fail") || strings.Contains(line, "fast-io-fail") {
			t.Fatalf("fast I/O failure must remain off as documented: %s", line)
		}
	}
	if connects != 2 {
		t.Fatalf("want both connect argv, got %d", connects)
	}
}
