package actions

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	tr "github.com/pingqiu/sw-test-runner"
	"os"
)

// The caller supplies a fresh bundle path. An incomplete/unreadable receipt
// is UNKNOWN; it never proves a refusal, a clean host or lease release.
type commandReceipt struct {
	Schema        string `json:"schema"`
	Node          string `json:"node"`
	CommandSHA256 string `json:"command_sha256"`
	Complete      bool   `json:"complete"`
	Code          int    `json:"code"`
	Stdout        string `json:"stdout"`
	Stderr        string `json:"stderr"`
	Error         string `json:"error"`
}

func beginReceipt(act tr.Action, cmd string) (*os.File, error) {
	path := act.Params["receipt_path"]
	if path == "" {
		return nil, nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = writeReceipt(f, act.Node, cmd, "", "", -1, fmt.Errorf("no answer yet")); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
func writeReceipt(f *os.File, node, cmd, out, stderr string, code int, callErr error) error {
	if f == nil {
		return nil
	}
	r := commandReceipt{Schema: "runner-command/v1", Node: node, CommandSHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(cmd))), Complete: callErr == nil, Code: code, Stdout: out, Stderr: stderr}
	if callErr != nil {
		r.Error = callErr.Error()
	}
	if _, err := f.Seek(0, 0); err != nil {
		return err
	}
	if err := f.Truncate(0); err != nil {
		return err
	}
	if err := json.NewEncoder(f).Encode(r); err != nil {
		return err
	}
	return f.Sync()
}
