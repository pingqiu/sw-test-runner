// Package smoke is a small, single-VM Azure lifecycle controller. The shared
// cloud HTTP entry trusts the just-created VM; on-prem S0b is unchanged.
package smoke

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"encoding/pem"
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

	"github.com/pingqiu/sw-test-runner/packs/seaweed"
	"golang.org/x/crypto/ssh"
)

type Config struct {
	Source          string              `json:"source"`
	ModuleSHA256    string              `json:"module_sha256"`
	Variables       string              `json:"variables"`
	VariablesSHA256 string              `json:"variables_sha256"`
	Output          string              `json:"output"`
	Cloud           seaweed.CloudConfig `json:"cloud"`
	EstimatedUSD    float64             `json:"estimated_usd"`
	PriorUSD        float64             `json:"prior_estimated_usd"`
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
	RemainingSummary  string         `json:"billable_resources"`
	EstimatedUSD      float64        `json:"estimated_usd"`
	CumulativeUSD     float64        `json:"cumulative_estimated_usd"`
	ActualCostUSD     *float64       `json:"actual_cost_usd"`
	Plan              map[string]any `json:"plan,omitempty"`
}
type Runner func(context.Context, string, string, ...string) ([]byte, error)
type Pack func(context.Context, []byte, seaweed.CloudConfig) (string, error)
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
	Cluster      bool   `json:"cluster"`
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
	var vars map[string]json.RawMessage
	if err = json.Unmarshal(b, &vars); err != nil {
		return nil, nil, d, err
	}
	for _, key := range []string{"host_keys", "node_host_keys", "run_id"} {
		if _, present := vars[key]; present {
			return nil, nil, d, fmt.Errorf("%s is reserved for controller-generated host keys", key)
		}
	}
	uuid := regexp.MustCompile(`^[a-fA-F0-9]{8}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{12}$`)
	if c.PriorUSD < 0 || c.PriorUSD+c.EstimatedUSD > 10 || !uuid.MatchString(d.Subscription) || !regexp.MustCompile(`^[A-Za-z0-9_-]{1,90}$`).MatchString(d.Group) || d.Location != "westus2" || d.Size != "Standard_D2s_v5" || d.Run.Budget <= 0 || d.Run.Budget > 3 || d.Run.Abort <= 0 || d.Run.Abort > d.Run.Budget || c.EstimatedUSD <= 0 || c.EstimatedUSD > d.Run.Abort || d.Run.Reserve < 600 || d.Run.Reserve > 1800 || d.Run.Work.IsZero() || !d.Run.Hard.After(d.Run.Work.Add(time.Duration(d.Run.Reserve-1)*time.Second)) || d.Run.Hard.Second() != 0 || d.Run.Hard.Nanosecond() != 0 {
		return nil, nil, d, errors.New("require one westus2 D2s_v5, explicit subscription/RG, <=$3 and minute-aligned cleanup envelope")
	}
	for _, p := range []string{c.Source, c.Variables, c.Output, c.Cloud.SSHKey} {
		if !filepath.IsAbs(p) {
			return nil, nil, d, errors.New("source/variables/output/SSH key must be absolute controller paths")
		}
	}
	return files, b, d, nil
}

