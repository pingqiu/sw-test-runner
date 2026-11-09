//go:build linux

package seaweed

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	tr "github.com/pingqiu/sw-test-runner"
)

// mountJobs adds the two M01 jobs of the mount soak, after proving the helpers
// on M01 are the embedded sources.
func (s *directSoak) mountJobs() error {
	smoke := s.b.Plan.Smoke
	if err := checkMountHelpers(s.in.Files["m01"], smoke); err != nil {
		return err
	}
	args, err := MountSoakArgs(s.d, smoke, s.c.ReadCase)
	if err != nil {
		return err
	}
	python := s.in.Files["m01"]["/usr/bin/python3.12"]
	s.b.Jobs["mount-soak"] = tr.JobPin{Node: "m01", Executable: "/usr/bin/python3.12", SHA256: python, Argv: args}
	s.b.Jobs["mount-assess"] = tr.JobPin{Node: "m01", Executable: "/usr/bin/python3.12", SHA256: python, Argv: MountAssessArgs(s.d, smoke)}
	return nil
}

// mountCase runs the load, then the assessor even when the load failed: a
// missing or partial journal is the assessor's INVALID, not a product verdict.
func (s *directSoak) mountCase(ctx context.Context) (string, error) {
	return runMountCase(ctx, s.step, s.c.Output, s.c.ReadCase)
}

// runMountCase publishes whatever verdict the assessor completed; a non-PASS
// verdict is a result for the caller, not a reason to lose the file.
func runMountCase(ctx context.Context, step func(context.Context, string, int) (tr.ExitInfo, error), output, name string) (string, error) {
	_, loadErr := step(ctx, "mount-soak", scheduleFor(name).WorkerS)
	e, stepErr := step(ctx, "mount-assess", mountSoakAssessBudget)
	raw, _, err := e.Output.Decode()
	if err != nil {
		return "", errors.Join(loadErr, stepErr, err)
	}
	v, err := decodeMountVerdict(raw)
	if err != nil {
		return "", errors.Join(loadErr, stepErr, err)
	}
	loadErr = errors.Join(loadErr, stepErr)
	rows := filepath.Join(output, "mount-verdict.json")
	if err = os.WriteFile(rows, raw, 0600); err != nil {
		return "", errors.Join(loadErr, err)
	}
	if name == MountSoakSmokeCase {
		// The smoke answers only whether the wiring ran and cleaned up.
		if v.Cleanup != "PROVEN" {
			return rows, errors.Join(loadErr, fmt.Errorf("mount soak smoke: cleanup %s (status %s)", v.Cleanup, v.Status))
		}
		return rows, loadErr
	}
	if v.Status != "PASS" {
		return rows, errors.Join(loadErr, fmt.Errorf("mount soak verdict %s (cleanup %s)", v.Status, v.Cleanup))
	}
	return rows, loadErr
}
