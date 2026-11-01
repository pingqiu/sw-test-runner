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
	"slices"
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
	if _, e := os.Stat(filepath.Join(root, "apply-transport-unknown")); !os.IsNotExist(e) {
		return true
	}
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

func transportOnly(raw []byte) bool {
	endpoint := `https://management\.azure\.com/[A-Za-z0-9/_?=.%:-]+`
	transport := `(?:ECONNRESET|ConnectionResetError(?: 104|\(104, 'Connection reset by peer'\))?|connection reset by peer|HTTP response was nil(?:; connection may have been reset)?|EOF before response|(?:Put|Get) "` + endpoint + `": EOF|TLS handshake: connection reset(?: by peer)?|\('Connection aborted\.', ConnectionResetError\(104, 'Connection reset by peer'\)\))`
	// Create/PUT forms only; every Delete goes through deleteResets' re-read rule.
	publicIP := `updating Public I P Address \(Subscription: "[a-f0-9-]{36}"\nResource Group Name: "[A-Za-z0-9_-]+"\nPublic I P Addresses Name: "[A-Za-z0-9_-]+"\): performing CreateOrUpdate: Put "` + endpoint + `": HTTP response was nil; connection may have been reset`
	metadata := `(?:\n\n  with module\.run\.azurerm_public_ip\.node\["[a-z0-9-]+"\],\n  on \.\./azure/main\.tf line [0-9]+, in resource "azurerm_public_ip" "node":\n  [0-9]+: resource "azurerm_public_ip" "node" \{)?`
	nicEndpoint := `https://management\.azure\.com/subscriptions/[a-f0-9-]{36}/resourceGroups/[A-Za-z0-9_-]+/providers/Microsoft\.Network/networkInterfaces/[A-Za-z0-9_-]+\?api-version=2025-07-01`
	nic := `creating Network Interface \(Subscription: "[a-f0-9-]{36}"\nResource Group Name: "[A-Za-z0-9_-]+"\nNetwork Interface Name: "[A-Za-z0-9_-]+"\): performing CreateOrUpdate: Put "` + nicEndpoint + `": HTTP response was nil; connection may have been reset`
	nicMetadata := `\n\n  with module\.run\.azurerm_network_interface\.node\["[a-z0-9-]+"\],\n  on \.\./azure/main\.tf line [0-9]+, in resource "azurerm_network_interface" "node":\n  [0-9]+: resource "azurerm_network_interface" "node" \{`
	message := `(?:(?:` + transport + `|` + publicIP + `)` + metadata + `|` + nic + nicMetadata + `)`
	whole := `\A(?:` + message + `|Error: ` + message + `(?:\s+Error: ` + message + `)*)\z`
	return regexp.MustCompile(whole).MatchString(strings.TrimSpace(strings.ReplaceAll(string(raw), "\r\n", "\n")))
}

