// Package smoke is a small, single-VM Azure lifecycle controller. The shared
// pack retains its own normal S0b admission; Terraform is not an S0b job.
package smoke

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/inventory"
	"github.com/pingqiu/sw-test-runner/packs/seaweed"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Source          string  `json:"source"`
	ModuleSHA256    string  `json:"module_sha256"`
	Variables       string  `json:"variables"`
	VariablesSHA256 string  `json:"variables_sha256"`
	Output          string  `json:"output"`
	Request         string  `json:"request"`
	Policy          string  `json:"policy"`
	EstimatedUSD    float64 `json:"estimated_usd"`
}
type Result struct {
	Schema            string         `json:"schema"`
	Code              string         `json:"code"`
	Remedy            string         `json:"remedy"`
	Effects           []string       `json:"effects"`
	Remaining         []string       `json:"remaining_resource_ids"`
	Bundle            string         `json:"bundle,omitempty"`
	Error             string         `json:"error,omitempty"`
	RemainingObserved bool           `json:"remaining_observed"`
	CreateUnresolved  bool           `json:"create_unresolved"`
	Plan              map[string]any `json:"plan,omitempty"`
}
type Runner func(context.Context, string, string, ...string) ([]byte, error)
type Pack func(context.Context, string, string, []byte) (string, error)
type Controller struct {
	Command Runner
	Pack    Pack
}

func StrictJSON(raw []byte, dst any) error {
	// Refuse duplicate/case aliases before Go's case-insensitive struct decoder.
	scan := json.NewDecoder(bytes.NewReader(raw))
	var walk func() error
	walk = func() error {
		t, e := scan.Token()
		if e != nil {
			return e
		}
		if t == json.Delim('{') {
			seen := map[string]bool{}
			for scan.More() {
				k, e := scan.Token()
				if e != nil {
					return e
				}
				s := k.(string)
				if s != strings.ToLower(s) || seen[s] {
					return errors.New("duplicate/noncanonical JSON key")
				}
				seen[s] = true
				if e = walk(); e != nil {
					return e
				}
			}
			_, e = scan.Token()
		} else if t == json.Delim('[') {
			for scan.More() {
				if e = walk(); e != nil {
					return e
				}
			}
			_, e = scan.Token()
		}
		return e
	}
	if err := walk(); err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}
func digest(b []byte) string { return fmt.Sprintf("%x", sha256.Sum256(b)) }

// Sources returns the exact module, provider lock and prerequisite bytes copied
// into the private run root. State, .terraform, credentials and tests are excluded.
func Sources(root string) (map[string][]byte, string, error) {
	files := map[string][]byte{}
	for _, dir := range []string{"azure", "azure-validation"} {
		base := filepath.Join(root, "provision", "terraform", dir)
		entries, err := os.ReadDir(base)
		if err != nil {
			return nil, "", err
		}
		for _, e := range entries {
			if e.IsDir() || !(strings.HasSuffix(e.Name(), ".tf") || e.Name() == ".terraform.lock.hcl" || e.Name() == "cloud-init.yaml") {
				continue
			}
			p := filepath.Join(base, e.Name())
			info, err := os.Lstat(p)
			if err != nil || !info.Mode().IsRegular() {
				return nil, "", fmt.Errorf("nonregular source: %s", p)
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return nil, "", err
			}
			files[filepath.ToSlash(filepath.Join(dir, e.Name()))] = b
		}
	}
	for _, p := range []string{"azure/versions.tf", "azure/cloud-init.yaml", "azure/.terraform.lock.hcl", "azure-validation/main.tf", "azure-validation/.terraform.lock.hcl"} {
		if len(files[p]) == 0 {
			return nil, "", fmt.Errorf("missing source %s", p)
		}
	}
	keys := []string{}
	for k := range files {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		fmt.Fprintf(h, "%s\x00%s\n", k, digest(files[k]))
	}
	return files, fmt.Sprintf("%x", h.Sum(nil)), nil
}

type declaration struct {
	Subscription string `json:"subscription_id"`
	Group        string `json:"resource_group_name"`
	Location     string `json:"location"`
	Size         string `json:"vm_size"`
	Run          struct {
		Work    time.Time `json:"work_deadline"`
		Hard    time.Time `json:"hard_deadline"`
		Reserve int       `json:"cloud_cleanup_reserve_seconds"`
		Budget  float64   `json:"budget_usd"`
		Abort   float64   `json:"abort_estimate_usd"`
	} `json:"run_declaration"`
}

