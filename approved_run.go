package testrunner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/pingqiu/sw-test-runner/infra"
	"gopkg.in/yaml.v3"
)

// Bootstrap is service configuration. Its policy pin names separate run-policy
// bytes; the GO binds that digest. Keeping the GO locator outside those bytes
// avoids a circular policy-hash/GO-hash definition.
type WorkerLeaseConfig struct {
	LockFile string `json:"lock_file"`
}

type PolicyBootstrap struct {
	WorkerLease *WorkerLeaseConfig `json:"worker_lease,omitempty"`
	Version     string             `json:"version"`
	Approval    FilePin            `json:"approval"`
	Policy      FilePin            `json:"policy"`
	RunningDir  string             `json:"running_dir"`
	LedgerDir   string             `json:"ledger_dir"`
	CancelFile  string             `json:"cancel_file"`
	KeyFiles    map[string]string  `json:"key_files"`
	ResultsDir  string             `json:"results_dir"`
}

type TrustedPolicy struct {
	file      FilePin
	owner     uint32
	bootstrap PolicyBootstrap
}
type ApprovedRunPolicy struct {
	Version     string                   `json:"version"`
	Runner      string                   `json:"runner"`
	Deployments []infra.DeploymentPolicy `json:"deployments"`
	Jobs        map[string]JobPin        `json:"jobs"`
	Scripts     []FilePin                `json:"scripts"`
}
type ApprovedInventory struct {
	Version string         `json:"version"`
	Hosts   []ApprovedHost `json:"hosts"`
}
type ApprovedArtifacts struct {
	Version     string                   `json:"version"`
	Deployments []infra.DeploymentPolicy `json:"deployments"`
}

// Runtime is a trusted compiled-adapter dependency, never decoded from the
// queue/YAML. nil selects the concrete SSH implementation. CPU controls use a
// fake backend and explicitly make no real-host qualification claim.
type ApprovedRuntime struct {
	Jobs     OwnedJobBackend
	Readback func(context.Context, infra.DeploymentPolicy) (infra.DeploymentReceipt, error)
}

type ApprovedRun struct {
	policy                      TrustedPolicy
	requestPath                 string
	request                     ClaimedRequest
	approval                    ApprovedGO
	lease                       LeaseObservation
	runPolicy                   ApprovedRunPolicy
	scenario                    *Scenario
	inputs                      S0bExecutionInputs
	snapshots                   map[string][]byte
	runtime                     ApprovedRuntime
	readbacks                   map[string]infra.DeploymentReceipt
	scope                       *RunScope
	mu                          sync.Mutex
	workErr                     error
	closing                     bool
	readers                     int
	readerChanged               chan struct{}
	watchStop                   context.CancelFunc
	watchDone                   chan struct{}
	closeOnce                   sync.Once
	closeErr                    error
	engineStarted, engineJoined bool
}

func ReadTrustedPolicy(name string, owner uint32) (TrustedPolicy, error) {
	var p TrustedPolicy
	raw, err := snapshotRecord(name, &owner, true)
	if err != nil {
		return p, err
	}
	if err = decodeStrictJSON(raw, &p.bootstrap); err != nil {
		return p, err
	}
	if p.bootstrap.Version != "s0b-bootstrap-v1" {
		return p, errors.New("unsupported trusted bootstrap")
	}
	for _, v := range []string{p.bootstrap.RunningDir, p.bootstrap.LedgerDir, p.bootstrap.CancelFile, p.bootstrap.ResultsDir} {
		if !filepath.IsAbs(v) || filepath.Clean(v) != v || v == "/" {
			return p, errors.New("noncanonical trusted location")
		}
	}
	for _, v := range []FilePin{p.bootstrap.Approval, p.bootstrap.Policy} {
		if !filepath.IsAbs(v.Path) || filepath.Clean(v.Path) != v.Path || !validSHA256(v.SHA256) {
			return p, errors.New("missing independent file pin")
		}
	}
	sum := sha256.Sum256(raw)
	p.file = FilePin{name, hex.EncodeToString(sum[:])}
	p.owner = owner
	return p, nil
}

