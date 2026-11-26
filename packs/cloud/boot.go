package cloud

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var bootIDShape = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// SSH runs one command on a host over its management link with the run's
// key and the host's approved key pinned (never accepting a new one). It is a
// variable so the controls run without a host.
var SSH = func(ctx context.Context, h Host, key, knownHosts string, command string) (string, error) {
	cmd := exec.CommandContext(ctx, "ssh", "-i", key, "-p", strconv.Itoa(h.Port),
		"-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "UserKnownHostsFile="+knownHosts,
		"-o", "ConnectTimeout=5", "-o", "ServerAliveInterval=2", "-o", "ServerAliveCountMax=2",
		h.User+"@"+h.Address, command)
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.Output()
	return string(out), err
}

// BootID reads the host's boot id. Any failure (unreachable, refused host
// key, timeout, an answer that is not a boot id) is an error: to the fault
// that means "the node does not answer", never a boot id.
func BootID(ctx context.Context, h Host, key, scratch string) (string, error) {
	known := filepath.Join(scratch, "known_hosts-"+h.ID)
	line := "[" + h.Address + "]:" + strconv.Itoa(h.Port) + " " + h.HostKey + "\n"
	if h.Port == 22 {
		line = h.Address + " " + h.HostKey + "\n"
	}
	if err := os.WriteFile(known, []byte(line), 0o600); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := SSH(ctx, h, key, known, "cat /proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", fmt.Errorf("boot id of %s: %w", h.ID, err)
	}
	id := strings.TrimSpace(out)
	if !bootIDShape.MatchString(id) {
		return "", fmt.Errorf("boot id of %s: answer %q is not a boot id", h.ID, firstLine(id))
	}
	return id, nil
}