func inputs(c Config) (map[string][]byte, []byte, declaration, error) {
	files, hash, err := Sources(c.Source)
	var d declaration
	if err != nil || hash != c.ModuleSHA256 {
		return nil, nil, d, errors.New("module/lock pin mismatch")
	}
	b, err := os.ReadFile(c.Variables)
	if err != nil || digest(b) != c.VariablesSHA256 {
		return nil, nil, d, errors.New("variable pin mismatch")
	}
	if err = json.Unmarshal(b, &d); err != nil {
		return nil, nil, d, err
	}
	uuid := regexp.MustCompile(`^[a-fA-F0-9]{8}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{12}$`)
	if !uuid.MatchString(d.Subscription) || !regexp.MustCompile(`^[A-Za-z0-9_-]{1,90}$`).MatchString(d.Group) || d.Location != "westus2" || d.Size != "Standard_D2s_v5" || d.Run.Budget <= 0 || d.Run.Budget > 3 || d.Run.Abort <= 0 || d.Run.Abort > d.Run.Budget || c.EstimatedUSD <= 0 || c.EstimatedUSD > d.Run.Abort || d.Run.Reserve < 600 || d.Run.Reserve > 1800 || d.Run.Work.IsZero() || !d.Run.Hard.After(d.Run.Work.Add(time.Duration(d.Run.Reserve-1)*time.Second)) || d.Run.Hard.Second() != 0 || d.Run.Hard.Nanosecond() != 0 {
		return nil, nil, d, errors.New("require one westus2 D2s_v5, explicit subscription/RG, <=$3 and minute-aligned cleanup envelope")
	}
	for _, p := range []string{c.Source, c.Variables, c.Output, c.Request, c.Policy} {
		if !filepath.IsAbs(p) {
			return nil, nil, d, errors.New("source/variables/output/request/policy must be absolute controller paths")
		}
	}
	return files, b, d, nil
}
func result(code, remedy string) Result {
	return Result{Schema: "seaweed.cloud-smoke/v1", Code: code, Remedy: remedy, Effects: []string{}, Remaining: []string{}}
}

// These are private evidence files, not a new operation resolver. A failed or
// interrupted apply has no success receipt; empty state/list cannot settle it.
func applyUnresolved(root string) bool {
	started, e := os.ReadFile(filepath.Join(root, "apply-started"))
	if e != nil || string(started) != "apply may have issued creates\n" {
		return true
	}
	finished, e := os.ReadFile(filepath.Join(root, "apply-succeeded"))
	return e != nil || string(finished) != "apply returned success\n"
}
func stamp(name, text string) error {
	f, e := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return e
	}
	_, e = f.WriteString(text)
	if e == nil {
		e = f.Sync()
	}
	return errors.Join(e, f.Close())
}

