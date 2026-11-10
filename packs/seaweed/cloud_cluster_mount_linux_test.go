//go:build linux

package seaweed

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pingqiu/sw-test-runner/inventory"
	"golang.org/x/crypto/ssh"
)

// On existing (onprem) hosts the cluster phases never stop their units, so the mount soak runs the direct teardown
// on all ten hosts on every way out: here meta's prepare fails, and every host is still stopped and postflighted;
// a client, which runs no unit, is never asked for a unit journal.
func TestOnpremClusterMountStopsEveryHostAfterAFailedPrepare(t *testing.T) {
	var base inventory.Document
	json.Unmarshal(newHTTPInventory(t), &base)
	d := base
	d.Target, d.ManagedResourceIDs, d.Hosts = "onprem", []string{}, nil
	c := CloudConfig{MountSoak: true, Output: t.TempDir(), HostKeys: map[string]string{}, Files: map[string]CloudFile{}}
	for _, name := range []string{"weed", "weed-volume", "native.tar"} {
		p := filepath.Join(t.TempDir(), name)
		os.WriteFile(p, []byte(name), 0600)
		c.Files[name] = CloudFile{Path: p, SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(name)))}
	}
	var mu sync.Mutex
	seen := map[string][]string{}
	for i, id := range []string{"meta", "vol1", "vol2", "vol3", "vol4", "vol5", "vol6", "client1", "client2", "client3"} {
		id := id
		address, pin, key, _ := cloudSSH(t, "", func(cmd string, ch ssh.Channel) uint32 {
			mu.Lock()
			seen[id] = append(seen[id], cmd)
			mu.Unlock()
			if strings.HasPrefix(cmd, "cat >") {
				io.Copy(io.Discard, ch)
			}
			if id == "meta" && strings.Contains(cmd, "install -d") {
				return 1 // meta's prepare fails
			}
			if strings.Contains(cmd, "systemctl list-units") && strings.Contains(cmd, "postflight") {
				fmt.Fprintln(ch, "units=0 ports=0") // the teardown's postflight; other unit listings are empty
			}
			return 0
		})
		c.SSHKey, c.HostKeys[id] = key, pin
		h := base.Hosts[0]
		host, port, _ := net.SplitHostPort(address)
		h.ID, h.Management.Address = id, host
		fmt.Sscan(port, &h.Management.Port)
		h.Data.Address = fmt.Sprintf("127.0.1.%d", i+1)
		switch {
		case id == "meta":
			h.Roles = []string{"master", "filer", "s3"}
			h.Services = []inventory.Service{{Name: "master_http", Port: 9333}, {Name: "master_grpc", Port: 19333}, {Name: "filer_http", Port: 8888}, {Name: "filer_grpc", Port: 18888}, {Name: "s3_http", Port: 8333}}
		case strings.HasPrefix(id, "vol"):
			h.Roles, h.Services = []string{"volume"}, []inventory.Service{{Name: "volume_http", Port: 8080}, {Name: "volume_grpc", Port: 18080}}
		default:
			h.Roles, h.Services = []string{"client"}, []inventory.Service{}
		}
		d.Hosts = append(d.Hosts, h)
	}
	for i := range d.Hosts {
		d.Hosts[i].RunRoot = cloudRunRoot(d, d.Hosts[i])
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := runCloudCluster(ctx, d, c); err == nil {
		t.Fatal("a failed prepare passed")
	}
	mu.Lock()
	defer mu.Unlock()
	for _, h := range d.Hosts {
		cmds := strings.Join(seen[h.ID], "\n")
		if !strings.Contains(cmds, "systemctl stop") || !strings.Contains(cmds, "systemctl list-units") {
			t.Errorf("%s was not stopped and postflighted", h.ID)
		}
		if strings.HasPrefix(h.ID, "client") && strings.Contains(cmds, "sh -ec 'sudo journalctl") { // the teardown's own capture
			t.Errorf("%s, with no unit, was asked for a unit journal", h.ID)
		}
		if _, e := os.Stat(filepath.Join(c.Output, "cleanup", h.ID, "cloud-postflight.json")); e != nil {
			t.Errorf("%s has no postflight record: %v", h.ID, e)
		}
	}
}
