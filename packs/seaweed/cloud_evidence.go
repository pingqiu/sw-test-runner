package seaweed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/pingqiu/sw-test-runner/infra"
)

func writeCloudStep(dir, file, name, command, stdout, stderr string, rc int, err error) error {
	message := ""
	if err != nil {
		message = err.Error()
	}
	b, _ := json.MarshalIndent(map[string]any{
		"step": name, "command": command, "stdout": stdout, "stderr": stderr,
		"exit_code": rc, "error": message,
	}, "", "  ")
	return os.WriteFile(filepath.Join(dir, file), b, 0600)
}

// One best-effort read-only capture, before the controller's finally destroy.
// Inherit the remaining work envelope; never borrow the cloud cleanup reserve.
func collectCloudFailure(ctx context.Context, original *infra.Node, dir, root, runID string, quote func(string) string) error {
	cx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	n := &infra.Node{Host: original.Host, User: original.User, KeyFile: original.KeyFile, HostKey: original.HostKey}
	defer n.Close()
	command := "timeout -k 2 55 sh -c " + quote(
		"capture_rc=0; sudo journalctl --no-pager -u "+quote("sw-cloud-"+runID+"-*")+" -n 300 || capture_rc=$?; "+
			"for p in /var/log/cloud-init-output.log "+quote(root+"/worker/kv_smoke.py")+" "+
			quote(root+"/worker/soak_native_worker.py")+" "+quote(root+"/worker/soak_oracle.py")+" "+
			quote(root+"/worker")+"/*.log "+quote(root+"/worker")+"/*.jsonl "+quote(root+"/cloud-kv.jsonl")+"; do "+
			"printf '\\n=== %s ===\\n' \"$p\" || capture_rc=$?; if test -f \"$p\"; then tail -c 1048576 \"$p\" || capture_rc=$?; else printf 'MISSING\\n' || capture_rc=$?; fi; done; exit \"$capture_rc\"")
	stdout, stderr, rc := "", "", -1
	err := n.ConnectContext(cx)
	if err == nil {
		stdout, stderr, rc, err = n.Run(cx, command)
	}
	if err == nil && rc != 0 {
		err = fmt.Errorf("diagnostic command exit %d", rc)
	}
	// A fetch failure stays evidence, not a reason to skip or defer destruction.
	return errors.Join(err, writeCloudStep(dir, "cloud-failure-fetch.json", "failure-log-fetch", command, stdout, stderr, rc, err), os.WriteFile(filepath.Join(dir, "cloud-services.log"), []byte(stdout), 0600))
}
