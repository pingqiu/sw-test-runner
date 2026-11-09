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
	if err := checkPinned(tr.PackInputs{Files: s.in.Files}, "m01", smoke, mountSoakHelpers, false); err != nil {
		return err
	}
	args, err := MountSoakArgs(s.d, smoke)
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
	_, loadErr := s.step(ctx, "mount-soak", mountSoakWorkerBudget)
	e, err := s.step(ctx, "mount-assess", mountSoakAssessBudget)
	if err != nil {
		return "", errors.Join(loadErr, err)
	}
	raw, _, err := e.Output.Decode()
	if err != nil {
		return "", errors.Join(loadErr, err)
	}
	v, err := decodeMountVerdict(raw)
	if err != nil {
		return "", errors.Join(loadErr, err)
	}
	rows := filepath.Join(s.c.Output, "mount-verdict.json")
	if err = os.WriteFile(rows, raw, 0600); err != nil {
		return "", errors.Join(loadErr, err)
	}
	if v.Status != "PASS" {
		return rows, errors.Join(loadErr, fmt.Errorf("mount soak verdict %s (cleanup %s)", v.Status, v.Cleanup))
	}
	return rows, loadErr
}
