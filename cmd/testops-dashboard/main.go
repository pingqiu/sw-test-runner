// Command testops-dashboard is a global view over TestOps run bundles.
//
// Point it at a shared results root that multiple projects/agents write into:
//
//	<root>/<project>/<run-id>/{manifest.json, status.json, result.html, artifacts/}
//
// It walks the root, lists every run (project, scenario, status, time, commit,
// host) and serves each run's result.html. Result browsing is read-only. When
// started with -controller, it can also write RDMA queue request files; the
// worker still owns execution and the lab lock.
//
//	testops-dashboard -root /mnt/smb/work/share/testops/results -port 9099
//
// Decoupled from the runner core on purpose: it only reads the on-disk bundle
// format (manifest.json + result.html), so it never needs the engine or packs.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
)

// manifest is the subset of <run>/manifest.json the dashboard reads. Unknown
// fields are ignored, so it tolerates bundle-format evolution.
type manifest struct {
	RunID        string `json:"run_id"`
	StartedAt    string `json:"started_at"`
	FinishedAt   string `json:"finished_at"`
	ScenarioName string `json:"scenario_name"`
	ScenarioFile string `json:"scenario_file"`
	RunnerVer    string `json:"runner_version"`
	GitSHA       string `json:"git_sha"`
	Host         string `json:"host"`
	Status       string `json:"status"`
	// Metadata is the runner's free-form run metadata (scenario `metadata:` block
	// + `run -meta key=value`): test_id, project, run_by, team, labels, ...
	Metadata map[string]string `json:"metadata"`
}

type run struct {
	manifest
	Project string // metadata.project, else the top-level dir under root, or "-"
	RelDir  string // run dir relative to root (used as the report key)
	HasHTML bool
	Phase   string // in-progress runs only: "current_phase done/total"
	TestID  string // metadata.test_id — stable id for the test (vs per-run RunID)
	RunBy   string // metadata.run_by / runner — who / which agent ran it
	Team    string // metadata.team — owning team
}

type server struct {
	root       string
	docsDir    string // optional: markdown docs served at /docs
	controller *controlConfig
	mu         sync.RWMutex
	runs       []run
}

type controlConfig struct {
	queueDir string
	stateDir string
	logDir   string
	token    string
	now      func() time.Time
}

type controlSubmitRequest struct {
	MonoRef  string `json:"mono_ref"`
	MonoRepo string `json:"mono_repo"`
	RunBy    string `json:"run_by"`
	Team     string `json:"team"`
	Project  string `json:"project"`
	TestID   string `json:"test_id"`
}

type controlSubmitResponse struct {
	RequestID string `json:"request_id"`
	QueuePath string `json:"queue_path"`
	State     string `json:"state"`
}

type controlFileEntry struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	ModTime string `json:"mod_time"`
}

type controlStatus struct {
	Queue       []controlFileEntry `json:"queue"`
	Running     []controlFileEntry `json:"running"`
	Done        []controlFileEntry `json:"done"`
	Failed      []controlFileEntry `json:"failed"`
	StatusFiles []controlFileEntry `json:"status_files"`
	LastRun     map[string]any     `json:"last_run,omitempty"`
	Paths       map[string]string  `json:"paths"`
}

const (
	controlDefaultMonoRepo = "git@github.com:seaweedfs/seaweed-mono.git"
	controlDefaultRunBy    = "testops-dashboard"
	controlDefaultTeam     = "rdma"
	controlDefaultProject  = "rdma-ci"
	controlDefaultTestID   = "rdma-unified-lab-gate"
	controlMaxFieldLen     = 256
	controlMaxListedFiles  = 30
)

var controlSafeValue = regexp.MustCompile(`^[A-Za-z0-9._@:/+=,-]+$`)

// mdRenderer renders the handbook/standard markdown for /docs (GFM: tables,
// fenced code, etc.). Compiled into the binary — still a single static file.
var mdRenderer = goldmark.New(goldmark.WithExtensions(extension.GFM))