func pinnedBytes(pin FilePin) ([]byte, error) {
	raw, err := snapshotRecord(pin.Path, nil, false)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != pin.SHA256 {
		return nil, fmt.Errorf("pinned input changed: %s", pin.Path)
	}
	return raw, nil
}
func equalJSON(a, b any) bool {
	x, e := json.Marshal(a)
	y, f := json.Marshal(b)
	return e == nil && f == nil && bytes.Equal(x, y)
}

func OpenApprovedRun(ctx context.Context, requestPath string, p TrustedPolicy, provided *ApprovedRuntime) (*ApprovedRun, error) {
	a := &ApprovedRun{policy: p, requestPath: requestPath, readbacks: map[string]infra.DeploymentReceipt{}, readerChanged: make(chan struct{})}
	if err := a.readBounded(ctx, 5*time.Second, a.capture); err != nil {
		return nil, err
	}
	if provided == nil {
		nodes := map[string]*infra.Node{}
		for _, deployment := range a.runPolicy.Deployments {
			nodes[deployment.NodeID] = &infra.Node{Host: deployment.Host, User: deployment.User, HostKey: deployment.HostKey, KeyFile: p.bootstrap.KeyFiles[deployment.NodeID]}
		}
		a.runtime = ApprovedRuntime{Jobs: &SSHJobs{Nodes: nodes, Runner: a.runPolicy.Runner}, Readback: func(ctx context.Context, p infra.DeploymentPolicy) (infra.DeploymentReceipt, error) {
			return nodes[p.NodeID].VerifyDeployment(ctx, p)
		}}
	} else {
		a.runtime = *provided
	}
	if a.runtime.Jobs == nil || a.runtime.Readback == nil {
		return nil, errors.New("approved runtime is incomplete")
	}
	probeCtx, probeCancel := context.WithDeadline(ctx, a.request.Binding.WorkDeadline)
	defer probeCancel()
	for _, deployment := range a.runPolicy.Deployments {
		if err := a.readback(probeCtx, deployment); err != nil {
			return nil, err
		}
	}
	b := a.request.Binding
	admission := RunAdmission{RunID: b.RunID, Workload: b.Workload, RequestID: b.RequestID, ManifestSHA256: b.Artifacts.SHA256, OutputDir: b.OutputDir, WorkDeadline: b.WorkDeadline, HardDeadline: b.HardDeadline, CleanupReserve: time.Duration(b.CleanupReserveMS) * time.Millisecond}
	guard := &approvedJobs{run: a, jobs: map[string]approvedJob{}, faults: map[string]registeredFault{}}
	s, err := NewS0bRunScope(ctx, admission, guard, func(ctx context.Context, _ RunAdmission) error { return a.authority(ctx, 5*time.Second) }, a.inputs)
	if err != nil {
		return nil, err
	}
	a.scope = s
	a.startWatch()
	return a, nil
}

func (a *ApprovedRun) capture() error {
	if err := a.captureInputs(); err != nil {
		return err
	}
	lease, err := ReadLiveLease(a.policy.bootstrap.LedgerDir, a.request.Binding.LeaseID, a.request.Binding.LeaseOwner)
	if err == nil {
		a.lease = lease
	}
	return err
}

