package testrunner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// RunApprovedRequest is the service adapter's strict-data entry. A nil runtime
// selects actual pinned SSH and protected deployment readback, never a fake.
// Packs may register trusted typed handlers; the CLI uses only approved_job,
// not the legacy raw-shell/build/automatic-cluster action registry.
func RunApprovedRequest(ctx context.Context, request, profile string, register func(*Registry), runtime *ApprovedRuntime, log func(string, ...interface{})) (*ScenarioResult, string, error) {
	policy, err := ReadTrustedPolicy(profile, uint32(os.Geteuid()))
	if err != nil {
		return nil, "", err
	}
	run, err := OpenApprovedRun(ctx, request, policy, runtime)
	if err != nil {
		return nil, "", err
	}
	defer run.Close()
	// Results are service-owned local evidence, not a queue-selected directory.
	if err = protectedResults(policy.bootstrap.ResultsDir, policy.owner); err != nil {
		return nil, "", err
	}
	bundle, err := createRunBundleSnapshot(policy.bootstrap.ResultsDir, run.request.Binding.Scenario.Path, []string{"approved-run", "--request", request, "--policy", profile}, run.inputs.Scenario, time.Now())
	if err != nil {
		return nil, "", err
	}
	bundle.Manifest.RunID = run.request.Binding.RunID
	bundle.provenance.RunID = run.request.Binding.RunID
	if err = bundle.writeManifest(); err != nil {
		return nil, bundle.Dir, err
	}
	for name, body := range run.snapshots {
		if err = os.WriteFile(filepath.Join(bundle.ArtifactsDir(), name), body, 0600); err != nil {
			return nil, bundle.Dir, err
		}
	}
	registry := NewRegistry()
	if register != nil {
		register(registry)
	}
	registry.RegisterFunc("approved_job", TierCore, func(ctx context.Context, a *ActionContext, act Action) (map[string]string, error) {
		name, ok := act.Params["job"]
		if !ok || len(act.Params) != 1 {
			return nil, errors.New("approved_job requires only the pinned job label")
		}
		pin, ok := run.runPolicy.Jobs[name]
		if !ok {
			return nil, errors.New("job label is not approved")
		}
		h, err := a.Safety.Start(JobSpec{Node: pin.Node, Executable: pin.Executable, SHA256: pin.SHA256, Argv: pin.Argv})
		if err != nil {
			return nil, err
		}
		exit, err := a.Safety.Wait(ctx, h)
		if err == nil && (exit.Code != 0 || exit.Signal != 0) {
			err = fmt.Errorf("approved job exited code=%d signal=%d", exit.Code, exit.Signal)
		}
		return nil, err
	})
	if errs := ValidateAgainstRegistry(run.scenario, registry); len(errs) > 0 {
		return nil, bundle.Dir, errors.Join(errs...)
	}
	result := run.Execute(ctx, NewEngine(registry, log), &ActionContext{Bundle: bundle})
	run.mu.Lock()
	readbacks := make(map[string]any, len(run.readbacks))
	for node, receipt := range run.readbacks {
		readbacks[node] = receipt
	}
	run.mu.Unlock()
	evidence := struct {
		Request   ClaimedRequest
		GO        ApprovedGO
		Profile   FilePin
		GOFile    FilePin
		Readbacks map[string]any
		Faults    map[string]FaultReceipt
		Lease     LeaseObservation
		Jobs      map[string]ApprovedJobReceipt
	}{run.request, run.approval, policy.file, policy.bootstrap.Approval, readbacks, run.scope.FaultReceipts(), run.lease, run.jobReceipts()}
	raw, err := json.MarshalIndent(evidence, "", "  ")
	if err == nil {
		err = os.WriteFile(filepath.Join(bundle.ArtifactsDir(), "authority.json"), append(raw, '\n'), 0600)
	}
	if err != nil {
		result.Status = StatusFail
		result.Error += "; authority evidence: " + err.Error()
	}
	if finish := bundle.Finalize(result); finish != nil {
		err = errors.Join(err, finish)
	}
	return result, bundle.Dir, err
}

func protectedResults(dir string, owner uint32) error {
	// Check every existing ancestor before creating a new service-owned leaf.
	ancestor := dir
	for {
		_, err := os.Lstat(ancestor)
		if err == nil {
			break
		}
		if !os.IsNotExist(err) {
			return err
		}
		next := filepath.Dir(ancestor)
		if next == ancestor {
			return errors.New("no result ancestor")
		}
		ancestor = next
	}
	for p := ancestor; ; p = filepath.Dir(p) {
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		uid, err := recordUID(info)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0022 != 0 || (uid != 0 && uid != owner) {
			return errors.New("results ancestor is not protected service storage")
		}
		if filepath.Dir(p) == p {
			break
		}
	}
	return os.MkdirAll(dir, 0700)
}
