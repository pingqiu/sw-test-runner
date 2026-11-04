package seaweed

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/pingqiu/sw-test-runner/inventory"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

var masterWriteSettle = 30 * time.Second

// A run-named unit owns the load; directCleanup always collects and stops it.
func runDirectMasterWrites(ctx context.Context, d inventory.Document, h inventory.Host, c CloudConfig, args []string, run func(string, string) (string, error)) (string, error) {
	q, root := cloudQuote, h.RunRoot
	var config SmokeConfig
	if err := json.Unmarshal([]byte(args[3]), &config); err != nil {
		return "", err
	}
	cmd := "python3"
	for _, arg := range append(args, "--stage", "write-load", "--read-path", "http") {
		cmd += " " + q(arg)
	}
	rows, exit := root+"/master-writes.jsonl", root+"/master-writes.exit"
	cmd += " > " + q(rows) + "; rc=$?; printf '%s\\n' \"$rc\" > " + q(exit) + "; exit \"$rc\""
	if _, err := run("write-start", cloudStart(d, root, "writes", "/bin/sh -c "+q(cmd))); err != nil {
		return "", err
	}
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-time.After(masterWriteSettle):
	}
	unit := "sw-cloud-" + d.RunID + "-master"
	killed, err := run("master-kill", "test ! -f "+q(exit)+"; "+chaosStamp+"; "+chaosKillScript(unit))
	if err != nil {
		return "", err
	}
	old := strings.Fields(killed)
	if len(old) != 5 || old[1] != "OLD" || old[4] != "GONE" {
		return "", errors.New("master exit witness missing")
	}
	preKill, err := strconv.ParseFloat(old[0], 64)
	if err != nil || math.IsNaN(preKill) || math.IsInf(preKill, 0) {
		return "", errors.New("invalid pre-Kill clock")
	}
	unit += "-g2"
	if _, err = run("master-restart", cloudStart(d, root, "master-g2", cloudServices(h, h, root, false, c.Replication)["master"])); err != nil {
		return "", err
	}
	// No Assign probe: writes must meet the master's warm-up instead of waiting it out.
	ready, err := run("master-ready", "until curl -fsS --max-time 3 "+q("http://"+endpoint(h, "master_http")+"/cluster/status")+" | python3 -c "+q("import json,sys;sys.exit(0 if json.load(sys.stdin)['IsLeader'] is True else 1)")+"; do sleep 1; done; "+chaosStamp+"; "+chaosIdentityScript(unit))
	if err != nil {
		return "", err
	}
	replacement := strings.Fields(ready)
	if len(replacement) != 4 || replacement[1] != "NEW" || (replacement[2] == old[2] && replacement[3] == old[3]) {
		return "", errors.New("master replacement identity missing")
	}
	readyMono, err := strconv.ParseFloat(replacement[0], 64)
	if err != nil || math.IsNaN(readyMono) || math.IsInf(readyMono, 0) {
		return "", errors.New("invalid READY clock")
	}
	raw, err := run("write-wait", "until test -f "+q(exit)+"; do sleep 1; done; cat "+q(rows)+"; test \"$(cat "+q(exit)+")\" = 0")
	if err != nil {
		return "", err
	}
	report, err := writeReport([]byte(raw), config, preKill, readyMono)
	if err != nil {
		return "", err
	}
	out := filepath.Join(c.Output, "master-writes.json")
	body, err := json.MarshalIndent(report, "", "  ")
	if err == nil {
		err = os.WriteFile(out, body, 0600)
	}
	return out, err
}
