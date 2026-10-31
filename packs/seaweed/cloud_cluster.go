package seaweed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/pingqiu/sw-test-runner/inventory"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

func cloudArtifact(h inventory.Host, name string) bool {
	switch name {
	case "weed":
		return contains(h.Roles, "master")
	case "weed-volume":
		return contains(h.Roles, "volume")
	default:
		return contains(h.Roles, "client")
	}
}
func cloudStepEnabled(h inventory.Host, phase, name string) bool {
	switch phase {
	case "prepare":
		return name == "prepare" || name == "configure" || strings.HasPrefix(name, "verify-") && cloudArtifact(h, strings.TrimPrefix(name, "verify-"))
	case "deploy":
		if strings.HasPrefix(name, "start-") {
			return contains(h.Roles, strings.TrimPrefix(name, "start-"))
		}
		for role, suffix := range map[string]string{"master": "/dir/status", "filer": "/", "volume": "/status", "s3": "/"} {
			if contains(h.Roles, role) && name == "ready-http://"+endpoint(h, role+"_http")+suffix {
				return true
			}
		}
	case "load":
		return name == "kv-smoke"
	case "collect":
		return name == "collect-services"
	default:
		return name == "snapshot" || phase == "grow" && strings.HasPrefix(name, "grow-")
	}
	return false
}

func runCloudCluster(ctx context.Context, d inventory.Document, c CloudConfig) (bundle string, failure error) {
	groups := map[string][]inventory.Host{}
	if len(d.Hosts) != 10 || len(c.HostKeys) != 10 {
		return "", errors.New("ten exact hosts and pins required")
	}
	for _, h := range d.Hosts {
		roles := []string{"client"}
		if h.ID == "meta" {
			roles = []string{"master", "filer", "s3"}
		} else if len(h.ID) == 4 && h.ID >= "vol1" && h.ID <= "vol6" {
			roles = []string{"volume"}
		} else if h.ID != "client1" && h.ID != "client2" && h.ID != "client3" {
			return "", errors.New("foreign cluster host")
		}
		if c.HostKeys[h.ID] == "" || len(h.Roles) != len(roles) || h.RunRoot != "/data/seaweed-runs/"+d.RunID {
			return "", errors.New("missing pin or changed role/root")
		}
		for _, role := range roles {
			if !contains(h.Roles, role) {
				return "", errors.New("cluster roles changed")
			}
		}
		groups[roles[0]] = append(groups[roles[0]], h)
	}
	if e := c.CheckFiles(); e != nil {
		return "", e
	}
	phase := func(label string, hosts []inventory.Host) error {
		limit := time.Hour
		if label == "collect" {
			limit = time.Minute
		}
		work, cancel := context.WithTimeout(ctx, limit)
		defer cancel()
		var wg sync.WaitGroup
		var mu sync.Mutex
		var result error
		slots := make(chan struct{}, 3)
		for _, h := range hosts {
			wg.Add(1)
			go func(h inventory.Host) {
				defer wg.Done()
				select {
				case slots <- struct{}{}:
				case <-work.Done():
					return
				}
				defer func() { <-slots }()
				node := c
				node.HostKey = c.HostKeys[h.ID]
				node.Output = filepath.Join(c.Output, label, h.ID)
				e := work.Err()
				if e == nil {
					e = os.MkdirAll(node.Output, 0700)
				}
				if e == nil {
					_, e = runCloudNode(work, d, h, node, label)
				}
				if e != nil {
					mu.Lock()
					result = errors.Join(result, fmt.Errorf("%s/%s: %w", label, h.ID, e))
					mu.Unlock()
					if label != "collect" {
						cancel()
					}
				}
			}(h)
		}
		wg.Wait()
		return errors.Join(result, work.Err())
	}
	collected := false
	defer func() {
		if failure != nil && !collected {
			failure = errors.Join(failure, phase("collect", d.Hosts))
		}
	}()
	servers := append(append([]inventory.Host{}, groups["master"]...), groups["volume"]...)
	for _, s := range []struct {
		name  string
		hosts []inventory.Host
	}{{"prepare", d.Hosts}, {"deploy", groups["master"]}, {"deploy", groups["volume"]}, {"deploy", groups["client"]}, {"grow", groups["master"]}, {"before", servers}, {"load", groups["client"]}, {"after", servers}} {
		if e := phase(s.name, s.hosts); e != nil {
			return "", e
		}
	}
	collected = true
	if e := phase("collect", d.Hosts); e != nil {
		return "", e
	}
	return assessCloudCluster(ctx, d, c.Output)
}

func assessCloudCluster(ctx context.Context, d inventory.Document, root string) (string, error) {
	owners := map[string]string{}
	for _, h := range d.Hosts {
		if contains(h.Roles, "volume") {
			owners[h.ID] = endpoint(h, "volume_http")
		}
	}
	body, e := workerFiles.ReadFile("worker/cloud_batch.py")
	if e != nil {
		return "", e
	}
	script := filepath.Join(root, "cluster-assess.py")
	if e = os.WriteFile(script, body, 0600); e != nil {
		return "", e
	}
	args, _ := json.Marshal(owners)
	out, e := exec.CommandContext(ctx, "python3", "-I", script, "--assess", root, string(args)).CombinedOutput()
	result := filepath.Join(root, "cluster-result.json")
	return result, errors.Join(e, os.WriteFile(result, out, 0600))
}