// A lost create PUT may have completed in Azure outside Terraform state
// (runs 3 and 7b), so a plain retry fails "already exists". Each create-form
// block's exact ID is re-read: absent leaves the retry as before; present is
// imported at the block's source address only if it is in this subscription/RG,
// of the address's resource type, and carries this run's exact seaweed_run tag.
// Anything else refuses. Generic transport forms that name no resource keep
// the existing retry.
func settleCreates(ctx context.Context, call func(context.Context, string, ...string) ([]byte, error), raw []byte, subscription, group, run string) ([]string, error) {
	types := map[string]string{"azurerm_public_ip": "Microsoft.Network/publicIPAddresses", "azurerm_network_interface": "Microsoft.Network/networkInterfaces"}
	// Route by "names an ARM resource ID", whatever the accepted form: the
	// CreateOrUpdate envelope with or without its "Error: " prefix, or the
	// standalone `Put "<URL>": EOF` (codex01 #52 R1). A Put naming no ARM
	// resource ID is a generic form and keeps the plain retry.
	put := regexp.MustCompile(`Put "https://management\.azure\.com(/subscriptions/[^"?]+)[^"]*"`)
	address := regexp.MustCompile(`\n  with (module\.run\.(azurerm_[a-z_]+)\.node\["[a-z0-9-]+"\]),`)
	scope := "/subscriptions/" + subscription + "/resourceGroups/" + group + "/providers/"
	var imported []string
	for _, block := range strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "Error: ") {
		m := put.FindStringSubmatch(block)
		if m == nil {
			continue
		}
		id, a := m[1], address.FindStringSubmatch(block)
		if a == nil || types[a[2]] == "" || len(id) <= len(scope) || !strings.EqualFold(id[:len(scope)], scope) || !strings.HasPrefix(strings.ToLower(id[len(scope):]), strings.ToLower(types[a[2]])+"/") {
			return imported, fmt.Errorf("lost create PUT %s has no in-scope address of its type", id)
		}
		present, b, e := reread(ctx, call, id)
		if e != nil || !present {
			if e != nil {
				return imported, e
			}
			continue
		}
		// The sweep's own ownership check: unambiguous decision keys (one exact
		// seaweed_run, no duplicate or case-aliased keys), this run's tag, this
		// subscription/RG and a supported type (codex01 #52 R2).
		if owned, e := orphanIDs(append(append([]byte("["), b...), ']'), subscription, group, run); e != nil || len(owned) != 1 || !strings.EqualFold(owned[0], id) {
			return imported, fmt.Errorf("created %s is not provably this run's: %v", id, e)
		}
		if _, e = call(ctx, "terraform", "import", "-input=false", "-lock-timeout=0s", "-no-color", a[1], id); e != nil {
			return imported, e
		}
		imported = append(imported, a[1])
	}
	return imported, nil
}

// planDecisionKeys is resourceDecisionKeys for a plan: before the typed
// decode, a duplicate or case-aliased decision field anywhere refuses, since
// Go's decoder would keep the last duplicate (codex01 #52 R3).
func planDecisionKeys(d *json.Decoder) error {
	t, e := d.Token()
	if e != nil || (t != json.Delim('{') && t != json.Delim('[')) {
		return e
	}
	seen := map[string]bool{}
	for d.More() {
		if t == json.Delim('{') {
			k, _ := d.Token()
			key, _ := k.(string)
			for _, canonical := range []string{"resource_changes", "address", "change", "actions"} {
				if strings.EqualFold(key, canonical) {
					if key != canonical || seen[canonical] {
						return errors.New("duplicate/case-aliased plan decision key")
					}
					seen[canonical] = true
				}
			}
		}
		if e = planDecisionKeys(d); e != nil {
			return e
		}
	}
	_, e = d.Token()
	return e
}

// keepsImported refuses a re-plan that would delete or replace an import.
func keepsImported(ctx context.Context, call func(context.Context, string, ...string) ([]byte, error), imported []string) error {
	if len(imported) == 0 {
		return nil
	}
	b, e := call(ctx, "terraform", "show", "-json", "-no-color", "smoke.tfplan")
	var plan *struct {
		Changes []struct {
			Address string
			Change  *struct{ Actions []string }
		} `json:"resource_changes"`
	}
	if e == nil {
		e = planDecisionKeys(json.NewDecoder(bytes.NewReader(b)))
	}
	if e != nil || json.Unmarshal(b, &plan) != nil || plan == nil || plan.Changes == nil {
		return errors.Join(e, errors.New("re-plan after import is unreadable"))
	}
	// Every import must appear with known actions; only an explicit plan that
	// keeps it counts as proof (codex01 #52 R3). A no-op plan is fine.
	known := []string{"no-op", "read", "update", "create", "delete"}
	for _, address := range imported {
		found := false
		for _, rc := range plan.Changes {
			if rc.Address != address {
				continue
			}
			if rc.Change == nil || len(rc.Change.Actions) == 0 {
				return fmt.Errorf("re-plan has no actions for imported %s", address)
			}
			for _, a := range rc.Change.Actions {
				if !slices.Contains(known, a) {
					return fmt.Errorf("re-plan action %q for imported %s is unknown", a, address)
				}
			}
			if slices.Contains(rc.Change.Actions, "delete") {
				return fmt.Errorf("re-plan would replace imported %s", address)
			}
			found = true
		}
		if !found {
			return fmt.Errorf("re-plan does not mention imported %s", address)
		}
	}
	return nil
}