// raw is a syntactically valid RawMessage from the resource-array decoder.
func resourceDecisionKeys(raw []byte, tags bool) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	if first, _ := d.Token(); first != json.Delim('{') {
		return errors.New("resource/tags must be an object")
	}
	keys := []string{"id", "type", "tags"}
	if tags {
		keys = []string{"seaweed_run"}
	}
	seen := map[string]bool{}
	for d.More() {
		token, _ := d.Token()
		key := token.(string)
		var value json.RawMessage
		if e := d.Decode(&value); e != nil {
			return e
		}
		for _, canonical := range keys {
			if strings.EqualFold(key, canonical) {
				if key != canonical || seen[canonical] {
					return errors.New("duplicate/case-aliased resource decision key")
				}
				seen[canonical] = true
			}
		}
		if tags && (len(value) == 0 || value[0] != '"') {
			return errors.New("individual tag value must be a string")
		}
		if !tags && key == "tags" && string(value) != "null" {
			if e := resourceDecisionKeys(value, true); e != nil {
				return e
			}
		}
	}
	return nil
}
func resourceIDs(raw []byte, run string, all bool) ([]string, error) {
	var rows []json.RawMessage
	if e := json.Unmarshal(raw, &rows); e != nil || rows == nil {
		return nil, errors.New("resource list must be an object array")
	}
	ids := []string{}
	for _, rawRow := range rows {
		if e := resourceDecisionKeys(rawRow, false); e != nil {
			return nil, e
		}
		var r struct {
			ID, Type string
			Tags     map[string]string
		}
		if e := json.Unmarshal(rawRow, &r); e != nil {
			return nil, e
		}
		if r.ID == "" || r.Type == "" {
			return nil, errors.New("resource missing id/type")
		}
		typ := strings.ToLower(r.Type)
		if all || r.Tags["seaweed_run"] == run || typ == "microsoft.compute/virtualmachines" || typ == "microsoft.compute/disks" || typ == "microsoft.network/publicipaddresses" {
			ids = append(ids, r.ID)
		}
	}
	return ids, nil
}
func result(code, remedy string) Result {
	return Result{Schema: "seaweed.cloud-smoke/v1", Code: code, Remedy: remedy, Effects: []string{}, Remaining: []string{}, RemainingSummary: "billable resources remaining: UNKNOWN (not checked)"}
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

// Execute has no default mutation mode. plan probes local parent writability. run requires
// a fresh private output path; recovery destroy reuses that path and fixed state.
func (ctl Controller) Execute(ctx context.Context, mode string, c Config) (out Result) {
	out = result("INPUT_REFUSED", "correct pinned inputs; no cloud operation was authorized")
	out.EstimatedUSD, out.CumulativeUSD = c.EstimatedUSD, c.PriorUSD+c.EstimatedUSD
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
	if ctl.Pack == nil && (mode == "run" || mode == "plan") {
		if err = c.Cloud.CheckFiles(); err != nil {
			out.Error = err.Error()
			return
		}
	}
	if mode == "run" || mode == "plan" {
		probe, e := os.MkdirTemp(filepath.Dir(c.Output), ".seaweed-cloud-write-probe-")
		if e == nil {
			e = os.Remove(probe)
		}
		if e != nil {
			out.Code, out.Error = "OUTPUT_PARENT_REFUSED", e.Error()
			out.Remedy = "create only the intended output parent, owned and writable by the controller user (0700); rerun local preflight before cloud admission"
			return
		}
	}
	if mode == "plan" {
		out.Code = "PLAN_ONLY"
		out.Remedy = "review estimate, module/lock/vars and artifacts; obtain launch token before run"
		count := 1
		if d.Cluster {
			count = 10
		}
		out.Plan = map[string]any{"module_sha256": c.ModuleSHA256, "variables_sha256": c.VariablesSHA256, "subscription": d.Subscription, "resource_group": d.Group, "vm_size": d.Size, "vm_count": count, "region": d.Location, "hard_deadline": d.Run.Hard, "estimated_usd": c.EstimatedUSD, "cap_usd": d.Run.Budget, "steps": []string{"generate VM host key locally", "read-only empty RG", "terraform init -lockfile=readonly", "terraform plan -out=smoke.tfplan", "terraform apply smoke.tfplan", "shared pack direct HTTP phases", "terraform destroy", "independent read-only RG list"}}
		return
	}
	if ctl.Command == nil {
		ctl.Command = Native
	}
	if ctl.Pack == nil {
		ctl.Pack = seaweed.RunCloudHTTP
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
			return nil, errors.Join(e, w)
		}
		status := []byte("OK")
		if e != nil {
			status = []byte(e.Error())
		}
		if w := os.WriteFile(filepath.Join(c.Output, label+"-status"), status, 0600); w != nil {
			return nil, errors.Join(e, w)
		}
		return b, e
	}
	once := call
	call = func(cx context.Context, tool string, args ...string) ([]byte, error) {
		eligible := tool == "terraform" && (args[0] == "apply" || args[0] == "destroy") || tool == "az" && args[0] == "resource" && args[1] == "list"
		for attempt := 0; ; attempt++ {
			if e := cx.Err(); e != nil {
				return nil, e
			}
			b, e := once(cx, tool, args...)
			transport := regexp.MustCompile(`(?i)ECONNRESET|ConnectionResetError|connection reset by peer|HTTP response was nil|EOF before response|(?:Put|Get|Delete) "https://[^"\n]+": EOF\b|TLS handshake[^\n]*reset`).Match(b)
			rejected := regexp.MustCompile(`(?i)authorization|authentication|quota|validation|StatusCode[=: ]+4[0-9][0-9]|status[^\n0-9]*4[0-9][0-9]|HTTP[/ 0-9.]*4[0-9][0-9]`).Match(b)
			if e == nil || !eligible || attempt == 2 || !transport || rejected {
				return b, e
			}
			timer := time.NewTimer([]time.Duration{10 * time.Second, 30 * time.Second}[attempt])
			select {
			case <-cx.Done():
				timer.Stop()
				return b, cx.Err()
			case <-timer.C:
			}
		}
	}
	list := func(cx context.Context) ([]string, error) {
		b, e := call(cx, "az", "resource", "list", "--subscription", d.Subscription, "--resource-group", d.Group, "--output", "json", "--only-show-errors")
		ids := []string{}
		if e == nil {
			ids, e = resourceIDs(b, filepath.Base(c.Output), true)
		}
		if d.Cluster {
			b, se := call(cx, "az", "resource", "list", "--subscription", d.Subscription, "--output", "json", "--only-show-errors")
			var subscription []string
			if se == nil {
				subscription, se = resourceIDs(b, filepath.Base(c.Output), false)
			}
			ids, e = append(ids, subscription...), errors.Join(e, se)
		}
		out.RemainingSummary = "billable resources remaining: UNKNOWN (list failed)"
		if e == nil {
			out.RemainingSummary = fmt.Sprintf("billable resources remaining: %v (checked %s)", ids, time.Now().UTC().Format("15:04Z"))
			if len(ids) == 0 {
				out.RemainingSummary = "billable resources remaining: 0 (checked " + time.Now().UTC().Format("15:04Z") + ")"
			}
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
		if d.Cluster {
			if !regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,49}$`).MatchString(filepath.Base(c.Output)) {
				out.Error = "cluster output basename must be a safe unique run ID"
				return
			}
			b, _ := json.Marshal(c)
			if err = os.WriteFile(filepath.Join(c.Output, "recovery.json"), b, 0600); err != nil {
				out.Error = err.Error()
				return
			}
		}
		c.Cloud.Output = c.Output
		if d.Cluster {
			c.Cloud.HostKeys, err = clusterHostKeys(root)
		} else {
			c.Cloud.HostKey, err = hostKey(root)
		}
		if err != nil {
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
		_, de := call(cx, "terraform", "destroy", "-parallelism=3", "-input=false", "-auto-approve", "-lock-timeout=0s", "-no-color")
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
	// partial apply/UNKNOWN. Only transport failures retry the same plan/state.
	if e = stamp(filepath.Join(c.Output, "apply-started"), "apply may have issued creates\n"); e != nil {
		out.Code = "EVIDENCE_FAILED"
		out.Error = e.Error()
		return
	}
	out.Effects = append(out.Effects, "terraform apply")
	defer cleanup()
	if _, e = call(work, "terraform", "apply", "-parallelism=3", "-input=false", "-lock-timeout=0s", "-no-color", "smoke.tfplan"); e != nil {
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
	if d.Cluster {
		disks, de := call(work, "terraform", "output", "-json", "os_disk_ids")
		var ids []string
		if de == nil {
			de = json.Unmarshal(disks, &ids)
			if de == nil && len(ids) != 10 {
				de = errors.New("require ten explicit OS disk IDs")
			}
		}
		for _, id := range ids {
			if de != nil {
				break
			}
			if !strings.HasPrefix(strings.ToLower(id), strings.ToLower("/subscriptions/"+d.Subscription+"/resourceGroups/"+d.Group+"/providers/Microsoft.Compute/disks/")) {
				de = errors.New("OS disk outside test RG")
				break
			}
			_, de = call(work, "az", "tag", "update", "--resource-id", id, "--operation", "Merge", "--tags", "seaweed_run="+filepath.Base(c.Output), "expires_at="+d.Run.Hard.Format(time.RFC3339), "--only-show-errors")
		}
		if de != nil {
			out.Code, out.Error = "DISK_TAG_FAILED", de.Error()
			return
		}
	}
	if e = os.WriteFile(filepath.Join(c.Output, "requested-inventory.json"), b, 0600); e != nil {
		out.Code = "EVIDENCE_FAILED"
		out.Error = e.Error()
		return
	}
	out.Effects = append(out.Effects, "direct cloud HTTP pack")
	if out.Bundle, e = ctl.Pack(work, b, c.Cloud); e != nil {
		out.Code = "PACK_FAILED"
		out.Error = e.Error()
		return
	}
	out.Code = "OK"
	return
}

func clusterHostKeys(root string) (map[string]string, error) {
	public, keys := map[string]string{}, map[string]map[string]string{}
	for _, id := range []string{"meta", "vol1", "vol2", "vol3", "vol4", "vol5", "vol6", "client1", "client2", "client3"} {
		p, e := hostKey(root)
		if e != nil {
			return nil, e
		}
		public[id] = p
		b, e := os.ReadFile(filepath.Join(root, "host.auto.tfvars.json"))
		if e != nil {
			return nil, e
		}
		var v struct {
			Key map[string]string `json:"host_keys"`
		}
		if e = json.Unmarshal(b, &v); e != nil {
			return nil, e
		}
		keys[id] = v.Key
	}
	b, e := json.Marshal(map[string]any{"node_host_keys": keys, "run_id": filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(root))))})
	if e != nil {
		return nil, e
	}
	return public, os.WriteFile(filepath.Join(root, "host.auto.tfvars.json"), b, 0600)
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

// Only the VM's server key enters private cloud-init/state. The controller's
// client authentication key never leaves the controller.
func hostKey(root string) (string, error) {
	_, private, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		return "", e
	}
	signer, e := ssh.NewSignerFromKey(private)
	if e != nil {
		return "", e
	}
	block, e := ssh.MarshalPrivateKey(private, "single-cloud-smoke")
	if e != nil {
		return "", e
	}
	public := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
	b, e := json.Marshal(map[string]any{"host_keys": map[string]string{"ed25519_private": string(pem.EncodeToMemory(block)), "ed25519_public": public}})
	if e != nil {
		return "", e
	}
	return public, os.WriteFile(filepath.Join(root, "host.auto.tfvars.json"), b, 0600)
}