func main() {
	root := flag.String("root", "results", "shared results root to scan (per-project subdirs of run bundles)")
	port := flag.Int("port", 9099, "listen port")
	emitMD := flag.String("emit-md", "", "write a markdown runs-index to this file and exit (feed a MkDocs/wiki page)")
	reportBase := flag.String("report-base", "", "base URL for report links in -emit-md (e.g. http://lab:9099); empty = no links")
	docs := flag.String("docs", "", "directory of markdown docs to serve at /docs (e.g. the runner's docs/)")
	controller := flag.Bool("controller", false, "enable safe RDMA queue submit/status panel")
	controllerQueue := flag.String("controller-queue", "/mnt/smb/work/share/testops/queue/rdma-ci", "controller RDMA queue directory")
	controllerState := flag.String("controller-state", "/mnt/smb/work/share/testops/state/rdma-ci", "controller RDMA state directory")
	controllerLogs := flag.String("controller-logs", "/mnt/smb/work/share/testops/logs/rdma-ci", "controller RDMA log directory")
	controllerToken := flag.String("controller-token", os.Getenv("TESTOPS_CONTROLLER_TOKEN"), "optional submit token for controller POSTs")
	flag.Parse()

	abs, err := filepath.Abs(*root)
	if err != nil {
		log.Fatalf("root: %v", err)
	}
	s := &server{root: abs}
	if *docs != "" {
		if d, e := filepath.Abs(*docs); e == nil {
			s.docsDir = d
		}
	}
	if *controller {
		s.controller = &controlConfig{
			queueDir: *controllerQueue,
			stateDir: *controllerState,
			logDir:   *controllerLogs,
			token:    *controllerToken,
			now:      time.Now,
		}
		if err := s.ensureControlDirs(); err != nil {
			log.Fatalf("controller dirs: %v", err)
		}
	}
	s.scan()

	// One-shot markdown emit (for a wiki / MkDocs page); no server.
	if *emitMD != "" {
		if err := emitMarkdown(s.snapshot(), *emitMD, *reportBase); err != nil {
			log.Fatalf("emit-md: %v", err)
		}
		log.Printf("wrote %s (%d runs)", *emitMD, len(s.runs))
		return
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/report", s.handleReport)
	mux.HandleFunc("/docs", s.handleDocs)
	mux.HandleFunc("/api/runs", s.handleAPIRuns)
	mux.HandleFunc("/api/controller/status", s.handleAPIControllerStatus)
	mux.HandleFunc("/api/rdma/submit", s.handleSubmitRDMA)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "ok") })

	mode := "read-only"
	if s.controller != nil {
		mode = "controller-enabled"
	}
	log.Printf("testops-dashboard (%s) on http://localhost:%d  root=%s  docs=%q  runs=%d", mode, *port, abs, s.docsDir, len(s.runs))
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", *port), mux))
}

// scan walks the root for every directory containing manifest.json (= one run
// bundle) and rebuilds the run list. Called on start and on each page load so
// the view reflects new bundles without a restart.
func (s *server) scan() {
	var found []run
	filepath.WalkDir(s.root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != "manifest.json" {
			return nil
		}
		runDir := filepath.Dir(path)
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		var m manifest
		if json.Unmarshal(raw, &m) != nil {
			return nil
		}
		rel, _ := filepath.Rel(s.root, runDir)
		rel = filepath.ToSlash(rel)
		project := "-"
		if parts := strings.SplitN(rel, "/", 2); len(parts) > 1 {
			project = parts[0]
		}
		// Authoritative status lives in status.json (the runner writes state:
		// running/pass/fail there). manifest.json carries no status field, so only
		// fall back to it for legacy/synthetic bundles that embed one.
		phase := ""
		if sraw, e := os.ReadFile(filepath.Join(runDir, "status.json")); e == nil {
			var st struct {
				State        string `json:"state"`
				CurrentPhase string `json:"current_phase"`
				PhasesDone   int    `json:"phases_done"`
				PhasesTotal  int    `json:"phases_total"`
			}
			if json.Unmarshal(sraw, &st) == nil && st.State != "" {
				m.Status = st.State
				if strings.EqualFold(st.State, "running") && st.PhasesTotal > 0 {
					phase = fmt.Sprintf("%s %d/%d", st.CurrentPhase, st.PhasesDone, st.PhasesTotal)
				}
			}
		}
		// Metadata: manifest.metadata (scenario block + `run -meta`) overlaid with
		// an optional meta.json sidecar so an agent can annotate a run post-hoc.
		meta := map[string]string{}
		for k, v := range m.Metadata {
			meta[k] = v
		}
		if mraw, e := os.ReadFile(filepath.Join(runDir, "meta.json")); e == nil {
			var sm map[string]string
			if json.Unmarshal(mraw, &sm) == nil {
				for k, v := range sm {
					meta[k] = v
				}
			}
		}
		if p := meta["project"]; p != "" {
			project = p // an explicit project wins over the directory name
		}
		runBy := meta["run_by"]
		if runBy == "" {
			runBy = meta["runner"]
		}
		_, htmlErr := os.Stat(filepath.Join(runDir, "result.html"))
		found = append(found, run{
			manifest: m,
			Project:  project,
			RelDir:   rel,
			HasHTML:  htmlErr == nil,
			Phase:    phase,
			TestID:   meta["test_id"],
			RunBy:    runBy,
			Team:     meta["team"],
		})
		return nil
	})
	// Newest first by started_at (RFC3339 strings sort correctly).
	sort.Slice(found, func(i, j int) bool { return found[i].StartedAt > found[j].StartedAt })
	s.mu.Lock()
	s.runs = found
	s.mu.Unlock()
}