// Execute has no default mutation mode. plan reads local files only. run requires
// a fresh private output path; recovery destroy reuses that path and fixed state.
func (ctl Controller) Execute(ctx context.Context, mode string, c Config) (out Result) {
	out = result("INPUT_REFUSED", "correct pinned inputs; no cloud operation was authorized")
	if mode == "destroy" || mode == "verify" {
		// Recovery needs only the retained private snapshot/state, even if the
		// original checkout or input file was subsequently moved or changed.
		c.Source = c.Output
		c.Variables = filepath.Join(c.Output, "provision", "terraform", "azure-validation", "run.auto.tfvars.json")
	}
	files, vars, d, err := inputs(c)
	if err != nil {
		out.Error = err.Error()
		return
	}
	if mode != "plan" && mode != "run" && mode != "destroy" && mode != "verify" {
		out.Code = "USAGE"
		return
	}
	if mode == "plan" {
		out.Code = "PLAN_ONLY"
		out.Remedy = "review estimate, module/lock/vars pins and normal pack admission; obtain launch token before run"
		out.Plan = map[string]any{"module_sha256": c.ModuleSHA256, "variables_sha256": c.VariablesSHA256, "subscription": d.Subscription, "resource_group": d.Group, "vm_size": d.Size, "vm_count": 1, "region": d.Location, "hard_deadline": d.Run.Hard, "estimated_usd": c.EstimatedUSD, "cap_usd": d.Run.Budget, "steps": []string{"read-only empty RG", "terraform init -lockfile=readonly", "terraform plan -out=smoke.tfplan", "terraform apply smoke.tfplan", "shared pack normal S0b admission", "terraform destroy", "independent read-only RG list"}}
		return
	}
	if ctl.Command == nil {
		ctl.Command = Native
	}
	if ctl.Pack == nil {
		ctl.Pack = ApprovedPack
	}
	terraformRoot := filepath.Join(c.Output, "provision", "terraform")
	root := filepath.Join(terraformRoot, "azure-validation")
	seq := 0
	call := func(cx context.Context, tool string, args ...string) ([]byte, error) {
		seq++
		label := fmt.Sprintf("%03d-%s", seq, tool)
		intent, _ := json.Marshal(args)
		if e := os.WriteFile(filepath.Join(c.Output, label+"-intent.json"), intent, 0600); e != nil {
			return nil, e
		}
		b, e := ctl.Command(cx, root, tool, args...)
		if w := os.WriteFile(filepath.Join(c.Output, label+"-output"), b, 0600); w != nil {
			e = errors.Join(e, w)
		}
		status := []byte("OK")
		if e != nil {
			status = []byte(e.Error())
		}
		if w := os.WriteFile(filepath.Join(c.Output, label+"-status"), status, 0600); w != nil {
			e = errors.Join(e, w)
		}
		return b, e
	}
	list := func(cx context.Context) ([]string, error) {
		b, e := call(cx, "az", "resource", "list", "--subscription", d.Subscription, "--resource-group", d.Group, "--query", "[].id", "--output", "json", "--only-show-errors")
		var ids []string
		if e == nil {
			e = json.Unmarshal(b, &ids)
			if e == nil && ids == nil {
				e = errors.New("resource list must be a JSON array")
			}
		}
		if ids == nil {
			ids = []string{}
		}
		return ids, e
	}
	if mode == "run" {
		remaining := time.Until(d.Run.Hard)
		if remaining <= time.Duration(d.Run.Reserve)*time.Second || remaining > time.Hour || !time.Now().Before(d.Run.Work) {
			out.Code = "DEADLINE_REFUSED"
			return
		}
		if err = os.Mkdir(c.Output, 0700); err != nil {
			out.Error = err.Error()
			return
		}
		for name, b := range files {
			p := filepath.Join(terraformRoot, filepath.FromSlash(name))
			if err = os.MkdirAll(filepath.Dir(p), 0700); err == nil {
				err = os.WriteFile(p, b, 0600)
			}
			if err != nil {
				out.Error = err.Error()
				return
			}
		}
		if err = os.WriteFile(filepath.Join(root, "run.auto.tfvars.json"), vars, 0600); err != nil {
			out.Error = err.Error()
			return
		}
	} else {
		// Separate command journals preserve the original UNKNOWN evidence.
		seq = int(time.Now().UnixMilli())
	}
	if mode == "verify" {
		read, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		ids, e := list(read)
		out.Remaining = ids
		out.RemainingObserved = e == nil
		out.CreateUnresolved = applyUnresolved(c.Output)
		if e != nil || len(ids) > 0 {
			out.Code = "RESOURCES_REMAIN"
			out.Error = fmt.Sprint(e)
			out.Remedy = "retain IDs and run destroy; notify the run owner"
		} else if out.CreateUnresolved {
			out.Code = "CREATE_UNRESOLVED"
			out.Remedy = "empty list is only a snapshot; retain apply evidence and ask the operator to resolve the pending create with the provider"
		} else {
			out.Code = "EMPTY"
			out.Remedy = "RG is retained and contains zero resources"
		}
		return
	}
	cleanup := func() {
		// Cleanup survives work cancellation; it never extends the run's hard
		// deadline. Explicit later recovery destroy is a new operator action.
		end := d.Run.Hard
		if mode == "destroy" {
			end = time.Now().Add(15 * time.Minute)
		}
		cx, cancel := context.WithDeadline(context.Background(), end)
		defer cancel()
		out.Effects = append(out.Effects, "terraform destroy")
		_, de := call(cx, "terraform", "destroy", "-input=false", "-auto-approve", "-lock-timeout=0s", "-no-color")
		ids, ve := list(cx)
		out.Remaining = ids
		out.RemainingObserved = ve == nil
		out.CreateUnresolved = applyUnresolved(c.Output)
		if de != nil || ve != nil || len(ids) > 0 {
			out.Code = "CLEANUP_INCOMPLETE"
			out.Error = fmt.Sprint(errors.Join(de, ve))
			out.Remedy = "notify run owner with remaining IDs; rerun destroy using this private output/state"
			return
		}
		if out.CreateUnresolved {
			if mode == "destroy" || out.Code == "OK" {
				out.Code = "CREATE_UNRESOLVED"
			}
			out.Remedy = "destroy/empty list cannot settle an UNKNOWN create; retain evidence and request provider-operation resolution from the operator"
			return
		}
		if out.Code == "OK" || mode == "destroy" {
			out.Code = "COMPLETE"
			out.Remedy = "destroy succeeded and independent Azure list proves empty RG"
		}
	}
	if mode == "destroy" {
		cleanup()
		return
	}
	work, cancel := context.WithDeadline(ctx, d.Run.Work)
	defer cancel()
	out.Remedy = "inspect retained command/pack evidence; notify the run owner and recover with destroy if resources remain"
	ids, e := list(work)
	out.RemainingObserved = e == nil
	if e != nil || len(ids) > 0 {
		out.Code = "RG_NOT_EMPTY"
		out.Remaining = ids
		out.Error = fmt.Sprint(e)
		return
	}
	for _, args := range [][]string{{"init", "-backend=false", "-input=false", "-lockfile=readonly", "-no-color"}, {"plan", "-input=false", "-lock-timeout=0s", "-out=smoke.tfplan", "-no-color"}} {
		if _, e = call(work, "terraform", args...); e != nil {
			out.Code = "PLAN_FAILED"
			out.Error = e.Error()
			return
		}
	}
	// From this boundary, always destroy and independently list, including a
	// partial apply/UNKNOWN. No automatic apply retry or custom recovery engine.
	if e = stamp(filepath.Join(c.Output, "apply-started"), "apply may have issued creates\n"); e != nil {
		out.Code = "EVIDENCE_FAILED"
		out.Error = e.Error()
		return
	}
	out.Effects = append(out.Effects, "terraform apply")
	defer cleanup()
	if _, e = call(work, "terraform", "apply", "-input=false", "-lock-timeout=0s", "-no-color", "smoke.tfplan"); e != nil {
		out.Code = "APPLY_FAILED_OR_UNKNOWN"
		out.Error = e.Error()
		return
	}
	if e = stamp(filepath.Join(c.Output, "apply-succeeded"), "apply returned success\n"); e != nil {
		out.Code = "EVIDENCE_FAILED"
		out.Error = e.Error()
		return
	}
	b, e := call(work, "terraform", "output", "-raw", "inventory_json")
	if e != nil {
		out.Code = "INVENTORY_FAILED"
		out.Error = e.Error()
		return
	}
	if e = os.WriteFile(filepath.Join(c.Output, "requested-inventory.json"), b, 0600); e != nil {
		out.Code = "EVIDENCE_FAILED"
		out.Error = e.Error()
		return
	}
	out.Effects = append(out.Effects, "normal S0b pack")
	if out.Bundle, e = ctl.Pack(work, c.Request, c.Policy, b); e != nil {
		out.Code = "PACK_FAILED"
		out.Error = e.Error()
		return
	}
	out.Code = "OK"
	return
}