func sweptOnly(ids, swept []string) bool {
	for _, id := range ids {
		found := false
		for _, s := range swept {
			found = found || strings.EqualFold(id, s)
		}
		if !found {
			return false
		}
	}
	return true
}

// deleteResets returns the exact ARM IDs when the text is only complete
// Terraform "deleting <Type> ...: performing Delete" blocks whose response was
// lost or reset, all inside this subscription/RG. Otherwise it returns nil.
func deleteResets(raw []byte, subscription, group string) []string {
	uuid := `[a-f0-9-]{36}`
	block := `Error: deleting [A-Z][A-Za-z ]* \(Subscription: "` + uuid + `"(?:\n[A-Z][A-Za-z ]* Name: "[A-Za-z0-9._-]+"(?:\) for [A-Z][A-Za-z ]* \(Subscription: "` + uuid + `")?)+\): performing Delete: Delete "https://management\.azure\.com(/subscriptions/` + uuid + `/resourceGroups/[A-Za-z0-9._-]+/providers/[A-Za-z]+\.[A-Za-z]+/[A-Za-z]+/[A-Za-z0-9._-]+)\?api-version=[0-9]{4}-[0-9]{2}-[0-9]{2}": HTTP response was nil; connection may have been reset`
	text := strings.TrimSpace(strings.ReplaceAll(string(raw), "\r\n", "\n"))
	if !regexp.MustCompile(`\A` + block + `(?:\s+` + block + `)*\z`).MatchString(text) {
		return nil
	}
	scope := "/subscriptions/" + subscription + "/resourceGroups/" + group + "/providers/"
	var ids []string
	for _, m := range regexp.MustCompile(block).FindAllStringSubmatch(text, -1) {
		if len(m[1]) <= len(scope) || !strings.EqualFold(m[1][:len(scope)], scope) {
			return nil
		}
		ids = append(ids, m[1])
	}
	return ids
}

