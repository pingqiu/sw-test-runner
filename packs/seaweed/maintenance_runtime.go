package seaweed

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pingqiu/sw-test-runner/infra"
	"github.com/pingqiu/sw-test-runner/inventory"
)

type maintenanceObject struct {
	Key     string `json:"key"`
	FID     string `json:"fid"`
	SHA     string `json:"expected_sha256"`
	Length  int64  `json:"expected_length"`
	Deleted bool   `json:"deleted"`
}
type maintenanceManifest struct {
	RunID      string              `json:"run_id"`
	Task       string              `json:"task"`
	Collection string              `json:"collection"`
	VID        string              `json:"vid"`
	Objects    []maintenanceObject `json:"objects"`
}
type maintenanceFile struct {
	Name  string `json:"name"`
	Size  int64  `json:"size"`
	Inode uint64 `json:"inode"`
	UID   uint32 `json:"uid"`
	SHA   string `json:"sha256,omitempty"`
}
type maintenanceScope struct {
	maintenanceSubject
	Files    []maintenanceFile          `json:"files"`
	Metadata map[string]json.RawMessage `json:"metadata"`
	Pending  int                        `json:"pending"`
	Ordinary bool                       `json:"ordinary"`
	Mounted  bool                       `json:"mounted,omitempty"`
	ReadOnly bool                       `json:"read_only,omitempty"`
	Volumes  map[string]bool            `json:"volumes,omitempty"`
	DataVIDs []string                   `json:"data_vids,omitempty"`
}
type maintenanceIdentity struct {
	PID       int    `json:"pid"`
	Start     string `json:"start"`
	Boot      string `json:"boot"`
	Task      string `json:"task"`
	RunID     string `json:"run_id"`
	ConfigSHA string `json:"config_sha256"`
}

func (i maintenanceIdentity) owner() string { return fmt.Sprintf("%d/%s", i.PID, i.Start) }

type maintenanceTaskResult struct {
	Job          string              `json:"job"`
	Attempt      string              `json:"attempt"`
	Terminal     string              `json:"terminal"`
	Refusal      string              `json:"refusal"`
	EffectsKnown bool                `json:"effects_known"`
	Exit         *int                `json:"exit"`
	Completion   json.RawMessage     `json:"completion,omitempty"`
	Diagnostic   string              `json:"diagnostic,omitempty"`
	Started      float64             `json:"started"`
	Ended        float64             `json:"ended"`
	Identity     maintenanceIdentity `json:"identity"`
}
type maintenanceProgress struct {
	Event string  `json:"event"`
	At    float64 `json:"at"`
	VID   string  `json:"vid"`
	Bytes int64   `json:"bytes,omitempty"`
	Begin float64 `json:"begin,omitempty"`
}
type maintenanceSample struct {
	Before   float64                `json:"before"`
	After    float64                `json:"after"`
	Identity *maintenanceIdentity   `json:"identity"`
	Alive    bool                   `json:"alive"`
	Boot     string                 `json:"boot"`
	Result   *maintenanceTaskResult `json:"result"`
	Events   []maintenanceProgress  `json:"events,omitempty"`
	Job      json.RawMessage        `json:"job,omitempty"`
	Scratch  []struct {
		Path string `json:"path"`
		Size int64  `json:"size"`
	} `json:"scratch,omitempty"`
	Progress *struct {
		State     string  `json:"state"`
		VID       string  `json:"vid"`
		Operation string  `json:"operation"`
		Bytes     int64   `json:"bytes"`
		Begin     float64 `json:"begin"`
		Source    string  `json:"source"`
		Target    string  `json:"target"`
	} `json:"progress"`
}
type maintenanceFaultSample struct {
	Victim, Role           string
	Before, After          maintenanceSample
	Subject                maintenanceSubject
	Missing                []int
	Confirmed              bool
	TargetEndpoint         string
	TargetUnit             string
	GoneWitness            string
	BeforePrecedesDispatch bool
	AfterFollowsGone       bool
	goneAt                 time.Time
}
type maintenanceRuntime struct {
	d                inventory.Document
	c                CloudConfig
	r                *chaosRun
	hosts            map[string]inventory.Host
	budget           maintenanceBudget
	base             map[string]any
	manifest         maintenanceManifest
	subject          maintenanceSubject
	initial          map[string]maintenanceScope
	missing          []int
	mu               sync.Mutex
	faults           []*maintenanceFaultSample
	injecting        bool
	done             chan struct{}
	taskResult       maintenanceTaskResult
	taskErr          error
	armed            []inventory.Host
	started          time.Time
	beforeTask       map[string]maintenanceScope
	receipts         map[string]maintenanceRootRecord
	effects          bool
	fixtureVIDs      map[string]bool
	mixedWrong       bool
	failureCancel    context.CancelFunc
	latest           maintenanceSample
	observerNode     *infra.Node
	observerSequence int
	retain           bool
}