// Pre-lease validation is private to protected worker bootstrap. Ordinary
// admission/watch always use capture, including its unchanged ReadLiveLease.
func (a *ApprovedRun) captureInputs() error {
	p := a.policy
	if p.file.Path == "" {
		return errors.New("trusted policy was not loaded")
	}
	profile, err := snapshotRecord(p.file.Path, &p.owner, true)
	if err != nil {
		return err
	}
	profileSum := sha256.Sum256(profile)
	if hex.EncodeToString(profileSum[:]) != p.file.SHA256 {
		return errors.New("trusted bootstrap changed")
	}
	request, err := ReadClaimedRequest(a.requestPath, p.bootstrap.RunningDir, p.owner)
	if err != nil {
		return err
	}
	approval, err := ReadApprovedGO(p.bootstrap.Approval, p.bootstrap.CancelFile, p.owner)
	if err != nil {
		return err
	}
	if request.SHA256 != approval.RequestSHA256 || !equalJSON(request.Binding, approval.Binding) || request.Binding.Policy != p.bootstrap.Policy {
		return errors.New("request/GO/policy binding mismatch")
	}
	b := request.Binding
	if b.RunID == "" || (b.Workload != "kv" && b.Workload != "s3") || (b.Transport != "http" && b.Transport != "rdma") || len(b.Hosts) == 0 || b.LeaseResource == "" || !path.IsAbs(b.OutputDir) || path.Clean(b.OutputDir) != b.OutputDir || b.OutputDir == "/" {
		return errors.New("incomplete exact run binding")
	}
	if !b.WorkDeadline.After(time.Now()) || b.CleanupReserveMS <= 0 || b.CleanupReserveMS > 60000 || b.WorkDeadline.Add(time.Duration(b.CleanupReserveMS)*time.Millisecond).After(b.HardDeadline) {
		return errors.New("invalid original authority envelope")
	}
	policyRaw, err := pinnedBytes(b.Policy)
	if err != nil {
		return err
	}
	var runPolicy ApprovedRunPolicy
	if err = decodeStrictJSON(policyRaw, &runPolicy); err != nil {
		return err
	}
	if runPolicy.Version != "s0b-run-policy-v1" || !path.IsAbs(runPolicy.Runner) || len(runPolicy.Deployments) != len(b.Hosts) {
		return errors.New("incomplete approved run policy")
	}
	invRaw, err := pinnedBytes(b.Inventory)
	if err != nil {
		return err
	}
	var inventory ApprovedInventory
	if err = decodeStrictJSON(invRaw, &inventory); err != nil {
		return err
	}
	if inventory.Version != "s0b-inventory-v1" || !equalJSON(inventory.Hosts, b.Hosts) {
		return errors.New("inventory/GO host set mismatch")
	}
	artRaw, err := pinnedBytes(b.Artifacts)
	if err != nil {
		return err
	}
	var artifacts ApprovedArtifacts
	if err = decodeStrictJSON(artRaw, &artifacts); err != nil {
		return err
	}
	if artifacts.Version != "s0b-artifacts-v1" || !equalJSON(artifacts.Deployments, runPolicy.Deployments) {
		return errors.New("artifact set differs from approved deployment")
	}
	seen := map[string]bool{}
	native := false
	for _, host := range b.Hosts {
		if host.Node == "" || seen[host.Node] || host.Host == "" || host.User == "" || host.HostKey == "" || host.Root != b.OutputDir || !filepath.IsAbs(p.bootstrap.KeyFiles[host.Node]) {
			return errors.New("host/root/key binding incomplete")
		}
		seen[host.Node] = true
		found := false
		for _, d := range runPolicy.Deployments {
			if d.NodeID != host.Node {
				continue
			}
			if found || d.Host != host.Host || d.User != host.User || d.HostKey != host.HostKey {
				return errors.New("deployment/GO host mismatch")
			}
			found = true
			for _, file := range d.Files {
				for _, role := range file.Roles {
					if role == "native" {
						if file.Size <= 0 {
							return errors.New("empty native artifact")
						}
						native = true
					}
				}
			}
		}
		if !found {
			return errors.New("approved host missing deployment")
		}
	}
	if b.Transport == "rdma" && !native {
		return errors.New("RDMA authority requires the exact nonempty native set")
	}
	if err := validateApprovedPolicy(runPolicy, b, approval.Cleanup); err != nil {
		return err
	}
	scenarioRaw, err := pinnedBytes(b.Scenario)
	if err != nil {
		return err
	}
	var rawScenario Scenario
	if err = yaml.Unmarshal(scenarioRaw, &rawScenario); err != nil {
		return err
	}
	for _, phase := range rawScenario.Phases {
		if phase.Include != "" {
			return errors.New("S0b v1 requires already-expanded pinned scenario bytes")
		}
	}
	if rawScenario.Cluster != nil || len(rawScenario.Targets) != 0 || len(rawScenario.Topology.Agents) != 0 {
		return errors.New("unreviewed automatic cluster/target/agent lifecycle")
	}
	scenario, err := Parse(scenarioRaw)
	if err != nil {
		return err
	}
	for id, node := range scenario.Topology.Nodes {
		if !seen[id] || node.IsLocal || node.Agent != "" {
			return errors.New("scenario selects an unapproved or local node")
		}
		for _, h := range b.Hosts {
			if h.Node == id && (node.Host != h.Host || node.User != h.User || node.HostKey != h.HostKey || (node.KeyFile != "" && node.KeyFile != p.bootstrap.KeyFiles[id])) {
				return errors.New("scenario node differs from approved inventory")
			}
		}
	}
	inputs := S0bExecutionInputs{Scenario: scenarioRaw, Scripts: map[string][]byte{}}
	if len(runPolicy.Scripts) > 128 {
		return errors.New("script set exceeds capture bound")
	}
	totalScriptBytes := 0
	for _, pin := range runPolicy.Scripts {
		body, err := pinnedBytes(pin)
		if err != nil {
			return err
		}
		totalScriptBytes += len(body)
		if totalScriptBytes > 8<<20 {
			return errors.New("script snapshot bytes exceed capture bound")
		}
		if _, ok := inputs.Scripts[pin.Path]; ok {
			return errors.New("duplicate script input")
		}
		inputs.Scripts[pin.Path] = body
	}
	a.request, a.approval, a.runPolicy, a.scenario, a.inputs = request, approval, runPolicy, scenario, inputs
	a.snapshots = map[string][]byte{"claimed-request.json": request.Bytes, "approved-go.json": approval.Bytes, "inventory.json": invRaw, "run-policy.json": policyRaw, "artifacts.json": artRaw}
	return nil
}

