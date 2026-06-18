// Command testops-dashboard is a READ-ONLY global view over TestOps run bundles.
//
// Point it at a shared results root that multiple projects/agents write into:
//
//	<root>/<project>/<run-id>/{manifest.json, status.json, result.html, artifacts/}
//
// It walks the root, lists every run (project, scenario, status, time, commit,
// host) and serves each run's result.html. It does NOT run scenarios and never
// writes — so any number of project agents (block-qa, vfs-rdma-qa, ...) can keep
// writing their own bundles while one dashboard gives a global, non-conflicting,
// read-only picture of "what ran + the reports".
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
	"net/http"
	"os"
	"path/filepath"
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
}

type run struct {
	manifest
	Project string // top-level dir under root (the owning project/agent), or "-"
	RelDir  string // run dir relative to root (used as the report key)
	HasHTML bool
}

type server struct {
	root    string
	docsDir string // optional: markdown docs served at /docs
	mu      sync.RWMutex
	runs    []run
}

// mdRenderer renders the handbook/standard markdown for /docs (GFM: tables,
// fenced code, etc.). Compiled into the binary — still a single static file.
var mdRenderer = goldmark.New(goldmark.WithExtensions(extension.GFM))

func main() {
	root := flag.String("root", "results", "shared results root to scan (per-project subdirs of run bundles)")
	port := flag.Int("port", 9099, "listen port")
	emitMD := flag.String("emit-md", "", "write a markdown runs-index to this file and exit (feed a MkDocs/wiki page)")
	reportBase := flag.String("report-base", "", "base URL for report links in -emit-md (e.g. http://lab:9099); empty = no links")
	docs := flag.String("docs", "", "directory of markdown docs to serve at /docs (e.g. the runner's docs/)")
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
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "ok") })

	log.Printf("testops-dashboard (read-only) on http://localhost:%d  root=%s  docs=%q  runs=%d", *port, abs, s.docsDir, len(s.runs))
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
		_, htmlErr := os.Stat(filepath.Join(runDir, "result.html"))
		found = append(found, run{
			manifest: m,
			Project:  project,
			RelDir:   rel,
			HasHTML:  htmlErr == nil,
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
	runs := s.snapshot()
	projects := map[string]int{}
	pass, fail, other := 0, 0, 0
	for _, rn := range runs {
		projects[rn.Project]++
		switch strings.ToLower(rn.Status) {
		case "pass":
			pass++
		case "fail", "error":
			fail++
		default:
			other++
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	indexTmpl.Execute(w, map[string]any{
		"Runs":     runs,
		"Root":     s.root,
		"Projects": len(projects),
		"Total":    len(runs),
		"Pass":     pass,
		"Fail":     fail,
		"Other":    other,
		"Now":      time.Now().UTC().Format("2006-01-02 15:04:05 UTC"),
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
		type entry struct{ File, Title string }
		var docs []entry
		for _, p := range files {
			docs = append(docs, entry{File: filepath.Base(p), Title: docTitle(p)})
		}
		sort.Slice(docs, func(i, j int) bool { return docs[i].File < docs[j].File })
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
{{range .Docs}}<a href="/docs?f={{.File}}"><b>{{.Title}}</b><div class="f">{{.File}}</div></a>{{else}}<p>no markdown under -docs dir</p>{{end}}
</div></body></html>`))

var docPageTmpl = template.Must(template.New("dp").Parse(`<!DOCTYPE html><html lang="en"><head><meta charset="utf-8">
<title>{{.Title}} — TestOps docs</title><style>` + docCSS + `</style></head><body>
<nav><a href="/">‹ Runs</a><a href="/docs">‹ Docs</a><span style="color:#666">{{.Title}}</span></nav>
<div class="wrap">{{.Body}}</div></body></html>`))

func statusClass(s string) string {
	switch strings.ToLower(s) {
	case "pass":
		return "pass"
	case "fail", "error":
		return "fail"
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
 h1{font-size:1.05em;color:#a0a0c0;margin:0} .muted{color:#777;font-size:.8em}
 .pill{padding:2px 8px;border-radius:3px;font-size:.72em;font-weight:bold}
 .pass{background:#1e5631;color:#b7e4c7}.fail{background:#7a241c;color:#f5b7b1}.other{background:#5a4a08;color:#f9e79f}
 table{width:100%;border-collapse:collapse;font-size:.86em} th,td{text-align:left;padding:7px 14px;border-bottom:1px solid #24243a}
 th{color:#8a8ab0;font-weight:600;position:sticky;top:0;background:#15151f} tr:hover{background:#1c1c30}
 a{color:#5aa0ff;text-decoration:none} a:hover{text-decoration:underline} code{color:#9ad}
 .proj{color:#c0a0e0}
</style></head><body>
<header><h1>TestOps — global runs</h1>
 <a href="/docs" style="color:#5aa0ff;text-decoration:none;font-size:.85em">Docs ›</a>
 <span class="muted">root <code>{{.Root}}</code></span>
 <span class="muted">{{.Projects}} projects · {{.Total}} runs ·
   <span class="pill pass">{{.Pass}} pass</span>
   <span class="pill fail">{{.Fail}} fail</span>
   <span class="pill other">{{.Other}} other</span></span>
 <span class="muted" style="margin-left:auto">read-only · {{.Now}} · auto-refresh 15s</span>
</header>
<table><thead><tr>
 <th>project</th><th>scenario</th><th>status</th><th>started</th><th>commit</th><th>host</th><th>report</th>
</tr></thead><tbody>
{{range .Runs}}<tr>
 <td class="proj">{{.Project}}</td>
 <td>{{.ScenarioName}}</td>
 <td><span class="pill {{sclass .Status}}">{{.Status}}</span></td>
 <td class="muted">{{.StartedAt}}</td>
 <td><code>{{.GitSHA}}</code></td>
 <td class="muted">{{.Host}}</td>
 <td>{{if .HasHTML}}<a href="/report?run={{.RelDir}}" target="_blank">result.html</a>{{else}}<span class="muted">-</span>{{end}}</td>
</tr>{{else}}<tr><td colspan="7" class="muted">no run bundles under root</td></tr>{{end}}
</tbody></table></body></html>`))
