//go:build linux

package seaweed

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pingqiu/sw-test-runner/inventory"
	"golang.org/x/crypto/ssh"
)

type maintenanceCallerHost struct {
	base                                           *processFake
	secondData                                     string
	mu                                             sync.Mutex
	variant                                        string
	arms, prepares, samples, cleanupCalls, disarms int
	prepared                                       map[string]maintenanceRootRecord
	config                                         map[string]any
	manifest                                       maintenanceManifest
	identity                                       maintenanceIdentity
	finished, removedReplica                       bool
	ready, release                                 chan struct{}
	postReadback                                   chan struct{}
	releaseSample                                  int
	postOnce                                       sync.Once
	secondEntered, restartSeen                     chan struct{}
	restartOnce                                    sync.Once
	once                                           sync.Once
}

func maintenanceInner(cmd string) string {
	if i := strings.Index(cmd, "sh -ec '"); i >= 0 {
		return strings.ReplaceAll(cmd[i+8:len(cmd)-1], `'"'"'`, `'`)
	}
	return cmd
}
func maintenanceCommandConfig(inner string) (map[string]any, bool) {
	if !strings.Contains(inner, "python3 -I") {
		return nil, false
	}
	i := strings.LastIndex(inner, " '{")
	if i < 0 || !strings.HasSuffix(inner, "'") {
		return nil, false
	}
	var c map[string]any
	if json.Unmarshal([]byte(strings.ReplaceAll(inner[i+2:len(inner)-1], `'"'"'`, `'`)), &c) != nil {
		return nil, false
	}
	return c, true
}
func (f *maintenanceCallerHost) exec(cmd string, ch ssh.Channel) uint32 {
	if strings.HasPrefix(cmd, "cat >") {
		io.Copy(io.Discard, ch)
		return 0
	}
	inner := maintenanceInner(cmd)
	if f.variant == "pre-GONE-RPC" {
		if strings.Contains(inner, "kill -KILL") {
			select {
			case <-f.secondEntered:
			case <-time.After(10 * time.Second):
				return 77
			}
		}
		if strings.Contains(inner, "systemd-run") && strings.Contains(inner, "volume-g2") && !strings.Contains(inner, "--on-active=") {
			f.restartOnce.Do(func() { close(f.restartSeen) })
		}
	}
	if c, ok := maintenanceCommandConfig(inner); ok {
		stage, _ := c["stage"].(string)
		emit := func(v any) uint32 { b, _ := json.Marshal(v); fmt.Fprint(ch, string(b)); return 0 }
		switch stage {
		case "fresh":
			return emit(map[string]bool{"fresh": true})
		case "prepare":
			f.mu.Lock()
			defer f.mu.Unlock()
			f.prepares++
			if f.arms != 2 {
				return 42
			}
			r := maintenanceRootRecord{Schema: "seaweed.maintenance-root/v1", RunID: c["run_id"].(string), RoleRunID: c["role_run_id"].(string), Path: c["root"].(string), Dev: 1, Inode: uint64(f.prepares + 100), UID: 1001}
			f.prepared[r.Path] = r
			if f.variant == "lost-prepare-reply" && f.prepares == 1 {
				return 73
			}
			return emit(r)
		case "preserve":
			if f.variant == "preserve-failed" {
				return 1
			}
			return emit(map[string]any{"journal": "owned task diagnostics", "roots": map[string]any{}})
		case "cleanup":
			f.mu.Lock()
			defer f.mu.Unlock()
			f.cleanupCalls++
			if f.variant == "lost-prepare-reply" {
				return 73
			}
			status := "CLEAN"
			removed := []string{}
			if c["remove"] != true {
				status = "STOPPED_EVIDENCE_RETAINED"
			} else {
				for _, root := range c["roots"].([]any) {
					removed = append(removed, root.(map[string]any)["path"].(string))
				}
			}
			return emit(map[string]any{"status": status, "removed": removed, "units": 0, "listeners": 0, "run_processes": 0})
		case "init":
			return emit(map[string]bool{"initialized": true})
		case "seed":
			f.mu.Lock()
			defer f.mu.Unlock()
			f.config = c
			m := maintenanceManifest{RunID: c["run_id"].(string), Task: c["task"].(string), Collection: c["collection"].(string), VID: "47"}
			for i := 0; i < 16; i++ {
				key := fmt.Sprint("maintenance-", i)
				block := sha256.Sum256([]byte(m.RunID + "/" + key))
				length := int64(1 << 20)
				if m.Task == "volume.balance" {
					length = 8 << 20
				}
				m.Objects = append(m.Objects, maintenanceObject{Key: key, FID: fmt.Sprintf("47,%x12345678", i+1), SHA: hash([]byte(strings.Repeat(string(block[:]), int(length)/32))), Length: length})
			}
			f.manifest = m
			return emit(m)
		case "scope":
			f.mu.Lock()
			defer f.mu.Unlock()
			id := regexp.MustCompile(`vol[1-6]$`).FindString(c["root"].(string))
			owned := f.owns(id)
			s := maintenanceScope{maintenanceSubject: maintenanceSubject{Collection: c["collection"].(string), VID: "47", Shards: []int{}}, Ordinary: owned, Metadata: map[string]json.RawMessage{}, Files: []maintenanceFile{}}
			if owned {
				s.DataVIDs = append(s.DataVIDs, "47")
				for _, ext := range []string{"dat", "idx", "vif"} {
					s.Files = append(s.Files, maintenanceFile{Name: s.Collection + "_47." + ext, Size: 100, Inode: 51, UID: 1001, SHA: strings.Repeat("a", 64)})
				}
			}
			if f.variant == "pending-effect" && f.finished {
				s.Pending = 1
			}
			if c["task"] == "volume.balance" && (id == "vol1" || id == "vol2") {
				s.DataVIDs = append(s.DataVIDs, "48", "49", "50", "51")
			}
			return emit(s)
		case "quarantine":
			f.mu.Lock()
			f.removedReplica = true
			f.mu.Unlock()
			fmt.Fprint(ch, `{"retained":true}`)
			return 0
		case "task":
			f.mu.Lock()
			f.identity = maintenanceIdentity{PID: 444, Start: "55", Boot: "meta-boot", Task: c["task"].(string), RunID: c["run_id"].(string), ConfigSHA: c["config_sha256"].(string)}
			f.mu.Unlock()
			close(f.ready)
			select {
			case <-f.release:
			case <-time.After(15 * time.Second):
				return 74
			}
			select {
			case <-f.postReadback:
			case <-time.After(15 * time.Second):
				return 76
			}
			f.mu.Lock()
			f.finished = true
			zero := 0
			r := maintenanceTaskResult{Job: c["run_id"].(string) + "-maintenance", Attempt: "1", Terminal: "succeeded", Exit: &zero, Identity: f.identity, Started: 50, Ended: float64(104 + f.samples*2)}
			f.mu.Unlock()
			return emit(r)
		case "sample":
			select {
			case <-f.ready:
			case <-time.After(15 * time.Second):
				return 75
			}
			f.mu.Lock()
			f.samples++
			n := f.samples
			f.mu.Unlock()
			if f.variant == "pre-GONE-RPC" && n == 2 {
				close(f.secondEntered)
				select {
				case <-f.restartSeen:
				case <-time.After(10 * time.Second):
					return 78
				}
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.releaseSample > 0 && f.samples >= f.releaseSample+2 {
				f.postOnce.Do(func() { close(f.postReadback) })
			}
			s := maintenanceSampleFixtureForCaller(f.identity, float64(100+f.samples*2), f.variant)
			s.Progress.Source = fmt.Sprintf("%s:%d", f.base.data, f.base.ports["1"])
			s.Progress.Target = fmt.Sprintf("%s:%d", f.base.data, f.base.ports["3"])
			s.Progress.Bytes += int64(f.samples * 1024)
			if f.finished || f.variant == "pre-GONE-RPC" && n > 2 {
				s.Alive = false
				s.Progress = nil
			}
			return emit(s)
		case "lock_check":
			f.mu.Lock()
			defer f.mu.Unlock()
			if !f.finished || f.variant == "lock-unreadable" {
				return 74
			}
			return emit(map[string]any{"lock_usable": true, "vid": "47", "collection": c["collection"], "waited": false})
		case "readback":
			f.mu.Lock()
			defer f.mu.Unlock()
			rows := []maintenanceData{}
			for _, o := range f.manifest.Objects {
				d := maintenanceData{Key: o.Key, ActualSHA256: o.SHA, ActualLength: o.Length, Status: 200, Observed: true}
				if f.variant == "wrong-bytes" {
					d.ActualSHA256 = strings.Repeat("b", 64)
				}
				rows = append(rows, d)
			}
			return emit(rows)
		}
	}
	if strings.Contains(inner, "--on-active=") && strings.Contains(inner, "sw-maint-") {
		f.mu.Lock()
		f.arms++
		f.mu.Unlock()
		return 0
	}
	if strings.Contains(inner, "ExecMainStartTimestampMonotonic") {
		if strings.Contains(inner, "echo DISARMED") {
			f.mu.Lock()
			f.disarms++
			f.mu.Unlock()
		}
		if f.variant == "backstop-fired" {
			return 1
		}
		return 0
	}
	if strings.Contains(inner, "chaos_load.py") && strings.Contains(inner, "readback") {
		f.once.Do(func() { f.mu.Lock(); f.releaseSample = f.samples; f.mu.Unlock(); close(f.release) })
	}
	if strings.Contains(inner, "/dir/status") && !strings.Contains(inner, "until") {
		nodes := []map[string]string{}
		for i := 1; i <= 6; i++ {
			nodes = append(nodes, map[string]string{"Url": fmt.Sprintf("%s:%d", f.dataAddress(i), f.base.ports[strconv.Itoa(i)])})
		}
		raw, _ := json.Marshal(map[string]any{"Topology": map[string]any{"DataCenters": []any{map[string]any{"Racks": []any{map[string]any{"DataNodes": nodes}}}}}})
		fmt.Fprint(ch, string(raw))
		return 0
	}
	if strings.Contains(inner, "/dir/lookup?") {
		vid := 0
		fmt.Sscanf(strings.Split(inner, "volumeId=")[1], "%d", &vid)
		if vid >= 47 {
			f.mu.Lock()
			ids := []int{}
			for i := 1; i <= 6; i++ {
				if vid == 47 && f.owns("vol"+strconv.Itoa(i)) || vid > 47 && i <= 2 {
					ids = append(ids, i)
				}
			}
			f.mu.Unlock()
			locations := []map[string]string{}
			for _, id := range ids {
				locations = append(locations, map[string]string{"url": fmt.Sprintf("%s:%d", f.dataAddress(id), f.base.ports[strconv.Itoa(id)])})
			}
			b, _ := json.Marshal(map[string]any{"locations": locations})
			fmt.Fprint(ch, string(b))
			return 0
		}
		if vid >= 13 {
			vid -= 10
		}
		a, b := 2*(vid-2)-1, 2*(vid-2)
		fmt.Fprintf(ch, `{"locations":[{"url":"%s:%d"},{"url":"%s:%d"}]}`, f.dataAddress(a), f.base.ports[strconv.Itoa(a)], f.dataAddress(b), f.base.ports[strconv.Itoa(b)])
		return 0
	}
	if strings.Contains(inner, "curl") && strings.Contains(inner, "/status") && !strings.Contains(inner, "until") {
		for index, port := range f.base.ports {
			if strings.Contains(inner, fmt.Sprintf(":%d/status", port)) {
				i, _ := strconv.Atoi(index)
				root := f.base.baseRoot + "-vol" + index
				if strings.Contains(inner, "/proc/$pid/cmdline") {
					fmt.Fprintf(ch, "OWNER %d 555 %s:%d %s\n", 1000+i, f.dataAddress(i), port, root)
				}
				volumes := []map[string]any{{"Id": 2 + (i+1)/2, "ReadOnly": false}}
				f.base.mu.Lock()
				after := f.base.journalRead
				f.base.mu.Unlock()
				if after {
					volumes = append(volumes, map[string]any{"Id": 12 + (i+1)/2, "ReadOnly": false})
				}
				f.mu.Lock()
				owned := f.owns("vol" + index)
				collection := ""
				if f.config != nil {
					collection, _ = f.config["collection"].(string)
				}
				f.mu.Unlock()
				if collection != "" && owned {
					volumes = append(volumes, map[string]any{"Id": 47, "Collection": collection, "ReadOnly": false})
				}
				f.mu.Lock()
				balance := f.config != nil && f.config["task"] == "volume.balance"
				f.mu.Unlock()
				if collection != "" && balance && i <= 2 {
					for id := 48; id <= 51; id++ {
						volumes = append(volumes, map[string]any{"Id": id, "Collection": collection, "ReadOnly": false})
					}
				}
				b, _ := json.Marshal(map[string]any{"Volumes": volumes, "DiskStatuses": []map[string]string{{"dir": root + "/volume"}}})
				fmt.Fprint(ch, string(b))
				return 0
			}
		}
	}
	return f.base.exec(cmd, ch)
}
func (f *maintenanceCallerHost) dataAddress(i int) string {
	if i >= 4 {
		return f.secondData
	}
	return f.base.data
}
func (f *maintenanceCallerHost) owns(id string) bool {
	if !f.finished {
		return id == "vol1" || id == "vol2" && !f.removedReplica
	}
	if f.variant == "no-op" {
		return id == "vol1" || id == "vol2"
	}
	if f.manifest.Task == "volume.fix.replication" {
		return id == "vol1" || id == "vol3"
	}
	return id == "vol2" || id == "vol3"
}
func maintenanceSampleFixtureForCaller(identity maintenanceIdentity, at float64, variant string) maintenanceSample {
	var s maintenanceSample
	json.Unmarshal([]byte(`{"progress":{"state":"in_flight","vid":"47","operation":"1","bytes":1048576,"begin":50}}`), &s)
	s.Identity = &identity
	s.Before, s.After = at, at+1
	s.Boot = identity.Boot
	s.Alive = true
	if variant == "queued" {
		s.Progress.State = "queued"
	}
	if variant == "foreign-VID" {
		s.Progress.VID = "48"
	}
	return s
}

func TestMaintenancePublicCallerLifecycleAndIndependentOracle(t *testing.T) {
	oldSettle, oldDown, oldBase := chaos2Settle, chaos2Down, chaosBase
	oldObserve := maintenanceObservationPeriod
	maintenanceObservationPeriod = time.Millisecond
	defer func() { maintenanceObservationPeriod = oldObserve }()
	chaos2Settle, chaos2Down = time.Millisecond, time.Millisecond
	chaosBase = map[string]float64{"client1": 1000, "client2": 1000, "client3": 1000}
	defer func() { chaos2Settle, chaos2Down, chaosBase = oldSettle, oldDown, oldBase }()
	for _, tc := range []struct{ task, scenario, variant, status string }{
		{"volume.balance", "C1-single-kill", "", "PASS"},
		// The modeled copy touches vol1/vol3, not all six rolling victims.
		{"volume.balance", "C2-rolling", "", "NOT_COVERED"},
		{"volume.fix.replication", "C1-single-kill", "", "PASS"},
		{"volume.fix.replication", "C4-meta-restart", "", "PASS"},
		{"volume.balance", "C1-single-kill", "queued", "NOT_COVERED"},
		{"volume.balance", "C1-single-kill", "pre-GONE-RPC", "NOT_COVERED"},
		{"volume.balance", "C1-single-kill", "foreign-VID", "NOT_COVERED"},
		{"volume.balance", "C1-single-kill", "no-op", "NOT_COVERED"},
		{"volume.balance", "C1-single-kill", "wrong-bytes", "FAIL"},
		{"volume.balance", "C1-single-kill", "pending-effect", "UNKNOWN"},
		{"volume.balance", "C1-single-kill", "lock-unreadable", "UNKNOWN"},
		{"volume.balance", "C1-single-kill", "lost-prepare-reply", "UNKNOWN"},
		{"volume.balance", "C1-single-kill", "preserve-failed", "UNKNOWN"},
		{"volume.balance", "C1-single-kill", "backstop-fired", "UNKNOWN"},
	} {
		t.Run(tc.task+"/"+tc.scenario+"/"+tc.variant, func(t *testing.T) {
			p := &processFake{ports: map[string]int{}, stage: &chaosFake{stamps: map[string]int{}, killed: map[string]bool{}}, mixed: &chaos2Fake{stamps: map[string]int{}, killed: map[string]bool{}, cases: map[string]string{}}}
			f := &maintenanceCallerHost{base: p, variant: tc.variant, prepared: map[string]maintenanceRootRecord{}, ready: make(chan struct{}), release: make(chan struct{}), postReadback: make(chan struct{}), secondEntered: make(chan struct{}), restartSeen: make(chan struct{})}
			a, pin, key, _ := cloudSSH(t, "", f.exec)
			b, pin2, _, _ := cloudSSH(t, "", f.exec)
			var d inventory.Document
			json.Unmarshal(directInput(t, a), &d)
			d.Deadlines = inventory.Deadlines{Work: time.Now().Add(1200 * time.Second).UTC().Format(time.RFC3339Nano), Hard: time.Now().Add(1500 * time.Second).UTC().Format(time.RFC3339Nano), CleanupReserveMS: 60000}
			d.Hosts[0].Management.HostKey = &pin
			second := d.Hosts[0]
			second.ID = "second"
			second.Data.Address = "10.84.1.11"
			f.secondData = second.Data.Address
			parts := strings.Split(b, ":")
			second.Management.Address = parts[0]
			second.Management.Port, _ = strconv.Atoi(parts[1])
			second.Management.HostKey = &pin2
			d.Hosts = append(d.Hosts, second)
			p.baseRoot = d.Hosts[0].RunRoot
			p.data = d.Hosts[0].Data.Address
			for i := 1; i <= 6; i++ {
				p.ports[strconv.Itoa(i)] = servicePort(d.Hosts[0], "volume_http") + i - 1
			}
			c := CloudConfig{MaintenanceTask: tc.task, ClusterChaosCases: []string{tc.scenario}, Replication: "001", SSHKey: key, Output: t.TempDir(), Files: map[string]CloudFile{}}
			for _, name := range []string{"weed", "weed-volume", "native.tar"} {
				path := filepath.Join(t.TempDir(), name)
				os.WriteFile(path, []byte(name), 0600)
				c.Files[name] = CloudFile{Path: path, SHA256: hash([]byte(name))}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Second)
			defer cancel()
			path, err := RunCloudHTTP(ctx, processJSON(d), c)
			if tc.status == "PASS" && err != nil {
				t.Fatalf("caller failed: %v", err)
			}
			if tc.status != "PASS" && err == nil {
				t.Fatal("non-pass became success")
			}
			raw, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatalf("missing result %q: %v; caller=%v", path, readErr, err)
			}
			var row map[string]any
			json.Unmarshal(raw, &row)
			if row["status"] != tc.status {
				t.Fatalf("wanted %s got %s; caller=%v", tc.status, raw, err)
			}
			if tc.scenario == "C2-rolling" && (row["total_faults"] != float64(6) || row["coverage"] != "NOT_COVERED") {
				t.Fatalf("partial rolling coverage became a pass: %s", raw)
			}
			if f.arms != 2 || f.cleanupCalls != 2 {
				t.Fatalf("lifecycle calls arm=%d cleanup=%d", f.arms, f.cleanupCalls)
			}
			if tc.variant == "preserve-failed" || tc.variant == "backstop-fired" || tc.variant == "lost-prepare-reply" || tc.variant == "pending-effect" || tc.variant == "lock-unreadable" {
				if f.disarms != 0 {
					t.Fatal("lost proof disarmed backstops")
				}
			} else if f.disarms != 2 {
				t.Fatal("clean hosts did not disarm")
			}
		})
	}
}