func newMaintenanceRuntime(ctx context.Context, d inventory.Document, c CloudConfig) *maintenanceRuntime {
	now := time.Now()
	m := &maintenanceRuntime{d: d, c: c, budget: maintenanceNewBudget(ctx, now), started: now, initial: map[string]maintenanceScope{}}
	if hard, e := time.Parse(time.RFC3339Nano, d.Deadlines.Hard); e == nil && hard.Before(m.budget.hard) {
		m.budget.hard = hard
		m.budget.work = hard.Add(-maintenanceReserve)
	}
	if work, e := time.Parse(time.RFC3339Nano, d.Deadlines.Work); e == nil && work.Before(m.budget.work) {
		m.budget.work = work
	}
	return m
}
func (m *maintenanceRuntime) ec() bool { return strings.HasPrefix(m.c.MaintenanceTask, "ec_") }
func (m *maintenanceRuntime) phase(ctx context.Context, maximum time.Duration) (context.Context, context.CancelFunc, error) {
	if err := m.budget.admit(time.Now(), maximum); err != nil {
		return nil, nil, err
	}
	limit := time.Now().Add(maximum)
	if limit.After(m.budget.work) {
		limit = m.budget.work
	}
	c, cancel := context.WithDeadline(ctx, limit)
	return c, cancel, nil
}
func (m *maintenanceRuntime) extend(view *inventory.Document, docs map[string]inventory.Document, physical map[string]string) error {
	if !m.ec() {
		return nil
	}
	for i := 0; i < 14; i++ {
		h := m.d.Hosts[i/7]
		id := "ec" + strconv.Itoa(i)
		physical[id] = h.ID
		h.ID = id
		h.Roles = []string{"volume"}
		h.Services = []inventory.Service{{Name: "volume_http", Port: servicePort(m.d.Hosts[i/7], "volume_http") + 20 + i%7}, {Name: "volume_grpc", Port: servicePort(m.d.Hosts[i/7], "volume_grpc") + 20 + i%7}}
		rd := m.d
		rd.RunID += "-" + id
		h.RunRoot = cloudRunRoot(rd, h)
		docs[id] = rd
		view.Hosts = append(view.Hosts, h)
	}
	for i := range view.Hosts {
		if view.Hosts[i].ID == "meta" {
			p := servicePort(view.Hosts[i], "master_http") + 30000
			for j, name := range []string{"maintenance_admin_http", "maintenance_worker_metrics", "maintenance_worker_debug"} {
				view.Hosts[i].Services = append(view.Hosts[i].Services, inventory.Service{Name: name, Port: p + j})
			}
			view.Hosts[i].Services = append(view.Hosts[i].Services, inventory.Service{Name: "maintenance_admin_grpc", Port: p + 10000})
		}
	}
	seen := map[string]bool{}
	for _, h := range view.Hosts {
		for _, s := range h.Services {
			key := fmt.Sprintf("%s:%d", h.Management.Address, s.Port)
			if s.Port < 1 || s.Port > 65535 || seen[key] {
				return errors.New("maintenance listener overflow/collision")
			}
			seen[key] = true
		}
	}
	return nil
}
func (m *maintenanceRuntime) bind(r *chaosRun, hosts map[string]inventory.Host) {
	m.r, m.hosts = r, hosts
	r.maintenance = m
	h := hosts["meta"]
	m.base = map[string]any{"root": h.RunRoot, "run_id": m.d.RunID, "task": m.c.MaintenanceTask, "collection": "maint-" + m.d.RunID, "weed": h.RunRoot + "/weed", "master": endpoint(h, "master_http"), "seed_node": endpoint(hosts["vol1"], "volume_http")}
	if m.ec() {
		m.base["seed_node"] = endpoint(hosts["ec0"], "volume_http")
		m.base["admin"] = "http://127.0.0.1:" + strconv.Itoa(servicePort(h, "maintenance_admin_http"))
	}
	m.base["weed_sha256"] = m.c.Files["weed"].SHA256
	helper, _ := workerFiles.ReadFile("worker/maintenance.py")
	m.base["helper_sha256"] = hash(helper)
	m.base["task_unit"] = "sw-cloud-" + r.docs["meta"].RunID + "-maintenance-task.service"
	m.base["config_sha256"] = hash(processJSON(m.base))
}
func (m *maintenanceRuntime) command(id, stage string, extras map[string]any) string {
	config := map[string]any{}
	for k, v := range m.base {
		config[k] = v
	}
	config["root"] = m.hosts[id].RunRoot
	config["stage"] = stage
	for k, v := range extras {
		config[k] = v
	}
	return "python3 -I " + cloudQuote(m.hosts[id].RunRoot+"/worker/maintenance.py") + " " + cloudQuote(string(processJSON(config)))
}
func (m *maintenanceRuntime) leaf(ctx context.Context, id, stage string, seconds int, extras map[string]any, dst any) error {
	out, err := m.r.step(ctx, id, "maintenance-"+stage+"-"+id, seconds, m.command(id, stage, extras))
	if err != nil {
		return err
	}
	if dst != nil {
		if err = maintenanceDecode([]byte(out), dst); err != nil {
			return fmt.Errorf("maintenance %s malformed reply: %w", stage, err)
		}
	}
	return nil
}
func (m *maintenanceRuntime) checkManifest(man maintenanceManifest) error {
	if man.RunID != m.d.RunID || man.Task != m.c.MaintenanceTask || man.Collection != m.base["collection"] || len(man.Objects) != 16 {
		return errors.New("maintenance manifest binding differs")
	}
	vid, err := strconv.ParseUint(man.VID, 10, 32)
	if err != nil || vid == 0 || strconv.FormatUint(vid, 10) != man.VID {
		return errors.New("maintenance VID invalid")
	}
	seenFID := map[string]bool{}
	for i, o := range man.Objects {
		key := fmt.Sprint("maintenance-", i)
		block := sha256.Sum256([]byte(m.d.RunID + "/" + key))
		length := int64(1 << 20)
		if m.c.MaintenanceTask == "volume.balance" {
			length = 8 << 20
		}
		payload := strings.Repeat(string(block[:]), int(length)/len(block))
		if seenFID[o.FID] || o.Key != key || o.Length != length || o.SHA != hash([]byte(payload)) || !regexp.MustCompile(`^`+man.VID+`,[0-9a-f]{9,24}$`).MatchString(o.FID) || o.Deleted && m.c.MaintenanceTask != "ec_vacuum" {
			return errors.New("maintenance ACK manifest identity/bytes differ")
		}
		seenFID[o.FID] = true
	}
	return nil
}
func (m *maintenanceRuntime) setup(ctx context.Context, phase func(string, inventory.Host) error) error {
	if m.ec() {
		for i := 0; i < 14; i++ {
			h := m.hosts["ec"+strconv.Itoa(i)]
			if err := phase("prepare", h); err != nil {
				return err
			}
			if err := phase("deploy", h); err != nil {
				return err
			}
		}
	}
	if err := m.leaf(ctx, "meta", "init", 10, nil, &struct {
		Initialized bool `json:"initialized"`
	}{}); err != nil {
		return err
	}
	if m.ec() {
		if err := m.startPlugins(ctx); err != nil {
			return err
		}
		if err := m.leaf(ctx, "meta", "plugin_preflight", 30, nil, &map[string]any{}); err != nil {
			return err
		}
	}
	if err := m.leaf(ctx, "meta", "seed", 60, nil, &m.manifest); err != nil {
		return err
	}
	if err := m.checkManifest(m.manifest); err != nil {
		return err
	}
	m.subject = maintenanceSubject{Collection: m.manifest.Collection, VID: m.manifest.VID}
	if !m.ec() {
		var err error
		m.initial, err = m.scopes(ctx, true)
		if err != nil {
			return err
		}
		m.fixtureVIDs, err = maintenanceFreezeCollection(m.initial, m.manifest.VID, m.c.MaintenanceTask)
		if err != nil {
			return err
		}
		if err = m.r.save("maintenance-fixture-volumes.json", m.fixtureVIDs); err != nil {
			return err
		}
	}
	if m.ec() {
		if err := m.leaf(ctx, "meta", "encode", 90, nil, &m.manifest); err != nil {
			return err
		}
		if err := m.checkManifest(m.manifest); err != nil {
			return err
		}
		var err error
		m.initial, err = m.scopes(ctx, true)
		if err != nil {
			return err
		}
		shards := map[int]string{}
		for id, s := range m.initial {
			if len(s.Shards) > 0 {
				if m.subject.EncodeNS == "" {
					m.subject.EncodeNS = s.EncodeNS
					m.subject.Generation = s.Generation
				}
				if s.EncodeNS != m.subject.EncodeNS || s.Generation != m.subject.Generation {
					return errors.New("maintenance initial EC generations differ")
				}
				for _, n := range s.Shards {
					if _, ok := shards[n]; ok {
						return errors.New("maintenance ambiguous initial shard owners")
					}
					shards[n] = id
				}
			}
		}
		if len(shards) != 14 || m.subject.EncodeNS == "" {
			return errors.New("maintenance fixture missing frozen 14-shard generation")
		}
		if m.c.MaintenanceTask == "ec_repair" {
			id := shards[13]
			if err = m.removeFixture(ctx, id, []int{13}); err != nil {
				return err
			}
			m.missing = []int{13}
		}
		if err = m.leaf(ctx, "meta", "detect", 30, nil, &map[string]any{}); err != nil {
			return err
		}
	} else if m.c.MaintenanceTask == "volume.fix.replication" {
		owners, err := m.replicaOwners(ctx)
		if err != nil {
			return err
		}
		if len(owners) != 2 {
			return errors.New("maintenance missing initial two-copy proof")
		}
		id := owners[1]
		if id == "vol1" {
			id = owners[0]
		}
		if err = m.removeFixture(ctx, id, nil); err != nil {
			return err
		}
	}
	var err error
	m.beforeTask, err = m.scopes(ctx, true)
	if err != nil {
		return err
	}
	return m.r.save("maintenance-manifest.json", m.manifest)
}