// Native runs fixed Terraform/Azure CLI argv on the Linux controller. Separate
// stderr is included in private evidence without corrupting JSON stdout.
func Native(ctx context.Context, dir, tool string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, tool, args...)
	cmd.Dir = dir
	for _, e := range os.Environ() {
		key := strings.SplitN(e, "=", 2)[0]
		if strings.HasPrefix(key, "TF_CLI_ARGS") || strings.HasPrefix(key, "TF_VAR_") || key == "TF_WORKSPACE" || key == "TF_DATA_DIR" || key == "TF_CLI_CONFIG_FILE" {
			continue
		}
		cmd.Env = append(cmd.Env, e)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.WaitDelay = 3 * time.Second
	b, e := cmd.Output()
	if e != nil {
		return append(b, stderr.Bytes()...), fmt.Errorf("%s: %w", tool, e)
	}
	return b, nil
}

func ApprovedPack(ctx context.Context, request, policy string, requested []byte) (string, error) {
	// These reads only join the provision output to the normal admission input.
	// RunApprovedRequest subsequently performs all trusted reads/admission itself.
	b, e := os.ReadFile(request)
	if e != nil {
		return "", e
	}
	var r tr.ClaimedRequest
	if e = StrictJSON(b, &r); e != nil {
		return "", e
	}
	b, e = os.ReadFile(r.Binding.Scenario.Path)
	if e != nil || digest(b) != r.Binding.Scenario.SHA256 {
		return "", errors.New("scenario pin mismatch")
	}
	var s tr.Scenario
	if e = yaml.Unmarshal(b, &s); e != nil {
		return "", e
	}
	var p seaweed.Plan
	if e = StrictJSON([]byte(s.Metadata["seaweed_plan"]), &p); e != nil {
		return "", e
	}
	d, e := inventory.Join(requested, []byte(p.Resolved))
	if e != nil || p.Requested != string(requested) || d.Target != "azure" || d.Transport != "http" || len(d.Hosts) != 1 {
		return "", errors.New("pack does not bind the exact one-VM HTTP export")
	}
	res, bundle, e := tr.RunApprovedRequest(ctx, request, policy, seaweed.RegisterPack, nil, nil)
	if e == nil && (res == nil || res.Status != tr.StatusPass) {
		e = errors.New("pack returned FAIL")
	}
	return bundle, e
}