func (s *server) snapshot() []run {
	s.scan()
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]run, len(s.runs))
	copy(out, s.runs)
	return out
}

// emitMarkdown writes a MkDocs/wiki-friendly runs-index page. Run it periodically
// (cron) pointing at the shared results root + a MkDocs docs path, e.g.:
//
//	testops-dashboard -root /mnt/smb/.../testops/results \
//	  -emit-md /c/work/seaweed_block/docs/wiki/testops-runs.md \
//	  -report-base http://lab-host:9099
//
// MkDocs then renders it as a wiki page; report links point at the live dashboard.
func emitMarkdown(runs []run, path, reportBase string) error {
	projects := map[string]bool{}
	pass, fail := 0, 0
	for _, rn := range runs {
		projects[rn.Project] = true
		switch strings.ToLower(rn.Status) {
		case "pass":
			pass++
		case "fail", "error":
			fail++
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# TestOps — global runs\n\n")
	fmt.Fprintf(&b, "_Generated %s · %d projects · %d runs · %d pass · %d fail. "+
		"Read-only; regenerated from `results/<project>/<run>/`._\n\n",
		time.Now().UTC().Format("2006-01-02 15:04 UTC"), len(projects), len(runs), pass, fail)
	b.WriteString("| Project | Scenario | Status | Started | Commit | Host | Report |\n")
	b.WriteString("|---|---|---|---|---|---|---|\n")
	for _, rn := range runs {
		icon := "⚪"
		switch strings.ToLower(rn.Status) {
		case "pass":
			icon = "✅"
		case "fail", "error":
			icon = "❌"
		}
		report := "—"
		if reportBase != "" && rn.HasHTML {
			report = fmt.Sprintf("[result.html](%s/report?run=%s)", strings.TrimRight(reportBase, "/"), rn.RelDir)
		}
		fmt.Fprintf(&b, "| %s | %s | %s %s | %s | `%s` | %s | %s |\n",
			rn.Project, mdEsc(rn.ScenarioName), icon, rn.Status, rn.StartedAt, rn.GitSHA, rn.Host, report)
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

func mdEsc(s string) string { return strings.ReplaceAll(s, "|", "\\|") }

func (s *server) handleAPIRuns(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.snapshot())
}

func (s *server) handleAPIControllerStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.controller == nil {
		http.Error(w, "controller not enabled", http.StatusNotFound)
		return
	}
	status := s.controlStatus()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(status)
}

func (s *server) handleSubmitRDMA(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.controller == nil {
		http.Error(w, "controller not enabled", http.StatusNotFound)
		return
	}
	if !s.controlAuthorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	req, err := parseControlSubmit(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	resp, err := s.submitControl(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if isJSONRequest(r) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(resp)
		return
	}
	http.Redirect(w, r, "/?submitted="+resp.RequestID, http.StatusSeeOther)
}

func isJSONRequest(r *http.Request) bool {
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return strings.EqualFold(ct, "application/json")
}

func (s *server) ensureControlDirs() error {
	if s.controller == nil {
		return nil
	}
	for _, dir := range []string{
		s.controller.queueDir,
		filepath.Join(s.controller.stateDir, "running"),
		filepath.Join(s.controller.stateDir, "done"),
		filepath.Join(s.controller.stateDir, "failed"),
		filepath.Join(s.controller.stateDir, "status"),
		s.controller.logDir,
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("%s: %w", dir, err)
		}
	}
	return nil
}

func (s *server) controlAuthorized(r *http.Request) bool {
	if s.controller.token == "" {
		return true
	}
	if r.Header.Get("X-TestOps-Token") == s.controller.token {
		return true
	}
	if err := r.ParseForm(); err == nil && r.Form.Get("token") == s.controller.token {
		return true
	}
	return false
}

func parseControlSubmit(r *http.Request) (controlSubmitRequest, error) {
	var req controlSubmitRequest
	if isJSONRequest(r) {
		var raw map[string]string
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			return req, fmt.Errorf("invalid JSON: %w", err)
		}
		req = controlSubmitRequest{
			MonoRef:  raw["mono_ref"],
			MonoRepo: raw["mono_repo"],
			RunBy:    raw["run_by"],
			Team:     raw["team"],
			Project:  raw["project"],
			TestID:   raw["test_id"],
		}
	} else {
		if err := r.ParseForm(); err != nil {
			return req, fmt.Errorf("invalid form: %w", err)
		}
		req = controlSubmitRequest{
			MonoRef:  r.Form.Get("mono_ref"),
			MonoRepo: r.Form.Get("mono_repo"),
			RunBy:    r.Form.Get("run_by"),
			Team:     r.Form.Get("team"),
			Project:  r.Form.Get("project"),
			TestID:   r.Form.Get("test_id"),
		}
	}
	req = withControlDefaults(req)
	return req, validateControlSubmit(req)
}

func withControlDefaults(req controlSubmitRequest) controlSubmitRequest {
	if req.MonoRef == "" {
		req.MonoRef = "main"
	}
	if req.MonoRepo == "" {
		req.MonoRepo = controlDefaultMonoRepo
	}
	if req.RunBy == "" {
		req.RunBy = controlDefaultRunBy
	}
	if req.Team == "" {
		req.Team = controlDefaultTeam
	}
	if req.Project == "" {
		req.Project = controlDefaultProject
	}
	if req.TestID == "" {
		req.TestID = controlDefaultTestID
	}
	return req
}

func validateControlSubmit(req controlSubmitRequest) error {
	checks := map[string]string{
		"mono_ref":  req.MonoRef,
		"mono_repo": req.MonoRepo,
		"run_by":    req.RunBy,
		"team":      req.Team,
		"project":   req.Project,
		"test_id":   req.TestID,
	}
	for name, value := range checks {
		if err := validateControlField(name, value); err != nil {
			return err
		}
	}
	return nil
}

func validateControlField(name, value string) error {
	if value == "" {
		return fmt.Errorf("%s is required", name)
	}
	if len(value) > controlMaxFieldLen {
		return fmt.Errorf("%s is too long", name)
	}
	if !controlSafeValue.MatchString(value) {
		return fmt.Errorf("%s has unsupported characters", name)
	}
	return nil
}

func (s *server) submitControl(req controlSubmitRequest) (controlSubmitResponse, error) {
	req = withControlDefaults(req)
	if err := validateControlSubmit(req); err != nil {
		return controlSubmitResponse{}, err
	}
	if err := s.ensureControlDirs(); err != nil {
		return controlSubmitResponse{}, err
	}
	now := s.controller.now().UTC()
	requestID := fmt.Sprintf("%s-%09d-%s-%d", now.Format("20060102-150405"), now.Nanosecond(), sanitizeControlID(req.MonoRef), os.Getpid())
	reqPath := filepath.Join(s.controller.queueDir, requestID+".env")
	if _, err := os.Stat(reqPath); err == nil {
		return controlSubmitResponse{}, fmt.Errorf("request id collision")
	}
	tmp, err := os.CreateTemp(s.controller.queueDir, "."+requestID+".*.tmp")
	if err != nil {
		return controlSubmitResponse{}, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.WriteString(buildControlEnv(requestID, req)); err != nil {
		tmp.Close()
		return controlSubmitResponse{}, err
	}
	if err := tmp.Close(); err != nil {
		return controlSubmitResponse{}, err
	}
	if err := os.Rename(tmpName, reqPath); err != nil {
		return controlSubmitResponse{}, err
	}
	return controlSubmitResponse{RequestID: requestID, QueuePath: reqPath, State: "queued"}, nil
}

func buildControlEnv(requestID string, req controlSubmitRequest) string {
	lines := []string{
		"REQUEST_ID=" + shellQuoteControl(requestID),
		"TESTOPS_MONO_REF=" + shellQuoteControl(req.MonoRef),
		"TESTOPS_MONO_REPO=" + shellQuoteControl(req.MonoRepo),
		"TESTOPS_RUN_BY=" + shellQuoteControl(req.RunBy),
		"TESTOPS_TEAM=" + shellQuoteControl(req.Team),
		"TESTOPS_PROJECT=" + shellQuoteControl(req.Project),
		"TESTOPS_TEST_ID=" + shellQuoteControl(req.TestID),
		"TESTOPS_BRANCH=" + shellQuoteControl(req.MonoRef),
		"TESTOPS_COMMIT=" + shellQuoteControl(req.MonoRef),
	}
	return strings.Join(lines, "\n") + "\n"
}

func shellQuoteControl(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

func sanitizeControlID(value string) string {
	var b strings.Builder
	for _, r := range value {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	out := b.String()
	if len(out) > 80 {
		return out[:80]
	}
	return out
}

func (s *server) controlStatus() controlStatus {
	if s.controller == nil {
		return controlStatus{}
	}
	last, _ := readJSONMap(filepath.Join(s.controller.stateDir, "status", "last-run.json"))
	return controlStatus{
		Queue:       listControlFiles(s.controller.queueDir, "*.env"),
		Running:     listControlFiles(filepath.Join(s.controller.stateDir, "running"), "*.env"),
		Done:        listControlFiles(filepath.Join(s.controller.stateDir, "done"), "*.env"),
		Failed:      listControlFiles(filepath.Join(s.controller.stateDir, "failed"), "*.env"),
		StatusFiles: listControlFiles(filepath.Join(s.controller.stateDir, "status"), "*.json"),
		LastRun:     last,
		Paths: map[string]string{
			"queue":  s.controller.queueDir,
			"state":  s.controller.stateDir,
			"logs":   s.controller.logDir,
			"status": filepath.Join(s.controller.stateDir, "status"),
		},
	}
}

func listControlFiles(dir, pattern string) []controlFileEntry {
	matches, _ := filepath.Glob(filepath.Join(dir, pattern))
	out := make([]controlFileEntry, 0, len(matches))
	for _, path := range matches {
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			continue
		}
		out = append(out, controlFileEntry{
			Name:    info.Name(),
			Path:    path,
			Size:    info.Size(),
			ModTime: info.ModTime().Format(time.RFC3339),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModTime > out[j].ModTime })
	if len(out) > controlMaxListedFiles {
		return out[:controlMaxListedFiles]
	}
	return out
}

func readJSONMap(path string) (map[string]any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	return doc, nil
}

// handleReport serves <root>/<run>/result.html. The run key is the bundle's
// dir relative to root; it is validated to stay under root (no traversal).
func (s *server) handleReport(w http.ResponseWriter, r *http.Request) {
	rel := filepath.FromSlash(strings.TrimSpace(r.URL.Query().Get("run")))
	if rel == "" {
		http.Error(w, "run query param required", http.StatusBadRequest)
		return
	}
	target := filepath.Join(s.root, rel, "result.html")
	clean := filepath.Clean(target)
	if !strings.HasPrefix(clean, s.root+string(os.PathSeparator)) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	http.ServeFile(w, r, clean)
}

func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	all := s.snapshot()
	fProject := strings.TrimSpace(r.URL.Query().Get("project"))
	fTeam := strings.TrimSpace(r.URL.Query().Get("team"))
	runs := make([]run, 0, len(all))
	projects := map[string]int{}
	pass, fail, other, running := 0, 0, 0, 0
	for _, rn := range all {
		if fProject != "" && rn.Project != fProject {
			continue
		}
		if fTeam != "" && rn.Team != fTeam {
			continue
		}
		runs = append(runs, rn)
		projects[rn.Project]++
		switch strings.ToLower(rn.Status) {
		case "pass", "passed":
			pass++
		case "fail", "failed", "error":
			fail++
		case "running", "in_progress":
			running++
		default:
			other++
		}
	}
	filter := ""
	if fProject != "" {
		filter = "project=" + fProject
	}
	if fTeam != "" {
		if filter != "" {
			filter += " · "
		}
		filter += "team=" + fTeam
	}
	var ctrl controlStatus
	if s.controller != nil {
		ctrl = s.controlStatus()
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	indexTmpl.Execute(w, map[string]any{
		"Runs":              runs,
		"Root":              s.root,
		"Projects":          len(projects),
		"Total":             len(runs),
		"Running":           running,
		"Pass":              pass,
		"Fail":              fail,
		"Other":             other,
		"Filter":            filter,
		"Now":               time.Now().UTC().Format("2006-01-02 15:04:05 UTC"),
		"ControllerEnabled": s.controller != nil,
		"Control":           ctrl,
		"Submitted":         strings.TrimSpace(r.URL.Query().Get("submitted")),
	})
}

// handleDocs serves the markdown docs (handbook/standard) at /docs, rendered to
// HTML. /docs lists them; /docs?f=<file>.md renders one. Read-only.
func (s *server) handleDocs(w http.ResponseWriter, r *http.Request) {
	if s.docsDir == "" {
		http.Error(w, "docs not configured (start with -docs <dir>)", http.StatusNotFound)
		return
	}
	f := strings.TrimSpace(r.URL.Query().Get("f"))
	if f == "" {
		files, _ := filepath.Glob(filepath.Join(s.docsDir, "*.md"))
		rank := map[string]int{}
		for i, f := range docReadingOrder {
			rank[f] = i
		}
		type entry struct{ File, Title string }
		var docs []entry
		for _, p := range files {
			f := filepath.Base(p)
			if _, ok := rank[f]; !ok {
				continue // hide internal roadmap/evidence from the agent-facing list
			}
			docs = append(docs, entry{File: f, Title: docTitle(p)})
		}
		sort.Slice(docs, func(i, j int) bool { return rank[docs[i].File] < rank[docs[j].File] })
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		docsListTmpl.Execute(w, map[string]any{"Docs": docs})
		return
	}
	if strings.ContainsAny(f, `/\`) || strings.Contains(f, "..") || !strings.HasSuffix(f, ".md") {
		http.Error(w, "bad doc name", http.StatusBadRequest)
		return
	}
	src, err := os.ReadFile(filepath.Join(s.docsDir, f))
	if err != nil {
		http.Error(w, "doc not found", http.StatusNotFound)
		return
	}
	var buf bytes.Buffer
	if err := mdRenderer.Convert(src, &buf); err != nil {
		http.Error(w, "render: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	docPageTmpl.Execute(w, map[string]any{"Title": f, "Body": template.HTML(buf.String())})
}

// docReadingOrder is the curated, agent-facing follow-set shown at /docs, in
// reading order. ONLY these are listed (internal roadmap/evidence md in the docs
// dir are hidden from the list, though still renderable by direct URL). Hand an
// agent /docs and this is the path: how → the contract → the schema → optional.
var docReadingOrder = []string{
	"testops-handbook.md",               // how: lab access, run, watch, the process
	"cross-product-testops-standard.md", // the contract to follow (observable runs)
	"scenario-spec.md",                  // the exact YAML schema
	"tutorial.md",                       // optional hands-on intro
	"rdma-vfs-s3-testing-start-here.md", // product guide: VFS + S3 over RDMA (sra-next)
}

func docTitle(path string) string {
	src, err := os.ReadFile(path)
	if err != nil {
		return filepath.Base(path)
	}
	for _, line := range strings.Split(string(src), "\n") {
		if strings.HasPrefix(line, "# ") {
			return strings.TrimSpace(line[2:])
		}
	}
	return filepath.Base(path)
}

const docCSS = `body{font-family:-apple-system,Segoe UI,Roboto,sans-serif;background:#15151f;color:#dcdce6;margin:0}
nav{background:#1d1d2e;padding:10px 20px;border-bottom:1px solid #2a2a4a;font-size:.9em}
nav a{color:#5aa0ff;text-decoration:none;margin-right:16px}nav a:hover{text-decoration:underline}
.wrap{max-width:900px;margin:0 auto;padding:24px 28px;line-height:1.55}
.wrap h1,.wrap h2,.wrap h3{color:#cfe0ff;border-bottom:1px solid #24243a;padding-bottom:4px;margin-top:1.4em}
.wrap code{background:#23233a;color:#9ad;padding:1px 5px;border-radius:3px;font-size:.92em}
.wrap pre{background:#101019;border:1px solid #24243a;border-radius:6px;padding:12px;overflow:auto}
.wrap pre code{background:none;color:#cdd}
.wrap table{border-collapse:collapse;margin:1em 0}.wrap th,.wrap td{border:1px solid #2a2a4a;padding:6px 12px}
.wrap th{background:#1d1d2e;color:#8a8ab0}.wrap a{color:#5aa0ff}
.wrap blockquote{border-left:3px solid #3a3a6a;margin:1em 0;padding:.2em 1em;color:#aab}
.doclist a{display:block;padding:10px 14px;margin:6px 0;background:#1c1c30;border-radius:5px;color:#dcdce6;text-decoration:none}
.doclist a:hover{background:#26264a}.doclist .f{color:#888;font-size:.8em}`

var docsListTmpl = template.Must(template.New("dl").Parse(`<!DOCTYPE html><html lang="en"><head><meta charset="utf-8">
<title>TestOps — docs</title><style>` + docCSS + `</style></head><body>
<nav><a href="/">‹ Runs</a><a href="/docs">Docs</a></nav>
<div class="wrap doclist"><h1>TestOps docs</h1>
<p style="color:#8a8ab0">Read in order: <b>Handbook</b> (how, on our lab) → <b>Standard</b> (the contract to follow) → <b>Scenario Spec</b> (the YAML schema) → <b>Tutorial</b> (optional). Then <b>product testing guides</b> (e.g. VFS/S3 over RDMA). Follow the Standard so runs land in the shared results root and show up here.</p>
{{range .Docs}}<a href="/docs?f={{.File}}"><b>{{.Title}}</b><div class="f">{{.File}}</div></a>{{else}}<p>no markdown under -docs dir</p>{{end}}
</div></body></html>`))

var docPageTmpl = template.Must(template.New("dp").Parse(`<!DOCTYPE html><html lang="en"><head><meta charset="utf-8">
<title>{{.Title}} — TestOps docs</title><style>` + docCSS + `</style></head><body>
<nav><a href="/">‹ Runs</a><a href="/docs">‹ Docs</a><span style="color:#666">{{.Title}}</span></nav>
<div class="wrap">{{.Body}}</div></body></html>`))

func statusClass(s string) string {
	switch strings.ToLower(s) {
	case "pass", "passed":
		return "pass"
	case "fail", "failed", "error":
		return "fail"
	case "running", "in_progress":
		return "running"
	default:
		return "other"
	}
}

var indexTmpl = template.Must(template.New("idx").Funcs(template.FuncMap{
	"sclass": statusClass,
}).Parse(`<!DOCTYPE html><html lang="en"><head><meta charset="utf-8">
<title>TestOps — global runs</title><meta http-equiv="refresh" content="15">
<style>
 body{font-family:-apple-system,Segoe UI,Roboto,monospace;background:#15151f;color:#e0e0e0;margin:0}
 header{background:#1d1d2e;padding:12px 20px;border-bottom:1px solid #2a2a4a;display:flex;gap:18px;align-items:baseline}
 h1{font-size:1.25em;color:#a0a0c0;margin:0} .muted{color:#777;font-size:.9em}
 .pill{padding:3px 10px;border-radius:3px;font-size:.9em;font-weight:bold}
 .pass{background:#1e5631;color:#b7e4c7}.fail{background:#7a241c;color:#f5b7b1}.other{background:#5a4a08;color:#f9e79f}
 .running{background:#1c3a5e;color:#9ad0ff;animation:pulse 1.4s ease-in-out infinite}
 @keyframes pulse{50%{opacity:.5}}
 table{width:100%;border-collapse:collapse;font-size:1.12em} th,td{text-align:left;padding:10px 16px;border-bottom:1px solid #24243a}
 th{color:#8a8ab0;font-weight:600;position:sticky;top:0;background:#15151f} tr:hover{background:#1c1c30}
 a{color:#5aa0ff;text-decoration:none} a:hover{text-decoration:underline} code{color:#9ad}
 .proj{color:#c0a0e0}
 .chip{background:#2a2a44;color:#b9b9e0;padding:1px 7px;border-radius:8px;font-size:.7em;margin-left:6px}
 .sub{display:block;color:#777;font-size:.74em;margin-top:2px}
 .bar{background:#23233a;padding:6px 20px;font-size:.85em;color:#9ad0ff}
 .control{margin:14px 20px;padding:12px 14px;background:#1b1b2b;border:1px solid #2a2a4a;border-radius:6px}
 .control h2{font-size:1em;margin:0 0 8px;color:#cfd8ff}.control form{display:flex;gap:8px;align-items:end;flex-wrap:wrap}
 .control label{font-size:.78em;color:#9aa;display:block}.control input{background:#11111b;color:#e0e0e0;border:1px solid #3a3a5a;border-radius:4px;padding:6px;min-width:220px}
 .control button{background:#1e5631;color:#dff5e5;border:0;border-radius:4px;padding:7px 12px;font-weight:bold;cursor:pointer}
 .controlgrid{display:grid;grid-template-columns:repeat(auto-fit,minmax(120px,1fr));gap:8px;margin:10px 0}.controlbox{background:#23233a;border-radius:5px;padding:8px}
 .controlbox b{display:block;font-size:1.4em}.submitted{color:#b7e4c7;margin:8px 0 0}
</style></head><body>
<header><h1>TestOps — global runs</h1>
 <a href="/docs" style="color:#5aa0ff;text-decoration:none;font-size:.85em">Docs ›</a>
 <span class="muted">root <code>{{.Root}}</code></span>
 <span class="muted">{{.Projects}} projects · {{.Total}} runs ·
   {{if .Running}}<span class="pill running">{{.Running}} running</span>{{end}}
   <span class="pill pass">{{.Pass}} pass</span>
   <span class="pill fail">{{.Fail}} fail</span>
   <span class="pill other">{{.Other}} other</span></span>
 <span class="muted" style="margin-left:auto">{{if .ControllerEnabled}}controller-enabled{{else}}read-only{{end}} · {{.Now}} · auto-refresh 15s</span>
</header>
{{if .ControllerEnabled}}
<section class="control">
 <h2>RDMA CI Controller</h2>
 {{if .Submitted}}<div class="submitted">queued request <code>{{.Submitted}}</code></div>{{end}}
 <form method="post" action="/api/rdma/submit">
  <div><label>mono ref</label><input name="mono_ref" value="main" required></div>
  <div><label>run by</label><input name="run_by" value="dashboard" required></div>
  <div><label>token, if configured</label><input name="token" type="password"></div>
  <input type="hidden" name="team" value="rdma">
  <input type="hidden" name="project" value="rdma-ci">
  <input type="hidden" name="test_id" value="rdma-unified-lab-gate">
  <button type="submit">Queue RDMA Gate</button>
 </form>
 <div class="controlgrid">
  <div class="controlbox"><span class="muted">queued</span><b>{{len .Control.Queue}}</b></div>
  <div class="controlbox"><span class="muted">running</span><b>{{len .Control.Running}}</b></div>
  <div class="controlbox"><span class="muted">done</span><b>{{len .Control.Done}}</b></div>
  <div class="controlbox"><span class="muted">failed</span><b>{{len .Control.Failed}}</b></div>
 </div>
 <div class="muted">API: <code>/api/controller/status</code> · submit: <code>/api/rdma/submit</code></div>
 {{if .Control.LastRun}}<div class="muted">last request: {{index .Control.LastRun "request_id"}} · {{index .Control.LastRun "state"}} · {{index .Control.LastRun "bundle_dir"}}</div>{{end}}
</section>
{{end}}
{{if .Filter}}<div class="bar">filtered: <b>{{.Filter}}</b> · <a href="/">clear ✕</a></div>{{end}}
<table><thead><tr>
 <th>project</th><th>scenario</th><th>status</th><th>started</th><th>commit</th><th>host</th><th>report</th>
</tr></thead><tbody>
{{range .Runs}}<tr>
 <td class="proj"><a class="proj" href="/?project={{.Project}}">{{.Project}}</a>{{if .Team}}<a class="chip" href="/?team={{.Team}}">{{.Team}}</a>{{end}}</td>
 <td>{{.ScenarioName}}{{if .TestID}}<span class="sub">{{.TestID}}</span>{{end}}</td>
 <td><span class="pill {{sclass .Status}}">{{.Status}}</span>{{if .Phase}} <span class="muted">{{.Phase}}</span>{{end}}</td>
 <td class="muted">{{.StartedAt}}</td>
 <td><code>{{.GitSHA}}</code></td>
 <td class="muted">{{.Host}}{{if .RunBy}}<span class="sub">by {{.RunBy}}</span>{{end}}</td>
 <td>{{if .HasHTML}}<a href="/report?run={{.RelDir}}" target="_blank">result.html</a>{{else}}<span class="muted">-</span>{{end}}</td>
</tr>{{else}}<tr><td colspan="7" class="muted">no run bundles under root</td></tr>{{end}}
</tbody></table></body></html>`))