func (m *maintenanceRuntime) startPlugins(ctx context.Context) error {
	h := m.hosts["meta"]
	root := h.RunRoot + "/maintenance"
	q := cloudQuote
	_, err := m.r.step(ctx, "meta", "maintenance-plugin-directories", 10, "mkdir -m700 "+q(root+"/admin")+" "+q(root+"/worker"))
	if err != nil {
		return err
	}
	admin := q(h.RunRoot+"/weed") + " admin -master=" + q(endpoint(h, "master_http")) + " -port=" + strconv.Itoa(servicePort(h, "maintenance_admin_http")) + " -dataDir=" + q(root+"/admin")
	worker := q(h.RunRoot+"/weed") + " worker -admin=" + q("127.0.0.1:"+strconv.Itoa(servicePort(h, "maintenance_admin_http"))) + " -id=" + q(m.d.RunID+"-maintenance-worker") + " -jobType=" + q(m.c.MaintenanceTask) + " -workingDir=" + q(root+"/worker") + " -maxExecute=1 -maxDetect=1 -metricsPort=" + strconv.Itoa(servicePort(h, "maintenance_worker_metrics")) + " -debug.port=" + strconv.Itoa(servicePort(h, "maintenance_worker_debug"))
	for _, x := range []struct{ name, cmd string }{{"maintenance-admin", admin}, {"maintenance-worker", worker}} {
		cmd := cloudStart(m.r.docs["meta"], h.RunRoot, x.name, "--property=User="+q(h.Management.User)+" --property=UMask=0077 "+x.cmd)
		if _, err = m.r.step(ctx, "meta", x.name, 30, cmd); err != nil {
			return err
		}
	}
	// The API must belong to our live admin, not an exporter on a reused port.
	unit := "sw-cloud-" + m.r.docs["meta"].RunID + "-maintenance-admin"
	cmd := mainPID(unit) + `test "$(sudo -n readlink /proc/$pid/exe)" = ` + q(h.RunRoot+"/weed") + `; args=$(sudo -n cat /proc/$pid/cmdline | tr '\000' '\n'); printf '%s\n' "$args" | grep -Fx -- ` + q("-dataDir="+root+"/admin") + ` >/dev/null; sockets=$(sudo -n ss -Hltnp); printf '%s\n' "$sockets" | awk -v p=` + strconv.Itoa(servicePort(h, "maintenance_admin_http")) + ` -v pid="$pid" '$4 ~ (":" p "$") && index($0,"pid=" pid ","){found=1} END{exit !found}'; test "$(` + chaosStat + `)" = "$start"`
	_, err = m.r.step(ctx, "meta", "maintenance-admin-owner", 30, cmd)
	return err
}

