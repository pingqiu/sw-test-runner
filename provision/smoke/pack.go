package smoke

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"time"

	"github.com/pingqiu/sw-test-runner/packs/seaweed"
)

// ExecPack is an external pack: a pinned executable the lifecycle runs between
// inventory and destroy, in place of the built-in seaweed pack. It reads
// $SEAWEED_CLOUD_PACK_CONTEXT (a JSON file: the inventory path, the controller
// ssh key path, the VM host key pin(s) and its own output directory), runs
// under the work context (its process group dies with it) and passes when it
// exits 0. Its digest is checked at admission and again just before it runs.
type ExecPack struct {
	Path   string   `json:"path"`
	SHA256 string   `json:"sha256"`
	Args   []string `json:"args,omitempty"`
}

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// packName is the configured pack ("" = the built-in seaweed pack).
func packName(c Config) string {
	if c.PackName == "" {
		return "seaweed"
	}
	return c.PackName
}

// checkPack admits the configured pack before any cloud call.
func checkPack(c Config) error {
	switch packName(c) {
	case "seaweed":
		if c.PackExec != nil {
			return errors.New("pack_exec is set but pack is seaweed: name the exec pack explicitly (--pack exec)")
		}
		return c.Cloud.CheckFiles()
	case "exec":
		p := c.PackExec
		if p == nil {
			return errors.New("pack exec needs pack_exec {path, sha256, args}")
		}
		if !filepath.IsAbs(p.Path) {
			return fmt.Errorf("pack_exec.path %q must be absolute", p.Path)
		}
		if !sha256Hex.MatchString(p.SHA256) {
			return errors.New("pack_exec.sha256 must be 64 lowercase hex")
		}
		got, err := fileSHA256(p.Path)
		if err != nil {
			return fmt.Errorf("pack_exec.path: %v", err)
		}
		if got != p.SHA256 {
			return fmt.Errorf("pack_exec.path digest %s != pinned %s", got, p.SHA256)
		}
		return nil
	}
	return fmt.Errorf("unknown pack %q (known: seaweed, exec)", c.PackName)
}

// packFor is the Pack the run calls between inventory and destroy.
func packFor(c Config) Pack {
	if packName(c) == "exec" && c.PackExec != nil {
		return execPack(*c.PackExec)
	}
	return seaweed.RunCloudHTTP
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// execPack runs the pinned executable with the run's inventory and keys.
func execPack(spec ExecPack) Pack {
	return func(ctx context.Context, inv []byte, cc seaweed.CloudConfig) (string, error) {
		dir := filepath.Join(cc.Output, "pack")
		if err := os.Mkdir(dir, 0700); err != nil {
			return "", err
		}
		if got, err := fileSHA256(spec.Path); err != nil || got != spec.SHA256 {
			return dir, fmt.Errorf("pack executable changed since admission (digest %s, err %v)", got, err)
		}
		invPath := filepath.Join(dir, "inventory.json")
		if err := os.WriteFile(invPath, inv, 0600); err != nil {
			return dir, err
		}
		deadline, _ := ctx.Deadline()
		pc, _ := json.Marshal(map[string]any{"schema": "seaweed.cloud-pack-context/v1", "inventory": invPath, "ssh_key": cc.SSHKey,
			"host_key": cc.HostKey, "host_keys": cc.HostKeys, "output": dir, "work_deadline": deadline.UTC().Format(time.RFC3339)})
		pcPath := filepath.Join(dir, "context.json")
		if err := os.WriteFile(pcPath, pc, 0600); err != nil {
			return dir, err
		}
		stdout, err := os.Create(filepath.Join(dir, "stdout.txt"))
		if err != nil {
			return dir, err
		}
		defer stdout.Close()
		stderr, err := os.Create(filepath.Join(dir, "stderr.txt"))
		if err != nil {
			return dir, err
		}
		defer stderr.Close()
		cmd := exec.CommandContext(ctx, spec.Path, spec.Args...)
		killGroup(cmd)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "SEAWEED_CLOUD_PACK_CONTEXT="+pcPath)
		cmd.Stdout, cmd.Stderr = stdout, stderr
		cmd.WaitDelay = 3 * time.Second
		if err = cmd.Run(); err != nil {
			return dir, fmt.Errorf("pack %s: %w", filepath.Base(spec.Path), err)
		}
		return dir, nil
	}
}
