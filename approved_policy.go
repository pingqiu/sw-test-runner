package testrunner

import (
	"errors"
	"fmt"
	"github.com/pingqiu/sw-test-runner/infra"
	"maps"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"
)

var authorityID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

func validateApprovedPolicy(p ApprovedRunPolicy, b RunBinding, grants []CleanupGrant) error {
	if !authorityID.MatchString(b.RequestID) || !authorityID.MatchString(b.RunID) || strings.ContainsRune(b.OutputDir, 0) {
		return errors.New("invalid scope identity/root")
	}
	byNode := map[string]infra.DeploymentPolicy{}
	for _, d := range p.Deployments {
		if !authorityID.MatchString(d.NodeID) {
			return errors.New("invalid deployment node ID")
		}
		if _, ok := byNode[d.NodeID]; ok {
			return errors.New("duplicate deployment node")
		}
		if err := infra.ValidateDeploymentPolicy(d); err != nil {
			return err
		}
		if b.OutputDir == d.InstallRoot || strings.HasPrefix(b.OutputDir, d.InstallRoot+"/") {
			return errors.New("work output overlaps protected installation")
		}
		runner := false
		for _, f := range d.Files {
			if f.Path == p.Runner && slices.Contains(f.Roles, "runner") && f.Size > 0 {
				runner = true
			}
		}
		if !runner {
			return errors.New("custodian argv does not name the read-back runner")
		}
		byNode[d.NodeID] = d
	}
	job := func(j JobPin) error {
		d, ok := byNode[j.Node]
		if !ok || !path.IsAbs(j.Executable) || path.Clean(j.Executable) != j.Executable || !validSHA256(j.SHA256) || len(j.Argv) > 4096 {
			return errors.New("unbound executable/argv")
		}
		for _, arg := range append([]string{j.Executable}, j.Argv...) {
			if strings.ContainsRune(arg, 0) {
				return errors.New("NUL in approved argv")
			}
			if insecureSSHOption.MatchString(arg) || nestedSSH.MatchString(arg) {
				return errors.New("S0b refuses nested SSH/insecure option in pinned argv")
			}
		}
		for _, f := range d.Files {
			if f.Path == j.Executable && f.SHA256 == j.SHA256 {
				if slices.Contains(f.Roles, "interpreter") {
					// v1 supports a pinned script path, optionally Python isolated mode.
					// Modules/eval/stdin need a reviewed pack, not a path-only exemption.
					args := j.Argv
					if len(args) > 0 && args[0] == "-I" {
						args = args[1:]
					}
					if len(args) == 0 {
						return errors.New("interpreter body is not pinned")
					}
					for _, body := range d.Files {
						if body.Path == args[0] && (slices.Contains(body.Roles, "adapter") || slices.Contains(body.Roles, "config")) {
							for _, source := range p.Scripts {
								if source.SHA256 == body.SHA256 {
									return nil
								}
							}
						}
					}
					return errors.New("interpreter requires a protected, read-back and scanned body")
				}
				for _, role := range f.Roles {
					if role == "runner" || role == "adapter" || role == "product" {
						return nil
					}
				}
			}
		}
		return errors.New("job executable is outside the qualified executable set")
	}
	if len(p.Jobs) > 1024 || len(grants) > 128 {
		return errors.New("policy exceeds bounded entries")
	}
	for key, j := range p.Jobs {
		if !authorityID.MatchString(key) {
			return errors.New("invalid job label")
		}
		if err := job(j); err != nil {
			return fmt.Errorf("job %s: %w", key, err)
		}
	}
	if len(p.GenerationRestarts) > 128 {
		return errors.New("too many generation restarts")
	}
	victims, replacements := map[string]bool{}, map[string]bool{}
	for name, g := range p.GenerationRestarts {
		old, ok := p.Jobs[g.Victim]
		next, nok := p.Jobs[g.Replacement]
		if !authorityID.MatchString(name) || !ok || !nok || g.Victim == g.Replacement || victims[g.Victim] || replacements[g.Replacement] || old.Node != next.Node || old.Executable != next.Executable || old.SHA256 != next.SHA256 {
			return errors.New("invalid exact generation restart plan")
		}
		victims[g.Victim] = true
		replacements[g.Replacement] = true
	}
	seen := map[string]bool{}
	for _, g := range grants {
		key := g.Node + "/" + g.Resource
		if seen[key] || g.Resource == "" || strings.ContainsRune(g.Resource, 0) || (g.Kind != "partition" && g.Kind != "netem" && g.Kind != "fill_disk") || g.Apply.Node != g.Node || g.Restore.Node != g.Node || len(g.Before) == 0 || len(g.Before) != len(g.During) || maps.Equal(g.Before, g.During) || !g.Deadline.After(time.Now()) || g.Deadline.After(b.HardDeadline) {
			return errors.New("incomplete exact cleanup grant")
		}
		seen[key] = true
		for k := range g.Before {
			if _, ok := g.During[k]; !ok {
				return errors.New("cleanup grant state schemas differ")
			}
		}
		if err := job(g.Apply); err != nil {
			return err
		}
		if err := job(g.Restore); err != nil {
			return err
		}
	}
	return nil
}