func (a *ApprovedRun) checkWork() error {
	fresh := &ApprovedRun{policy: a.policy, requestPath: a.requestPath}
	if err := fresh.capture(); err != nil {
		return err
	}
	if a.request.SHA256 != "" && a.request.SHA256 != fresh.request.SHA256 {
		return errors.New("work authority changed")
	}
	return nil
}

func (a *ApprovedRun) readback(ctx context.Context, p infra.DeploymentPolicy) error {
	a.mu.Lock()
	if old, ok := a.readbacks[p.NodeID]; ok {
		p.ExpectedBootID = old.BootID
	}
	a.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	started := time.Now()
	receipt, err := a.runtime.Readback(ctx, p)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(p)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(encoded)
	if ctx.Err() != nil || receipt.PolicySHA256 != hex.EncodeToString(sum[:]) || receipt.ReadStart.IsZero() || receipt.ReadEnd.Before(receipt.ReadStart) || receipt.ReadStart.Before(started) || receipt.ReadEnd.After(time.Now()) {
		return errors.New("deployment policy/clock bracket is not bound to this read")
	}
	if receipt.NodeID != p.NodeID || receipt.Host != p.Host || receipt.User != p.User || receipt.HostKey != p.HostKey || receipt.BootID == "" || (p.ExpectedBootID != "" && receipt.BootID != p.ExpectedBootID) || receipt.WorkloadUID != p.WorkloadUID || len(receipt.Files) != len(p.Files) {
		return errors.New("deployment identity/set mismatch")
	}
	for _, f := range p.Files {
		if receipt.Files[f.Path] != f.SHA256 {
			return fmt.Errorf("foreign deployed bytes: %s", f.Path)
		}
	}
	a.mu.Lock()
	receipt.Files = maps.Clone(receipt.Files)
	a.readbacks[p.NodeID] = receipt
	a.mu.Unlock()
	return nil
}