func (m *maintenanceRuntime) scopes(ctx context.Context, hashed bool) (map[string]maintenanceScope, error) {
	ids := []string{}
	if m.ec() {
		for i := 0; i < 14; i++ {
			ids = append(ids, "ec"+strconv.Itoa(i))
		}
	} else {
		for i := 1; i <= 6; i++ {
			ids = append(ids, "vol"+strconv.Itoa(i))
		}
	}
	out := map[string]maintenanceScope{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	var failure error
	for _, id := range ids {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			scope, e := m.scope(ctx, id, hashed)
			mu.Lock()
			defer mu.Unlock()
			if e == nil {
				out[id] = scope
			}
			failure = errors.Join(failure, e)
		}(id)
	}
	wg.Wait()
	return out, failure
}
func (m *maintenanceRuntime) scope(ctx context.Context, id string, hashed bool) (maintenanceScope, error) {
	var s maintenanceScope
	first, _, err := processOwnerReceipt(ctx, m.r, m.hosts, id)
	if err != nil {
		return s, err
	}
	err = m.leaf(ctx, id, "scope", 20, map[string]any{"vid": m.manifest.VID, "hash_files": hashed}, &s)
	if err != nil {
		return s, err
	}
	last, body, err := processOwnerReceipt(ctx, m.r, m.hosts, id)
	if err != nil {
		return s, err
	}
	if first != last || s.VID != m.manifest.VID || s.Collection != m.manifest.Collection {
		return s, errors.New("maintenance owner/generation observation changed")
	}
	var status struct {
		Volumes []struct {
			Id         uint64
			Collection string
			ReadOnly   bool
		}
	}
	if err = json.Unmarshal([]byte(body), &status); err != nil {
		return s, err
	}
	s.Mounted, s.ReadOnly = false, false
	s.Volumes = map[string]bool{}
	for _, v := range status.Volumes {
		if v.Collection == m.manifest.Collection {
			vid := strconv.FormatUint(v.Id, 10)
			if v.Id == 0 {
				return s, errors.New("invalid collection inventory VID")
			}
			if _, exists := s.Volumes[vid]; exists {
				return s, errors.New("duplicate collection inventory VID")
			}
			s.Volumes[vid] = v.ReadOnly
		}
		if strconv.FormatUint(v.Id, 10) == m.manifest.VID && v.Collection == m.manifest.Collection {
			s.Mounted = true
			s.ReadOnly = v.ReadOnly
		}
	}
	if !m.ec() {
		seen := map[string]bool{}
		for _, vid := range s.DataVIDs {
			if seen[vid] {
				return s, errors.New("duplicate collection data VID")
			}
			seen[vid] = true
			if _, ok := s.Volumes[vid]; !ok {
				s.Pending++
			}
		}
		for vid := range s.Volumes {
			if !seen[vid] {
				s.Pending++
			}
		}
	}
	return s, nil
}
func (m *maintenanceRuntime) replicaOwners(ctx context.Context) ([]string, error) {
	states, err := m.scopes(ctx, true)
	if err != nil {
		return nil, err
	}
	m.initial = states
	ids := []string{}
	for id, s := range states {
		if s.Ordinary && s.Mounted {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids, nil
}
func (m *maintenanceRuntime) removeFixture(ctx context.Context, id string, shards []int) error {
	state, ok := m.initial[id]
	if !ok {
		return errors.New("missing fixture owner scope")
	}
	files := []maintenanceFile{}
	prefix := m.manifest.Collection + "_" + m.manifest.VID
	for _, f := range state.Files {
		selected := len(shards) == 0 && (f.Name == prefix+".dat" || f.Name == prefix+".idx" || f.Name == prefix+".vif")
		for _, s := range shards {
			selected = selected || f.Name == fmt.Sprintf("%s.ec%02d", prefix, s)
		}
		if selected {
			files = append(files, f)
		}
	}
	if len(files) == 0 {
		return errors.New("maintenance missing-copy fixture is empty")
	}
	f := &chaos2Fault{r: m.r, d: m.d, hosts: m.hosts, g: m.r.gens}
	if err := f.kill(ctx, id, "volume"); err != nil {
		return err
	}
	cmd := "sudo -n " + m.command(id, "quarantine", map[string]any{"vid": m.manifest.VID, "files": files})
	if _, err := m.r.step(ctx, id, "maintenance-quarantine", 30, cmd); err != nil {
		return err
	}
	return f.start(ctx, id, "volume")
}

func (m *maintenanceRuntime) startTask(ctx context.Context) error {
	m.done = make(chan struct{})
	go func() {
		defer close(m.done)
		h := m.hosts["meta"]
		cmd := cloudStart(m.r.docs["meta"], h.RunRoot, "maintenance-task", "--wait --pipe --collect --property=User="+cloudQuote(h.Management.User)+" --property=UMask=0077 --property=RuntimeMaxSec=580 --property=TimeoutStopSec=3 --property=KillMode=control-group "+m.command("meta", "task", nil))
		out, err := m.r.step(ctx, "meta", "maintenance-task", 590, cmd)
		var result maintenanceTaskResult
		if err == nil {
			err = maintenanceDecode([]byte(out), &result)
			if err == nil {
				err = m.validateTaskResult(result)
			}
		}
		m.mu.Lock()
		m.taskResult, m.taskErr = result, err
		m.mu.Unlock()
	}()
	return nil
}

func (m *maintenanceRuntime) validateTaskResult(result maintenanceTaskResult) error {
	i := result.Identity
	if result.Job != m.d.RunID+"-maintenance" || result.Attempt != "1" || i.PID <= 0 || i.Start == "" || i.Boot == "" || i.Task != m.c.MaintenanceTask || i.RunID != m.d.RunID || i.ConfigSHA != m.base["config_sha256"] || !validMaintenanceBound(maintenanceBound{result.Started, result.Ended}) {
		return errors.New("maintenance terminal reply is not bound to this task/attempt")
	}
	if result.Terminal == "succeeded" && (result.Exit == nil || *result.Exit != 0 || result.Refusal != "") {
		return errors.New("maintenance success has no explicit zero exit")
	}
	if result.Ended-result.Started > 600 {
		return errors.New("maintenance task exceeded its own bounded lifetime")
	}
	if result.Terminal == "refused" && (result.Exit == nil || result.Refusal == "") {
		return errors.New("maintenance refusal has no explicit process result")
	}
	return nil
}

func (m *maintenanceRuntime) taskSnapshot() (maintenanceTaskResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.taskResult, m.taskErr
}

func (m *maintenanceRuntime) sample(ctx context.Context) (maintenanceSample, error) {
	var s maintenanceSample
	if d, ok := ctx.Deadline(); !ok || time.Until(d) < 15*time.Second {
		return s, errors.New("maintenance observer cannot fit maximum")
	}
	m.observerSequence++
	out, err := cloudStep(ctx, m.observerNode, m.c.Output, fmt.Sprintf("maintenance-observe-%06d.json", m.observerSequence), "maintenance-observe", 10, m.command("meta", "sample", nil))
	if err == nil {
		err = maintenanceDecode([]byte(out), &s)
	}
	if err == nil && (!validMaintenanceBound(maintenanceBound{s.Before, s.After}) || s.Boot == "") {
		err = errors.New("invalid maintenance-host clock bracket")
	}
	if err == nil && s.Identity != nil && (s.Identity.ConfigSHA != m.base["config_sha256"] || s.Identity.Task != m.c.MaintenanceTask || s.Identity.RunID != m.d.RunID || s.Identity.Boot != s.Boot || s.Identity.PID <= 0 || s.Identity.Start == "") {
		err = errors.New("foreign maintenance task owner")
	}
	return s, err
}

func (m *maintenanceRuntime) beforeFault(ctx context.Context, id, role string) (*maintenanceFaultSample, error) {
	if !m.injecting {
		return nil, nil
	}
	// Pure snapshot: never add an SSH round trip to the existing fault schedule.
	// A missing/old/idle observation earns no coverage; it never delays a fault.
	m.mu.Lock()
	s := m.latest
	m.mu.Unlock()
	return &maintenanceFaultSample{Victim: id, Role: role, Subject: m.subject, Before: s, BeforePrecedesDispatch: true, TargetEndpoint: endpoint(m.hosts[id], role+"_http"), TargetUnit: m.r.gens.unit(m.r.docs[id], id, role)}, nil
}
func (m *maintenanceRuntime) afterFault(ctx context.Context, f *maintenanceFaultSample, confirmed bool) error {
	if f == nil {
		return nil
	}
	f.Confirmed = confirmed
	f.goneAt = time.Now()
	m.mu.Lock()
	m.faults = append(m.faults, f)
	m.mu.Unlock()
	return nil // the independent observer takes the post-GONE sample
}

// Validate the original bytes before the case-insensitive struct decoder. Raw
// product payloads stay opaque here; their decision fields are read by exact key.
func maintenanceDecode(raw []byte, dst any) error {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return errors.New("null maintenance reply")
	}
	if err := oneStrictValue(raw); err != nil {
		return err
	}
	var validate func([]byte, reflect.Type) error
	validate = func(b []byte, t reflect.Type) error {
		if bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
			if t.Kind() == reflect.Pointer || t.Kind() == reflect.Interface || t == reflect.TypeOf(json.RawMessage{}) {
				return nil
			}
			return errors.New("null maintenance scalar")
		}
		for t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		if t == reflect.TypeOf(json.RawMessage{}) || t.Kind() == reflect.Interface {
			return nil
		}
		if t.Kind() == reflect.Struct {
			fields := map[string]reflect.Type{}
			required := map[string]bool{}
			var add func(reflect.Type)
			add = func(s reflect.Type) {
				for i := 0; i < s.NumField(); i++ {
					f := s.Field(i)
					if f.Anonymous && f.Type.Kind() == reflect.Struct {
						add(f.Type)
						continue
					}
					if f.PkgPath != "" {
						continue
					}
					name := strings.Split(f.Tag.Get("json"), ",")[0]
					if name == "-" {
						continue
					}
					if name == "" {
						name = f.Name
					}
					fields[name] = f.Type
					required[name] = !strings.Contains(","+f.Tag.Get("json")+",", ",omitempty,")
				}
			}
			add(t)
			var values map[string]json.RawMessage
			if err := json.Unmarshal(b, &values); err != nil {
				return err
			}
			if values == nil {
				return errors.New("null maintenance object")
			}
			for key, needed := range required {
				if _, exists := values[key]; needed && !exists {
					return fmt.Errorf("missing maintenance field %q", key)
				}
			}
			for name, value := range values {
				ft, ok := fields[name]
				if !ok {
					return fmt.Errorf("unknown maintenance field %q", name)
				}
				if err := validate(value, ft); err != nil {
					return err
				}
			}
		} else if t.Kind() == reflect.Slice || t.Kind() == reflect.Array {
			var values []json.RawMessage
			if err := json.Unmarshal(b, &values); err != nil {
				return err
			}
			for _, value := range values {
				if err := validate(value, t.Elem()); err != nil {
					return err
				}
			}
		} else if t.Kind() == reflect.Map {
			var values map[string]json.RawMessage
			if err := json.Unmarshal(b, &values); err != nil {
				return err
			}
			for _, value := range values {
				if err := validate(value, t.Elem()); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := validate(raw, reflect.TypeOf(dst)); err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	return d.Decode(dst)
}