// reread shows one exact ARM ID. One JSON object is present; az's exact
// ResourceNotFound for this same type, name and RG is absent; anything else errors.
func reread(ctx context.Context, call func(context.Context, string, ...string) ([]byte, error), id string) (bool, []byte, error) {
	b, e := call(ctx, "az", "resource", "show", "--ids", id, "--output", "json", "--only-show-errors")
	if e == nil {
		var object map[string]json.RawMessage
		if json.Unmarshal(b, &object) != nil || object == nil {
			return false, nil, fmt.Errorf("re-read %s: not one standalone JSON object", id)
		}
		return true, b, nil
	}
	diagnostics, failed := b, (*toolError)(nil)
	if errors.As(e, &failed) {
		diagnostics = failed.stderr
	}
	if p := strings.Split(id, "/"); len(p) == 9 {
		line := "The Resource '" + p[6] + "/" + p[7] + "/" + p[8] + "' under resource group '" + p[4] + "' was not found. For more details please go to https://aka.ms/ARMResourceNotFoundFix"
		if strings.TrimSpace(strings.ReplaceAll(string(diagnostics), "\r\n", "\n")) == "ERROR: (ResourceNotFound) "+line+"\nCode: ResourceNotFound\nMessage: "+line {
			return false, nil, nil
		}
	}
	return false, nil, fmt.Errorf("re-read %s: %w", id, e)
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
		recordErr := os.WriteFile(filepath.Join(c.Output, label+"-output"), b, 0600)
		e = errors.Join(e, recordErr)
		status := []byte("OK")
		if e != nil {
			status = []byte(e.Error())
		}
		if w := os.WriteFile(filepath.Join(c.Output, label+"-status"), status, 0600); w != nil {
			return nil, errors.Join(e, w)
		}
		if recordErr != nil {
			return nil, e
		}
		return b, e
	}
	once := call
	call = func(cx context.Context, tool string, args ...string) ([]byte, error) {
		eligible := tool == "terraform" && (args[0] == "apply" || args[0] == "destroy") || tool == "az" && args[0] == "resource" && args[1] == "list"
		var imported []string
		for attempt := 0; ; attempt++ {
			if e := cx.Err(); e != nil {
				return nil, e
			}
			if attempt > 0 && tool == "terraform" && args[0] == "apply" {
				if _, e := once(cx, "terraform", "plan", "-refresh=true", "-input=false", "-lock-timeout=0s", "-out=smoke.tfplan", "-no-color"); e != nil {
					return nil, e
				}
				if e := keepsImported(cx, once, imported); e != nil {
					return nil, e
				}
				if e := cx.Err(); e != nil {
					return nil, e
				}
			}
			b, e := once(cx, tool, args...)
			// Judge the tool's own diagnostics: Terraform's stdout carries refresh,
			// plan and progress text, never the failure being classified.
			diagnostics, failed := b, (*toolError)(nil)
			if errors.As(e, &failed) {
				diagnostics = failed.stderr
			}
			if e == nil || !eligible || attempt == 2 {
				return b, e
			}
			// A destroy whose only failures are lost Delete responses re-reads each
			// exact ID first: absent is deleted, present is re-deleted by the same-state
			// retry. An unknown re-read refuses. Create/PUT keeps the form allowlist.
			if !transportOnly(diagnostics) {
				ids := []string(nil)
				if tool == "terraform" && args[0] == "destroy" {
					ids = deleteResets(diagnostics, d.Subscription, d.Group)
				}
				if len(ids) == 0 {
					return b, e
				}
				for _, id := range ids {
					if _, _, re := reread(cx, once, id); re != nil {
						return b, errors.Join(e, re)
					}
				}
			}
			if tool == "terraform" && args[0] == "apply" && attempt == 0 {
				if w := stamp(filepath.Join(c.Output, "apply-transport-unknown"), "provider settlement required for the failed apply attempt\n"); w != nil {
					return nil, w
				}
			}
			if tool == "terraform" && args[0] == "apply" {
				done, ie := settleCreates(cx, once, diagnostics, d.Subscription, d.Group, filepath.Base(c.Output))
				if ie != nil {
					return b, errors.Join(e, ie)
				}
				imported = append(imported, done...)
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
		// At most 60 min of work plus the 15 min cleanup reserve (Claude01, run 7).
		if remaining <= time.Duration(d.Run.Reserve)*time.Second || remaining > 75*time.Minute || !time.Now().Before(d.Run.Work) {
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
		var swept []string
		if d.Cluster {
			var se error
			swept, se = sweepOrphans(cx, d.Subscription, d.Group, filepath.Base(c.Output), call)
			de = errors.Join(de, se)
		}
		ids, ve := list(cx)
		// ARM's list can lag a delete the sweep itself just confirmed (run7):
		// re-list at most 3 times, 10 s apart, only while every remaining ID is
		// one of those. Any other residue or error stays CLEANUP_INCOMPLETE.
		for try := 0; try < 3 && de == nil && ve == nil && len(ids) > 0 && sweptOnly(ids, swept); try++ {
			timer := time.NewTimer(10 * time.Second)
			select {
			case <-cx.Done():
				timer.Stop()
				ve = cx.Err()
			case <-timer.C:
				ids, ve = list(cx)
			}
		}
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
		return append(b, stderr.Bytes()...), &toolError{fmt.Errorf("%s: %w", tool, e), stderr.Bytes()}
	}
	return b, nil
}

// toolError keeps a failed tool's stderr apart from its stdout for the
// transport classifier; the returned output and message are unchanged.
type toolError struct {
	error
	stderr []byte
}

func (e *toolError) Unwrap() error { return e.error }

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