func (a *ApprovedRun) Scope() *RunScope { return a.scope }
func (a *ApprovedRun) Close() error {
	a.closeOnce.Do(func() {
		a.mu.Lock()
		a.closing = true
		a.mu.Unlock()
		if a.watchStop != nil {
			a.watchStop()
		}
		err := a.scope.Close()
		ctx, cancel := a.scope.cleanupContext(context.Background())
		defer cancel()
		for {
			a.mu.Lock()
			pending, changed := a.readers, a.readerChanged
			a.mu.Unlock()
			if pending == 0 {
				break
			}
			select {
			case <-changed:
			case <-ctx.Done():
				err = errors.Join(err, errors.New("authority reader settlement UNKNOWN"))
				goto joined
			}
		}
	joined:
		if a.watchDone != nil {
			select {
			case <-a.watchDone:
			case <-ctx.Done():
				err = errors.Join(err, errors.New("authority watcher join UNKNOWN"))
			}
		}
		a.mu.Lock()
		a.closeErr = errors.Join(err, a.workErr)
		a.mu.Unlock()
	})
	return a.closeErr
}

func (a *ApprovedRun) revoke(err error) {
	a.mu.Lock()
	if a.workErr == nil && !a.closing {
		a.workErr = err
	}
	scope := a.scope
	a.mu.Unlock()
	if scope != nil {
		scope.cancel()
	}
}
func (a *ApprovedRun) Execute(ctx context.Context, engine *Engine, actx *ActionContext) (result *ScenarioResult) {
	if actx == nil {
		actx = &ActionContext{}
	}
	if engine == nil || engine.registry == nil || (actx.Safety != nil && actx.Safety != a.scope) || actx.Coordinator != nil || len(actx.Targets) != 0 {
		_ = a.Close()
		return &ScenarioResult{Status: StatusFail, Error: "foreign scope/automatic executor"}
	}
	// Raw shell/target adapters are not an alternate route around owned jobs.
	for id := range actx.Nodes {
		actx.Nodes[id] = refusedApprovedNode{}
	}
	actx.approved = a
	actx.Safety, actx.TempRoot = a.scope, a.request.Binding.OutputDir
	actx.Scenario = a.scenario
	registry := *engine.registry
	registry.AllowMutating = a.approval.AllowMutating
	boundEngine := *engine
	boundEngine.registry = &registry
	ctx, cancel := context.WithDeadline(ctx, a.request.Binding.WorkDeadline)
	defer cancel()
	stop := context.AfterFunc(a.scope.ctx, cancel)
	defer stop()
	a.mu.Lock()
	a.engineStarted = true
	a.mu.Unlock()
	finished := make(chan *ScenarioResult, 1)
	go func() {
		var r *ScenarioResult
		defer func() {
			if p := recover(); p != nil {
				r = &ScenarioResult{Status: StatusFail, Error: fmt.Sprintf("approved engine panic: %v", p)}
			}
			finished <- r
		}()
		r = boundEngine.Run(ctx, a.scenario, actx)
	}()
	join, joinCancel := context.WithDeadline(context.Background(), a.request.Binding.HardDeadline)
	defer joinCancel()
	select {
	case result = <-finished:
		a.mu.Lock()
		a.engineJoined = true
		a.mu.Unlock()
	case <-join.Done():
		cancel()
		result = &ScenarioResult{Status: StatusFail, Error: "approved engine join UNKNOWN at original hard deadline"}
	}
	if result == nil {
		result = &ScenarioResult{Status: StatusFail, Error: "missing engine result"}
	}
	if err := a.Close(); err != nil {
		result.Status = StatusFail
		result.Error = strings.TrimSpace(result.Error + "; " + err.Error())
	}
	result.RunID = a.request.Binding.RunID
	return result
}

type refusedApprovedNode struct{}

func (refusedApprovedNode) Run(context.Context, string) (string, string, int, error) {
	return "", "", -1, errors.New("S0b requires an owned job; raw shell refused")
}
func (n refusedApprovedNode) RunRoot(ctx context.Context, cmd string) (string, string, int, error) {
	return n.Run(ctx, cmd)
}
func (refusedApprovedNode) Upload(string, string) error {
	return errors.New("S0b input staging requires separately approved deployment")
}
func (refusedApprovedNode) Close() {}
