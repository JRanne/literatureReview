// Package server exposes the review workflow over HTTP.
package server

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"literaturereview/internal/ai"
	"literaturereview/internal/browserlogin"
	"literaturereview/internal/config"
	"literaturereview/internal/fetch"
	"literaturereview/internal/fulltext"
	"literaturereview/internal/jev"
	"literaturereview/internal/model"
	"literaturereview/internal/prisma"
	"literaturereview/internal/sources"
	"literaturereview/internal/store"
	"literaturereview/internal/venues"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static/*
var staticFS embed.FS

// Server wires configuration, storage and services to HTTP handlers.
type Server struct {
	cfg   config.Config
	store *store.Store
	ai    *ai.Client
	jev   *jev.Client
	reg   *sources.Registry
	chain *fetch.Chain
	tmpl  *template.Template

	jobMu sync.Mutex
	jobs  map[string]context.CancelFunc
	login loginState

	jufoOnce    sync.Once
	raterClient *venues.Rater
}

// New constructs the server and parses templates.
func New(cfg config.Config, st *store.Store) (*Server, error) {
	s := &Server{
		cfg:   cfg,
		store: st,
		ai:    newAI(cfg),
		jev:   jev.New(cfg.JevAPIKey, cfg.JevModel, cfg.JevURL),
		reg:   sources.NewRegistry(cfg),
		chain: fetch.NewChain(cfg),
		jobs:  map[string]context.CancelFunc{},
	}
	t, err := template.New("").Funcs(s.funcs()).ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	s.tmpl = t
	if err := s.applyAccess(s.loadAccess()); err != nil {
		log.Printf("WARNING: proxy settings ignored: %v", err)
	}
	if cfg.BrowserFetch != "off" {
		if bin, err := browserlogin.FindBrowser(cfg.Browser); err == nil {
			s.chain.SetBrowser(&browserlogin.Downloader{Binary: bin, ProfileDir: filepath.Join(st.Dir(), "browser-profile"), Cookies: s.browserCookies}, func() string {
				if px := s.chain.Proxy(); px != nil && px.HasCookie() {
					return px.Prefix()
				}
				return ""
			})
			log.Printf("PDF chain: headless browser stage enabled (%s)", filepath.Base(bin))
		}
	}
	return s, nil
}

// newAI picks the Claude backend: an API key, or the logged-in claude CLI
// (subscription auth) in headless mode.
func newAI(cfg config.Config) *ai.Client {
	cli := func() *ai.Client {
		if path, ok := ai.LookupCLI(cfg.ClaudeCLIPath); ok {
			log.Printf("AI backend: claude CLI in headless mode (%s, model: %s)", path, orDefault(cfg.ClaudeCLIModel, "sonnet"))
			return ai.New(&ai.CLIBackend{Path: path, Model: cfg.ClaudeCLIModel})
		}
		return nil
	}
	switch cfg.AIBackend {
	case "api":
		return ai.NewAPI(cfg.AnthropicAPIKey, cfg.AnthropicModel)
	case "claude-cli":
		if c := cli(); c != nil {
			return c
		}
		log.Printf("WARNING: AI_BACKEND=claude-cli but %q was not found on PATH; AI features disabled", cfg.ClaudeCLIPath)
		return ai.New(nil)
	default: // auto
		if cfg.AnthropicAPIKey != "" {
			log.Printf("AI backend: Anthropic API (model: %s)", cfg.AnthropicModel)
			return ai.NewAPI(cfg.AnthropicAPIKey, cfg.AnthropicModel)
		}
		if c := cli(); c != nil {
			return c
		}
		log.Print("WARNING: no AI backend: set ANTHROPIC_API_KEY or install and log in to the claude CLI; AI features disabled")
		return ai.New(nil)
	}
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// Handler returns the HTTP mux.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(static))))

	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("POST /projects", s.handleCreate)
	mux.HandleFunc("GET /projects/{id}", s.handleProject)
	mux.HandleFunc("POST /projects/{id}/delete", s.handleDelete)
	mux.HandleFunc("POST /projects/{id}/settings", s.handleSettings)
	mux.HandleFunc("POST /projects/{id}/queries", s.handleQueries)
	mux.HandleFunc("POST /projects/{id}/reset", s.handleReset)
	mux.HandleFunc("POST /projects/{id}/search", s.handleSearch)
	mux.HandleFunc("POST /projects/{id}/cancel", s.handleCancel)
	mux.HandleFunc("GET /projects/{id}/job", s.handleJobPartial)
	mux.HandleFunc("GET /projects/{id}/downloads", s.handleDownloadsPartial)
	mux.HandleFunc("POST /projects/{id}/papers/{pid}/decision", s.handleDecision)
	mux.HandleFunc("POST /projects/{id}/bulk", s.handleBulk)
	mux.HandleFunc("POST /projects/{id}/undo", s.handleUndo)
	mux.HandleFunc("POST /projects/{id}/redo", s.handleRedo)
	mux.HandleFunc("POST /projects/{id}/adjudicate", s.handleAdjudicate)
	mux.HandleFunc("POST /projects/{id}/download", s.handleDownload)
	mux.HandleFunc("POST /projects/{id}/papers/{pid}/retry", s.handleRetry)
	mux.HandleFunc("POST /projects/{id}/synthesize", s.handleSynthesize)
	mux.HandleFunc("GET /projects/{id}/export.bib", s.handleExport)
	mux.HandleFunc("GET /projects/{id}/export.json", s.handleExport)
	mux.HandleFunc("GET /projects/{id}/synthesis.md", s.handleSynthesisExport)
	mux.HandleFunc("GET /projects/{id}/synthesis.tex", s.handleSynthesisExport)
	mux.HandleFunc("POST /projects/{id}/venues", s.handleVenues)
	mux.HandleFunc("POST /access/scimago", s.handleScimagoUpload)
	mux.HandleFunc("GET /projects/{id}/concepts", s.handleConcepts)
	mux.HandleFunc("POST /projects/{id}/concepts/build", s.handleConceptsBuild)
	mux.HandleFunc("GET /projects/{id}/concepts.csv", s.handleConceptsExport)
	mux.HandleFunc("GET /projects/{id}/concepts.tex", s.handleConceptsLaTeX)
	mux.HandleFunc("GET /projects/{id}/table.tex", s.handleTableLaTeX)
	mux.HandleFunc("GET /projects/{id}/concepts.xlsx", s.handleConceptsExport)
	mux.HandleFunc("GET /projects/{id}/prisma", s.handlePrisma)
	mux.HandleFunc("GET /projects/{id}/prisma.svg", s.handlePrisma)
	mux.HandleFunc("GET /projects/{id}/zotero", s.handleZotero)
	mux.HandleFunc("POST /projects/{id}/zotero/key", s.handleZoteroKey)
	mux.HandleFunc("POST /projects/{id}/zotero/send", s.handleZoteroSend)
	mux.HandleFunc("GET /projects/{id}/keywords", s.handleKeywords)
	mux.HandleFunc("POST /projects/{id}/keywords/enrich", s.handleKeywordsEnrich)
	mux.HandleFunc("GET /projects/{id}/keywords/vosviewer-map.txt", s.handleKeywordsExport)
	mux.HandleFunc("GET /projects/{id}/keywords/vosviewer-network.txt", s.handleKeywordsExport)
	mux.HandleFunc("GET /projects/{id}/keywords/keywords.csv", s.handleKeywordsExport)
	mux.HandleFunc("GET /projects/{id}/table", s.handleTable)
	mux.HandleFunc("POST /projects/{id}/extract", s.handleExtract)
	mux.HandleFunc("GET /projects/{id}/table.csv", s.handleTableExport)
	mux.HandleFunc("GET /projects/{id}/table.xlsx", s.handleTableExport)
	mux.HandleFunc("GET /access", s.handleAccess)
	mux.HandleFunc("POST /access", s.handleAccessSave)
	mux.HandleFunc("POST /access/test", s.handleAccessTest)
	mux.HandleFunc("POST /access/browser", s.handleAccessBrowser)
	mux.HandleFunc("POST /access/browser/cancel", s.handleAccessBrowserCancel)
	mux.HandleFunc("GET /access/status", s.handleAccessStatus)
	mux.HandleFunc("POST /projects/{id}/papers/{pid}/upload", s.handleUpload)
	mux.HandleFunc("POST /projects/{id}/retry-all", s.handleRetryAll)
	mux.HandleFunc("GET /files/{id}/{name}", s.handleFile)
	return logRequests(mux)
}

func logRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		h.ServeHTTP(w, r)
		if !strings.HasPrefix(r.URL.Path, "/static/") && !strings.HasSuffix(r.URL.Path, "/job") && !strings.HasSuffix(r.URL.Path, "/downloads") && !strings.HasSuffix(r.URL.Path, "/status") {
			log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
		}
	})
}

// --- template helpers --------------------------------------------------------

func (s *Server) funcs() template.FuncMap {
	return template.FuncMap{
		"join":  strings.Join,
		"add":   func(a, b int) int { return a + b },
		"list":  func(items ...string) []string { return items },
		"split": strings.Split,
		"mulf":  func(a, b float64) float64 { return a * b },
		"dict": func(kv ...any) (map[string]any, error) {
			if len(kv)%2 != 0 {
				return nil, errors.New("dict needs key/value pairs")
			}
			m := map[string]any{}
			for i := 0; i < len(kv); i += 2 {
				k, ok := kv[i].(string)
				if !ok {
					return nil, errors.New("dict keys must be strings")
				}
				m[k] = kv[i+1]
			}
			return m, nil
		},
		"authors": func(a []string) string {
			if len(a) == 0 {
				return "Unknown authors"
			}
			if len(a) > 4 {
				return strings.Join(a[:3], ", ") + " et al."
			}
			return strings.Join(a, ", ")
		},
		"scoreClass": func(n int) string {
			switch {
			case n >= 70:
				return "hi"
			case n >= 40:
				return "mid"
			default:
				return "lo"
			}
		},
		"sourceLabel": func(name string) string {
			if src, ok := s.reg.Get(name); ok {
				return src.Label()
			}
			return name
		},
		"basename": filepath.Base,
		"fmtTime": func(t time.Time) string {
			if t.IsZero() {
				return ""
			}
			return t.Local().Format("2006-01-02 15:04")
		},
		"pct": func(done, total int) int {
			if total == 0 {
				return 0
			}
			return done * 100 / total
		},
		"kb": func(b int64) string { return fmt.Sprintf("%d KB", b/1024) },
		"elapsed": func(t time.Time) string {
			if t.IsZero() {
				return ""
			}
			d := time.Since(t).Round(time.Second)
			if d < time.Minute {
				return fmt.Sprintf("%ds", int(d.Seconds()))
			}
			return fmt.Sprintf("%dm %02ds", int(d.Minutes()), int(d.Seconds())%60)
		},
		"markdown":   renderMarkdown,
		"stageIndex": model.StageIndex,
		"seq": func(n int) []int {
			out := make([]int, n)
			for i := range out {
				out[i] = i
			}
			return out
		},
	}
}

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("template %s: %v", name, err)
		http.Error(w, "template error: "+err.Error(), 500)
	}
}

type sourceView struct {
	Name, Label, Syntax string
	Checked             bool
}

func (s *Server) sourceViews(selected []string) []sourceView {
	sel := map[string]bool{}
	for _, n := range selected {
		sel[n] = true
	}
	var out []sourceView
	for _, src := range s.reg.All() {
		out = append(out, sourceView{Name: src.Name(), Label: src.Label(), Syntax: src.Syntax(), Checked: len(selected) == 0 || sel[src.Name()]})
	}
	return out
}

type baseData struct {
	AIEnabled      bool
	AIBackend      string
	Model          string
	JevEnabled     bool
	JevModel       string
	Screener       string
	SynthesisModel string
	EmailSet       bool
	ProxySet       bool
	ProxyCookie    bool
	ProxyPrefix    string
}

func (s *Server) base() baseData {
	return baseData{
		AIEnabled:      s.ai.Enabled(),
		AIBackend:      s.ai.BackendName(),
		Model:          s.ai.ModelLabel(),
		JevEnabled:     s.useJev(),
		SynthesisModel: s.cfg.SynthesisModel,
		JevModel:       s.cfg.JevModel,
		Screener:       s.cfg.Screener,
		EmailSet:       s.cfg.ContactEmail != "",
		ProxySet:       s.chain.Proxy() != nil,
		ProxyCookie:    s.chain.Proxy() != nil && s.chain.Proxy().HasCookie(),
		ProxyPrefix: func() string {
			if px := s.chain.Proxy(); px != nil {
				return px.Prefix()
			}
			return ""
		}(),
	}
}

// --- handlers: projects -------------------------------------------------------

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	s.render(w, "index", map[string]any{
		"Base":     s.base(),
		"Projects": s.store.List(),
		"Sources":  s.sourceViews(nil),
		"YearTo":   time.Now().Year(),
		"YearFrom": time.Now().Year() - 10,
	})
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	title := strings.TrimSpace(r.FormValue("title"))
	question := strings.TrimSpace(r.FormValue("question"))
	if question == "" {
		http.Error(w, "research question is required", 400)
		return
	}
	if title == "" {
		title = question
		if len(title) > 80 {
			title = title[:80] + "…"
		}
	}
	yearFrom, _ := strconv.Atoi(r.FormValue("year_from"))
	yearTo, _ := strconv.Atoi(r.FormValue("year_to"))
	maxPer, _ := strconv.Atoi(r.FormValue("max_per_source"))
	if maxPer <= 0 {
		maxPer = 25
	}
	target, _ := strconv.Atoi(r.FormValue("target"))
	if target < 0 {
		target = 0
	}
	if maxPer > 200 {
		maxPer = 200
	}
	var srcs []string
	for _, n := range r.Form["sources"] {
		if _, ok := s.reg.Get(n); ok {
			srcs = append(srcs, n)
		}
	}
	if len(srcs) == 0 {
		http.Error(w, "select at least one database", 400)
		return
	}
	now := time.Now()
	p := &model.Project{
		ID:               store.NewID(),
		Title:            title,
		Question:         question,
		Inclusion:        strings.TrimSpace(r.FormValue("inclusion")),
		Exclusion:        strings.TrimSpace(r.FormValue("exclusion")),
		Discipline:       strings.TrimSpace(r.FormValue("discipline")),
		PeerReviewedOnly: r.FormValue("peer_reviewed_only") != "0",
		EnglishOnly:      r.FormValue("english_only") != "0",
		YearFrom:         yearFrom,
		YearTo:           yearTo,
		MaxPerSource:     maxPer,
		Target:           target,
		Sources:          srcs,
		Stage:            model.StageSetup,
		Papers:           []*model.Paper{},
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	if err := s.store.Put(p); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if r.FormValue("start") == "1" {
		if err := s.startSearch(p.ID, "generate"); err != nil {
			http.Redirect(w, r, "/projects/"+p.ID+"?view=search&error="+urlEsc(err.Error()), http.StatusSeeOther)
			return
		}
	}
	http.Redirect(w, r, "/projects/"+p.ID, http.StatusSeeOther)
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.cancelJob(id)
	if err := s.store.Delete(id); err != nil && !errors.Is(err, store.ErrNotFound) {
		http.Error(w, err.Error(), 500)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

type projectData struct {
	Base      baseData
	P         *model.Project
	View      string
	Papers    []*model.Paper
	Filter    string
	Keep      int
	Drop      int
	Undecided int
	Chain     []string
	Sources   []sourceView
	Error     string
	Message   string
	// navigation flags
	HasPapers     bool
	HasKept       bool
	HasQueries    bool
	StageIdx      int
	JobRunning    bool
	JobName       string
	PendingPDFs   int // kept papers without a PDF yet
	QueryText     map[string]string
	Extracted     int
	ProxyMissing  bool
	Conflicts     int
	FullTextCount int
	Preprints     int
	NonEnglish    int
	UndoLabel     string
	RedoLabel     string
	FailedSources []string // one line per database whose queries all failed in the last round
	Rated         int      // papers with a venue rating
	Jufo0         int      // tier "below" and not dropped
	UnknownVenue  int      // tier "unknown" and not dropped
	Unrated       int      // kept papers without a rating
}

func (s *Server) projectData(p *model.Project, filter string) projectData {
	keep, drop, und := p.Counts()
	var papers []*model.Paper
	for _, pp := range p.Papers {
		switch filter {
		case "keep":
			if pp.Decision != model.DecisionKeep {
				continue
			}
		case "drop":
			if pp.Decision != model.DecisionDrop {
				continue
			}
		case "undecided":
			if pp.Decision != model.DecisionUndecided {
				continue
			}
		case "conflict":
			if pp.Agreement() != "conflict" {
				continue
			}
		case "quality":
			if pp.VenueRating == nil || (pp.VenueRating.Tier != "top" && pp.VenueRating.Tier != "good" && pp.VenueRating.Tier != "basic") {
				continue
			}
		case "jufo0":
			if pp.VenueRating == nil || pp.VenueRating.Tier != "below" {
				continue
			}
		case "unknownvenue":
			if pp.VenueRating == nil || pp.VenueRating.Tier != "unknown" {
				continue
			}
		}
		papers = append(papers, pp)
	}
	d := projectData{
		Base: s.base(), P: p, Papers: papers, Filter: filter,
		Keep: keep, Drop: drop, Undecided: und,
		Chain:      s.chain.Names(),
		Sources:    s.sourceViews(p.Sources),
		HasPapers:  len(p.Papers) > 0,
		HasKept:    keep > 0,
		HasQueries: len(p.Queries) > 0,
		StageIdx:   model.StageIndex(p.Stage),
		JobRunning: p.Job != nil && p.Job.Running,
		QueryText:  map[string]string{},
	}
	if d.JobRunning {
		d.JobName = p.Job.Name
	}
	for _, pp := range p.Kept() {
		if pp.Download.Status != model.DownloadOK {
			d.PendingPDFs++
		}
		if pp.Extract != nil {
			d.Extracted++
		}
		if pp.TextPath != "" {
			d.FullTextCount++
		}
	}
	for _, q := range p.Queries {
		d.QueryText[q.Source] += q.Query + "\n"
	}
	// Per-source failure summary for the latest round.
	lastRound := 0
	for _, q := range p.Queries {
		if q.Round > lastRound {
			lastRound = q.Round
		}
	}
	type stat struct {
		total, failed int
		err           string
	}
	bySrc := map[string]*stat{}
	var order []string
	for _, q := range p.Queries {
		if q.Round != lastRound || q.URL == "" && q.Error == "" {
			continue
		}
		st, ok := bySrc[q.Source]
		if !ok {
			st = &stat{}
			bySrc[q.Source] = st
			order = append(order, q.Source)
		}
		st.total++
		if q.Error != "" {
			st.failed++
			st.err = q.Error
		}
	}
	for _, src := range order {
		st := bySrc[src]
		if st.total > 0 && st.failed == st.total {
			label := src
			if sv, ok := s.reg.Get(src); ok {
				label = sv.Label()
			}
			d.FailedSources = append(d.FailedSources, fmt.Sprintf("%s: all %d queries failed — %s", label, st.total, shortTitle(st.err)))
		}
	}
	for _, pp := range p.Papers {
		if pp.Agreement() == "conflict" {
			d.Conflicts++
		}
		if sources.IsPreprint(pp) && pp.Decision != model.DecisionDrop {
			d.Preprints++
		}
		if !sources.IsEnglish(pp) && pp.Decision != model.DecisionDrop {
			d.NonEnglish++
		}
		if pp.VenueRating != nil {
			d.Rated++
			if pp.VenueRating.Tier == "below" && pp.Decision != model.DecisionDrop {
				d.Jufo0++
			}
			if pp.VenueRating.Tier == "unknown" && pp.Decision != model.DecisionDrop {
				d.UnknownVenue++
			}
		} else if pp.Decision == model.DecisionKeep {
			d.Unrated++
		}
	}
	return d
}

// defaultView picks the view to show for the project's state.
func defaultView(p *model.Project) string {
	if p.Job != nil && p.Job.Running {
		switch p.Job.Name {
		case "search":
			return "search"
		case "download", "retry":
			return "downloads"
		case "synthesis", "extract":
			return "results"
		case "adjudicate":
			return "screening"
		case "keywords", "zotero", "concepts":
			return "results"
		case "venues":
			return "screening"
		}
	}
	switch p.Stage {
	case model.StageSetup:
		return "setup"
	case model.StageSearching:
		return "search"
	case model.StageScreening:
		return "screening"
	case model.StageDownloading:
		return "downloads"
	default:
		return "results"
	}
}

func (s *Server) handleProject(w http.ResponseWriter, r *http.Request) {
	p, err := s.store.Get(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	filter := r.URL.Query().Get("filter")
	if filter == "" {
		filter = "all"
	}
	view := r.URL.Query().Get("view")
	var data projectData
	_ = s.store.View(p.ID, func(p *model.Project) {
		if view == "" {
			view = defaultView(p)
		}
		// Guard views whose prerequisites are missing.
		keep, _, _ := p.Counts()
		switch view {
		case "screening":
			if len(p.Papers) == 0 {
				view = "search"
			}
		case "downloads":
			if keep == 0 {
				view = "screening"
			}
		case "results":
			if model.StageIndex(p.Stage) < 3 {
				view = "downloads"
			}
		case "setup", "search":
		default:
			view = defaultView(p)
		}
		if view == "downloads" {
			filter = "keep"
		}
		data = s.projectData(p, filter)
		data.View = view
	})
	data.UndoLabel, data.RedoLabel = s.store.Labels(p.ID)
	data.Error = r.URL.Query().Get("error")
	data.Message = r.URL.Query().Get("msg")
	s.render(w, "project", data)
}

func (s *Server) handleUndo(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	label, err := s.store.Undo(id)
	back := "/projects/" + id + "?view=" + urlEsc(r.FormValue("view"))
	if err != nil {
		http.Redirect(w, r, back+"&error="+urlEsc(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, back+"&msg="+urlEsc("Undone: "+label), http.StatusSeeOther)
}

func (s *Server) handleRedo(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	label, err := s.store.Redo(id)
	back := "/projects/" + id + "?view=" + urlEsc(r.FormValue("view"))
	if err != nil {
		http.Redirect(w, r, back+"&error="+urlEsc(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, back+"&msg="+urlEsc("Redone: "+label), http.StatusSeeOther)
}

// --- settings, queries, reset -----------------------------------------------------

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	var srcs []string
	for _, n := range r.Form["sources"] {
		if _, ok := s.reg.Get(n); ok {
			srcs = append(srcs, n)
		}
	}
	_ = s.store.SaveUndo(id, "Change settings")
	err := s.store.Update(id, func(p *model.Project) error {
		if p.Job != nil && p.Job.Running {
			return errors.New("wait for the running job to finish before changing settings")
		}
		q := strings.TrimSpace(r.FormValue("question"))
		if q == "" {
			return errors.New("research question is required")
		}
		if len(srcs) == 0 {
			return errors.New("select at least one database")
		}
		p.Question = q
		if t := strings.TrimSpace(r.FormValue("title")); t != "" {
			p.Title = t
		}
		p.Inclusion = strings.TrimSpace(r.FormValue("inclusion"))
		p.Exclusion = strings.TrimSpace(r.FormValue("exclusion"))
		p.Discipline = strings.TrimSpace(r.FormValue("discipline"))
		p.PeerReviewedOnly = r.FormValue("peer_reviewed_only") == "1"
		p.EnglishOnly = r.FormValue("english_only") == "1"
		p.YearFrom, _ = strconv.Atoi(r.FormValue("year_from"))
		p.YearTo, _ = strconv.Atoi(r.FormValue("year_to"))
		if n, _ := strconv.Atoi(r.FormValue("max_per_source")); n > 0 {
			if n > 200 {
				n = 200
			}
			p.MaxPerSource = n
		}
		p.Sources = srcs
		if t, err := strconv.Atoi(r.FormValue("target")); err == nil && t >= 0 {
			p.Target = t
		}
		return nil
	})
	if err != nil {
		http.Redirect(w, r, "/projects/"+id+"?view=setup&error="+urlEsc(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/projects/"+id+"?view=search", http.StatusSeeOther)
}

// handleQueries saves hand-edited queries: one textarea per source, one
// query per line. Rationale is kept for unchanged query strings.
func (s *Server) handleQueries(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	_ = s.store.SaveUndo(id, "Edit queries")
	err := s.store.Update(id, func(p *model.Project) error {
		if p.Job != nil && p.Job.Running {
			return errors.New("wait for the running job to finish before editing queries")
		}
		old := map[string]model.Query{}
		for _, q := range p.Queries {
			old[q.Source+"\x00"+q.Query] = q
		}
		var out []model.Query
		for _, src := range p.Sources {
			for _, line := range strings.Split(r.FormValue("q_"+src), "\n") {
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}
				if prev, ok := old[src+"\x00"+line]; ok {
					out = append(out, prev)
				} else {
					out = append(out, model.Query{Source: src, Query: line, Rationale: "edited by hand"})
				}
			}
		}
		p.Queries = out
		return nil
	})
	if err != nil {
		http.Redirect(w, r, "/projects/"+id+"?view=search&error="+urlEsc(err.Error()), http.StatusSeeOther)
		return
	}
	if r.FormValue("run") == "1" {
		if err := s.startSearch(id, "run"); err != nil {
			http.Redirect(w, r, "/projects/"+id+"?view=search&error="+urlEsc(err.Error()), http.StatusSeeOther)
			return
		}
	}
	http.Redirect(w, r, "/projects/"+id+"?view=search", http.StatusSeeOther)
}

// handleReset clears results (papers, queries, synthesis) but keeps settings.
func (s *Server) handleReset(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.cancelJob(id)
	_ = s.store.SaveUndo(id, "Clear all results")
	err := s.store.Update(id, func(p *model.Project) error {
		p.Papers = []*model.Paper{}
		p.Queries = nil
		p.Synthesis = ""
		p.Job = nil
		p.Stage = model.StageSetup
		return nil
	})
	if err != nil {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "/projects/"+id+"?view=setup", http.StatusSeeOther)
}

// --- handlers: jobs -----------------------------------------------------------

type jobLogger struct {
	s  *Server
	id string
}

func (l jobLogger) log(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	log.Printf("[%s] %s", l.id, msg)
	_ = l.s.store.Update(l.id, func(p *model.Project) error {
		if p.Job != nil {
			p.Job.Message = msg
			p.Job.Log = append(p.Job.Log, time.Now().Format("15:04:05")+"  "+msg)
			if len(p.Job.Log) > 400 {
				p.Job.Log = p.Job.Log[len(p.Job.Log)-400:]
			}
		}
		return nil
	})
}

// phase sets the current phase label and resets the bar. total==0 shows an
// indeterminate bar.
func (l jobLogger) phase(num, of int, label string, total int) {
	_ = l.s.store.Update(l.id, func(p *model.Project) error {
		if p.Job != nil {
			p.Job.Phase, p.Job.PhaseNum, p.Job.PhaseTotal = label, num, of
			p.Job.Progress, p.Job.Total = 0, total
		}
		return nil
	})
}

func (l jobLogger) progress(done, total int) {
	_ = l.s.store.Update(l.id, func(p *model.Project) error {
		if p.Job != nil {
			p.Job.Progress, p.Job.Total = done, total
		}
		return nil
	})
}

// runJob starts fn in the background as the project's single active job.
func (s *Server) runJob(id, name string, fn func(ctx context.Context, lg jobLogger) error) error {
	s.jobMu.Lock()
	defer s.jobMu.Unlock()
	if _, busy := s.jobs[id]; busy {
		return errors.New("a job is already running for this project")
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.jobs[id] = cancel
	if err := s.store.Update(id, func(p *model.Project) error {
		p.Job = &model.JobStatus{Name: name, Running: true, StartedAt: time.Now()}
		return nil
	}); err != nil {
		cancel()
		delete(s.jobs, id)
		return err
	}
	go func() {
		defer func() {
			cancel()
			s.jobMu.Lock()
			delete(s.jobs, id)
			s.jobMu.Unlock()
		}()
		err := fn(ctx, jobLogger{s: s, id: id})
		_ = s.store.Update(id, func(p *model.Project) error {
			if p.Job != nil {
				p.Job.Running = false
				p.Job.FinishedAt = time.Now()
				if err != nil {
					p.Job.Error = err.Error()
					p.Job.Message = "Failed: " + err.Error()
				} else {
					p.Job.Message = "Done"
				}
			}
			return nil
		})
		if err != nil {
			log.Printf("[%s] job %s failed: %v", id, name, err)
		}
	}()
	return nil
}

func (s *Server) cancelJob(id string) {
	s.jobMu.Lock()
	cancel, ok := s.jobs[id]
	s.jobMu.Unlock()
	if ok {
		cancel()
	}
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.cancelJob(id)
	http.Redirect(w, r, "/projects/"+id, http.StatusSeeOther)
}

func (s *Server) handleJobPartial(w http.ResponseWriter, r *http.Request) {
	p, err := s.store.Get(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var running bool
	_ = s.store.View(p.ID, func(p *model.Project) { running = p.Job != nil && p.Job.Running })
	if !running {
		w.Header().Set("HX-Refresh", "true")
	}
	_ = s.store.View(p.ID, func(p *model.Project) { s.render(w, "job", map[string]any{"P": p}) })
}

func (s *Server) handleDownloadsPartial(w http.ResponseWriter, r *http.Request) {
	p, err := s.store.Get(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var running bool
	_ = s.store.View(p.ID, func(p *model.Project) { running = p.Job != nil && p.Job.Running })
	if !running {
		w.Header().Set("HX-Refresh", "true")
	}
	_ = s.store.View(p.ID, func(p *model.Project) { s.render(w, "downloads", s.projectData(p, "keep")) })
}

// --- search job ---------------------------------------------------------------

// Search modes:
//
//	generate  – ask Claude for fresh queries, run them, screen new papers
//	run       – run the current (possibly hand-edited) queries, screen new papers
//	rescreen  – re-run AI screening on all papers; user decisions are kept
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	mode := r.FormValue("mode")
	if mode == "" {
		mode = "generate"
	}
	if err := s.startSearch(id, mode); err != nil {
		http.Redirect(w, r, "/projects/"+id+"?view=search&error="+urlEsc(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/projects/"+id+"?view=search", http.StatusSeeOther)
}

func (s *Server) startSearch(id, mode string) error {
	if !s.ai.Enabled() && mode == "generate" {
		return errors.New("no AI backend configured (set ANTHROPIC_API_KEY, or install and log in to the claude CLI); queries cannot be generated")
	}
	if !s.useClaude() && !s.useJev() {
		return errors.New("no screener available: configure Claude (ANTHROPIC_API_KEY or the claude CLI) and/or Jev (JEV_API_KEY)")
	}
	snap, err := s.snapshot(id)
	if err != nil {
		return err
	}
	switch mode {
	case "run":
		if len(snap.Queries) == 0 {
			return errors.New("there are no queries yet; generate them with AI or write them by hand first")
		}
	case "rescreen":
		if len(snap.Papers) == 0 {
			return errors.New("there are no papers to re-screen")
		}
	case "expand":
		if !s.ai.Enabled() {
			return errors.New("expanding the search needs Claude to design new queries")
		}
		if len(snap.Papers) == 0 {
			return errors.New("run an initial search before expanding it")
		}
	case "snowball":
		if _, ok := s.reg.SnowballerFor(snap.Sources); !ok {
			return errors.New("snowballing needs OpenAlex in the registry")
		}
		if len(snap.Papers) == 0 {
			return errors.New("run a search first; snowballing follows the citations of papers you keep")
		}
	case "generate":
	default:
		return fmt.Errorf("unknown search mode %q", mode)
	}
	_ = s.store.SaveUndo(id, "Search ("+mode+")")
	if err := s.store.Update(id, func(p *model.Project) error {
		if p.Stage == model.StageSetup {
			p.Stage = model.StageSearching
		}
		return nil
	}); err != nil {
		return err
	}
	return s.runJob(id, "search", func(ctx context.Context, lg jobLogger) error {
		var err error
		switch mode {
		case "rescreen":
			err = s.rescreenJob(ctx, snap, lg)
		case "snowball":
			err = s.snowballJob(ctx, snap, lg)
		default:
			err = s.searchJob(ctx, snap, mode, lg)
		}
		_ = s.store.Update(id, func(p *model.Project) error {
			if p.Stage == model.StageSearching {
				if len(p.Papers) == 0 {
					p.Stage = model.StageSetup
				} else {
					p.Stage = model.StageScreening
				}
			}
			return nil
		})
		return err
	})
}

// useJev / useClaude reflect the SCREENER setting and what is configured.
func (s *Server) useJev() bool    { return s.jev.Enabled() && s.cfg.Screener != "claude" }
func (s *Server) useClaude() bool { return s.ai.Enabled() && s.cfg.Screener != "jev" }

// screen runs the configured screeners over papers and applies verdicts.
// phase numbering continues from phaseStart within phaseTotal.
func (s *Server) screen(ctx context.Context, p *model.Project, papers []*model.Paper, lg jobLogger, phaseStart, phaseTotal int, label string) error {
	ph := phaseStart
	if s.useClaude() {
		lg.phase(ph, phaseTotal, label+" (Claude)", len(papers))
		lg.log("Screening %d %s with Claude (%s, %s), %d batches of up to %d, 3 at a time…", len(papers), label, s.ai.BackendName(), s.ai.ModelLabel(), (len(papers)+19)/20, 20)
		if err := s.ai.ScreenPapers(ctx, p, papers, func(done, total int) {
			lg.progress(done, total)
			lg.log("Claude screened %d/%d", done, total)
		}); err != nil {
			return fmt.Errorf("screening: %w", err)
		}
		ph++
	}
	if s.useJev() {
		lg.phase(ph, phaseTotal, label+" (Jev second opinion)", len(papers))
		lg.log("Asking Jev (%s) for an independent assessment of %d %s, %d requests in parallel…", s.cfg.JevModel, len(papers), label, s.jev.Concurrency)
		step := len(papers) / 10
		if step < 1 {
			step = 1
		}
		if err := s.jev.AssessAll(ctx, p, papers, func(done, total int) {
			lg.progress(done, total)
			if done%step == 0 || done == total {
				lg.log("Jev assessed %d/%d", done, total)
			}
		}); err != nil {
			return err
		}
		failed := 0
		for _, pp := range papers {
			if pp.Jev != nil && pp.Jev.Error != "" {
				failed++
			}
		}
		if failed == len(papers) && len(papers) > 0 {
			return fmt.Errorf("jev: every request failed, e.g. %s", papers[0].Jev.Error)
		}
		if failed > 0 {
			lg.log("Jev failed for %d papers; Claude's verdict is used for those.", failed)
		}
	}
	for _, pp := range papers {
		pp.ApplyVerdict()
	}
	return nil
}

// snapshot copies a project (and its papers) so a job can work without
// holding the store lock; live decisions are re-applied when it writes back.
func (s *Server) snapshot(id string) (*model.Project, error) {
	var snap model.Project
	err := s.store.View(id, func(p *model.Project) {
		snap = *p
		snap.Papers = make([]*model.Paper, 0, len(p.Papers))
		for _, pp := range p.Papers {
			c := *pp
			snap.Papers = append(snap.Papers, &c)
		}
		snap.Queries = append([]model.Query{}, p.Queries...)
	})
	if err != nil {
		return nil, err
	}
	return &snap, nil
}

// writeBackPapers stores the job's paper list, preserving any decisions,
// downloads and extractions the user made while the job ran.
func (s *Server) writeBackPapers(id string, papers []*model.Paper, queries []model.Query) error {
	return s.store.Update(id, func(p *model.Project) error {
		for _, np := range papers {
			if live := p.Paper(np.ID); live != nil {
				if live.DecisionBy == "user" {
					np.Decision, np.DecisionBy = live.Decision, live.DecisionBy
				}
				if live.Download.Status != model.DownloadPending {
					np.Download = live.Download
				}
				if live.Extract != nil && np.Extract == nil {
					np.Extract = live.Extract
				}
			}
		}
		p.Papers = papers
		if queries != nil {
			p.Queries = queries
		}
		p.EnsureBibKeys()
		return nil
	})
}

// maxExpansions bounds automatic broadening rounds per search job.
const maxExpansions = 2

func (s *Server) sourceInfos(p *model.Project) ([]ai.SourceInfo, []sources.Source) {
	var infos []ai.SourceInfo
	var active []sources.Source
	for _, n := range p.Sources {
		if src, ok := s.reg.Get(n); ok {
			infos = append(infos, ai.SourceInfo{Name: src.Name(), Label: src.Label(), Syntax: src.Syntax()})
			active = append(active, src)
		}
	}
	return infos, active
}

// runQueries executes the given query indices (grouped per source so each
// adapter's rate limiter is respected), merges results into all and returns
// the merged list plus the papers that are new.
func (s *Server) runQueries(ctx context.Context, p *model.Project, active []sources.Source, queries []model.Query, idx []int, opts sources.Options, all []*model.Paper, lg jobLogger) ([]*model.Paper, []*model.Paper) {
	type result struct {
		idx int
		res *sources.Result
		err error
	}
	results := make(chan result)
	var wg sync.WaitGroup
	for _, src := range active {
		src := src
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, i := range idx {
				if queries[i].Source != src.Name() {
					continue
				}
				res, err := src.Search(ctx, queries[i].Query, opts)
				results <- result{idx: i, res: res, err: err}
			}
		}()
	}
	go func() { wg.Wait(); close(results) }()

	before := map[string]bool{}
	for _, pp := range all {
		before[pp.ID] = true
	}
	done := 0
	for res := range results {
		done++
		lg.progress(done, len(idx))
		q := &queries[res.idx]
		q.RanAt = time.Now()
		if res.res != nil {
			q.URL, q.Total = res.res.URL, res.res.Total
		}
		if res.err != nil {
			q.Error = res.err.Error()
			lg.log("%s: query %q failed: %v", q.Source, q.Query, res.err)
			continue
		}
		q.Results = len(res.res.Papers)
		papers := res.res.Papers
		retrieved := len(papers)
		var pre, nonEn []*model.Paper
		if p.PeerReviewedOnly {
			papers, pre = sources.FilterPreprints(papers)
		}
		if p.EnglishOnly {
			papers, nonEn = sources.FilterNonEnglish(papers)
		}
		sources.Tag(papers, q.Query)
		var added int
		all, added = sources.Merge(all, papers)
		dupes := len(papers) - added
		_ = s.store.Update(p.ID, func(pp *model.Project) error {
			pp.Stats.Retrieved += retrieved
			pp.Stats.Preprints += len(pre)
			pp.Stats.NonEnglish += len(nonEn)
			pp.Stats.Duplicates += dupes
			return nil
		})
		note := ""
		if len(pre) > 0 {
			note += fmt.Sprintf(", %d preprints excluded", len(pre))
		}
		if len(nonEn) > 0 {
			note += fmt.Sprintf(", %d non-English excluded", len(nonEn))
		}
		lg.log("%s: %d hits reported, %d retrieved for %q (%d new unique%s)", q.Source, q.Total, q.Results, q.Query, added, note)
		_ = s.store.Update(p.ID, func(pp *model.Project) error { pp.Queries = append([]model.Query{}, queries...); return nil })
	}
	var fresh []*model.Paper
	for _, pp := range all {
		if !before[pp.ID] {
			fresh = append(fresh, pp)
		}
	}
	return all, fresh
}

// queryYield fills Query.Relevant: papers surfaced by the query ranking >= 50.
func queryYield(queries []model.Query, all []*model.Paper) {
	for i := range queries {
		n := 0
		for _, pp := range all {
			if pp.RankScore() < 50 {
				continue
			}
			for _, fb := range pp.FoundBy {
				if fb == queries[i].Query {
					n++
					break
				}
			}
		}
		queries[i].Relevant = n
	}
}

func keptCount(ps []*model.Paper) int {
	n := 0
	for _, pp := range ps {
		if pp.Decision == model.DecisionKeep {
			n++
		}
	}
	return n
}

func (s *Server) screenerPhases() int {
	n := 0
	if s.useClaude() {
		n++
	}
	if s.useJev() {
		n++
	}
	return n
}

// searchJob runs one or more search rounds. Modes: generate (fresh AI
// queries), run (existing queries), expand (AI broadens). With a target set,
// rounds continue in expand mode until the target is met or maxExpansions.
func (s *Server) searchJob(ctx context.Context, p *model.Project, mode string, lg jobLogger) error {
	infos, active := s.sourceInfos(p)
	if len(active) == 0 {
		return errors.New("no valid sources selected")
	}
	opts := sources.Options{YearFrom: p.YearFrom, YearTo: p.YearTo, Limit: p.MaxPerSource}
	queries := p.Queries
	all := p.Papers
	round := p.Rounds
	if round == 0 && len(queries) > 0 { // projects from before round tracking
		round = 1
		for i := range queries {
			if queries[i].Round == 0 {
				queries[i].Round = 1
			}
		}
	}
	expansions := 0
	for {
		round++
		phases := 2 + s.screenerPhases()
		var idx []int
		switch mode {
		case "generate":
			lg.phase(1, phases, fmt.Sprintf("Round %d · designing search queries", round), 0)
			lg.log("Asking Claude (%s, %s) to design search queries for %d databases…", s.ai.BackendName(), s.ai.ModelLabel(), len(active))
			qs, err := s.ai.GenerateQueries(ctx, p, infos)
			if err != nil {
				return fmt.Errorf("query generation: %w", err)
			}
			for i := range qs {
				qs[i].Round = round
			}
			queries = qs
			lg.log("Generated %d queries.", len(queries))
			for i := range queries {
				idx = append(idx, i)
			}
		case "run":
			lg.log("Round %d · running %d existing queries (no AI query generation).", round, len(queries))
			for i := range queries {
				queries[i].Results, queries[i].Total, queries[i].Error, queries[i].URL = 0, 0, "", ""
				if queries[i].Round == 0 {
					queries[i].Round = round
				}
				idx = append(idx, i)
			}
		case "expand":
			lg.phase(1, phases, fmt.Sprintf("Round %d · broadening queries", round), 0)
			var stats []ai.QueryStat
			queryYield(queries, all)
			for _, q := range queries {
				stats = append(stats, ai.QueryStat{Source: q.Source, Query: q.Query, Total: q.Total, Retrieved: q.Results, Relevant: q.Relevant})
			}
			ranked := append([]*model.Paper{}, all...)
			sources.SortByScore(ranked)
			var good, bad []*model.Paper
			for _, pp := range ranked {
				if pp.RankScore() >= 55 {
					good = append(good, pp)
				}
			}
			for i := len(ranked) - 1; i >= 0 && len(bad) < 8; i-- {
				if ranked[i].RankScore() < 30 {
					bad = append(bad, ranked[i])
				}
			}
			lg.log("Asking Claude to broaden the search using the terminology of the %d best-ranked papers…", len(good))
			qs, err := s.ai.ExpandQueries(ctx, p, infos, stats, good, bad, keptCount(all), p.Target)
			if err != nil {
				return fmt.Errorf("query expansion: %w", err)
			}
			for i := range qs {
				qs[i].Round = round
				idx = append(idx, len(queries))
				queries = append(queries, qs[i])
			}
			lg.log("Added %d broader queries.", len(qs))
			// widen the net on expansion rounds
			opts.Limit = p.MaxPerSource * 2
			if opts.Limit > 100 {
				opts.Limit = 100
			}
		}
		_ = s.store.Update(p.ID, func(pp *model.Project) error {
			pp.Queries = append([]model.Query{}, queries...)
			pp.Rounds = round
			return nil
		})

		lg.phase(2, phases, fmt.Sprintf("Round %d · searching databases", round), len(idx))
		var fresh []*model.Paper
		all, fresh = s.runQueries(ctx, p, active, queries, idx, opts, all, lg)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		lg.log("Round %d: %d new unique papers (%d known).", round, len(fresh), len(all)-len(fresh))
		if len(all) == 0 {
			return errors.New("no results from any database; check the query log and try broader criteria")
		}
		if len(fresh) > 0 {
			if err := s.screen(ctx, p, fresh, lg, 3, phases, "new papers"); err != nil {
				queryYield(queries, all)
				_ = s.writeBackPapers(p.ID, all, queries)
				return fmt.Errorf("%w (new papers kept unscreened)", err)
			}
		}
		queryYield(queries, all)
		sources.SortByScore(all)
		// Re-apply live user decisions before counting toward the target.
		if err := s.writeBackPapers(p.ID, all, queries); err != nil {
			return err
		}
		var kept int
		_ = s.store.View(p.ID, func(pp *model.Project) { kept = keptCount(pp.Papers) })
		keep, drop, und, conflicts := 0, 0, 0, 0
		for _, pp := range fresh {
			switch pp.FinalVerdict() {
			case model.VerdictKeep:
				keep++
			case model.VerdictDrop:
				drop++
			default:
				und++
			}
			if pp.Agreement() == "conflict" {
				conflicts++
			}
		}
		lg.log("Round %d screening: %d keep, %d drop, %d unsure among new papers%s. Total included so far: %d.", round, keep, drop, und, conflictNote(conflicts), kept)

		switch {
		case mode == "run":
			return nil
		case p.Target <= 0 || kept >= p.Target:
			if p.Target > 0 {
				lg.log("Target of %d included papers reached.", p.Target)
			}
			return nil
		case expansions >= maxExpansions:
			lg.log("Still %d of target %d after %d expansion rounds. Options: add inclusion criteria that translate the question's terminology, expand again, snowball from kept papers, or lower the target.", kept, p.Target, expansions)
			return nil
		case mode == "expand" && len(fresh) == 0:
			lg.log("Expansion found nothing new; stopping.")
			return nil
		case !s.ai.Enabled():
			return nil
		}
		expansions++
		lg.log("Only %d of target %d included; broadening the search (expansion %d of %d)…", kept, p.Target, expansions, maxExpansions)
		mode = "expand"
	}
}

// snowballJob follows references and citations of kept papers via OpenAlex
// and screens the new candidates.
func (s *Server) snowballJob(ctx context.Context, p *model.Project, lg jobLogger) error {
	sb, ok := s.reg.SnowballerFor(p.Sources)
	if !ok {
		return errors.New("no snowball-capable source")
	}
	seeds := p.Kept()
	seedKind := "kept"
	if len(seeds) == 0 {
		ranked := append([]*model.Paper{}, p.Papers...)
		sources.SortByScore(ranked)
		for _, pp := range ranked {
			if pp.RankScore() >= 50 && len(seeds) < 10 {
				seeds = append(seeds, pp)
			}
		}
		seedKind = "top-ranked"
	}
	if len(seeds) > 25 {
		ranked := append([]*model.Paper{}, seeds...)
		sources.SortByScore(ranked)
		seeds = ranked[:25]
	}
	if len(seeds) == 0 {
		return errors.New("no seed papers: keep some papers (or get some ranked ≥ 50) first")
	}
	round := p.Rounds + 1
	if p.Rounds == 0 && len(p.Queries) > 0 {
		round = 2
	}
	phases := 2 + s.screenerPhases()
	lg.phase(1, phases, fmt.Sprintf("Round %d · snowballing", round), 0)
	lg.log("Snowballing from %d %s papers via OpenAlex: references (backward) and citing works (forward)…", len(seeds), seedKind)
	opts := sources.Options{YearFrom: p.YearFrom, YearTo: p.YearTo, Limit: p.MaxPerSource}
	backward, forward, err := sb.Snowball(ctx, seeds, opts)
	if err != nil && len(backward)+len(forward) == 0 {
		return fmt.Errorf("snowball: %w", err)
	}
	if err != nil {
		lg.log("Snowball partially failed: %v", err)
	}
	sources.Tag(backward, "snowball: references of "+seedKind+" papers")
	lg.log("Found %d referenced works and %d citing works.", len(backward), len(forward))
	cap := p.Target * 10
	if cap < 200 {
		cap = 200
	}
	cands := append(backward, forward...)
	if p.PeerReviewedOnly {
		var pre []*model.Paper
		cands, pre = sources.FilterPreprints(cands)
		if len(pre) > 0 {
			lg.log("%d preprint/repository records excluded.", len(pre))
		}
	}
	if p.EnglishOnly {
		var nonEn []*model.Paper
		cands, nonEn = sources.FilterNonEnglish(cands)
		if len(nonEn) > 0 {
			lg.log("%d non-English records excluded.", len(nonEn))
		}
	}
	if len(cands) > cap {
		sort.SliceStable(cands, func(i, j int) bool { return cands[i].Citations > cands[j].Citations })
		cands = cands[:cap]
		lg.log("Capped candidates to the %d most cited.", cap)
	}
	all := p.Papers
	before := map[string]bool{}
	for _, pp := range all {
		before[pp.ID] = true
	}
	snowRetrieved := len(backward) + len(forward)
	preN, nonEnN := snowRetrieved-len(cands), 0
	_ = nonEnN
	all, added := sources.Merge(all, cands)
	var fresh []*model.Paper
	for _, pp := range all {
		if !before[pp.ID] {
			fresh = append(fresh, pp)
		}
	}
	_ = s.store.Update(p.ID, func(pp *model.Project) error {
		pp.Stats.Snowballed += snowRetrieved
		pp.Stats.Preprints += preN
		pp.Stats.Duplicates += len(cands) - added
		return nil
	})
	lg.log("%d new unique candidates after de-duplication.", added)
	q := model.Query{Source: "openalex", Query: fmt.Sprintf("snowball from %d %s papers", len(seeds), seedKind), Rationale: "backward + forward citation chasing", Results: len(cands), Total: len(backward) + len(forward), RanAt: time.Now(), Round: round, URL: "https://api.openalex.org/works?filter=cites:…"}
	queries := append(append([]model.Query{}, p.Queries...), q)
	_ = s.store.Update(p.ID, func(pp *model.Project) error { pp.Queries = queries; pp.Rounds = round; return nil })
	if len(fresh) > 0 {
		if err := s.screen(ctx, p, fresh, lg, 2, phases, "snowball candidates"); err != nil {
			_ = s.writeBackPapers(p.ID, all, queries)
			return fmt.Errorf("%w (candidates kept unscreened)", err)
		}
	}
	queryYield(queries, all)
	sources.SortByScore(all)
	if err := s.writeBackPapers(p.ID, all, queries); err != nil {
		return err
	}
	keep, und := 0, 0
	for _, pp := range fresh {
		switch pp.FinalVerdict() {
		case model.VerdictKeep:
			keep++
		case model.VerdictUnsure:
			und++
		}
	}
	var kept int
	_ = s.store.View(p.ID, func(pp *model.Project) { kept = keptCount(pp.Papers) })
	lg.log("Snowball screening: %d keep, %d unsure among %d new candidates. Total included so far: %d.", keep, und, len(fresh), kept)
	return nil
}

// rescreenJob re-runs AI screening on every paper. Decisions made by the
// user are preserved; AI-made decisions follow the new verdicts.
func (s *Server) rescreenJob(ctx context.Context, p *model.Project, lg jobLogger) error {
	phases := 0
	if s.useClaude() {
		phases++
	}
	if s.useJev() {
		phases++
	}
	lg.log("Re-screening %d papers against the current question and criteria…", len(p.Papers))
	if err := s.screen(ctx, p, p.Papers, lg, 1, phases, "papers"); err != nil {
		return fmt.Errorf("re-%w", err)
	}
	kept, conflicts := 0, 0
	for _, pp := range p.Papers {
		if pp.Decision == model.DecisionKeep {
			kept++
		}
		if pp.Agreement() == "conflict" {
			conflicts++
		}
	}
	sources.SortByScore(p.Papers)
	lg.log("Re-screening complete; %d papers now marked keep (your manual decisions were kept)%s.", kept, conflictNote(conflicts))
	return s.writeBackPapers(p.ID, p.Papers, nil)
}

func conflictNote(n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("; %d screener conflicts left undecided for you", n)
}

// --- screening handlers -------------------------------------------------------

func (s *Server) handleDecision(w http.ResponseWriter, r *http.Request) {
	id, pid := r.PathValue("id"), r.PathValue("pid")
	dec := model.Decision(r.FormValue("decision"))
	if dec != model.DecisionKeep && dec != model.DecisionDrop {
		dec = model.DecisionUndecided
	}
	label := ""
	_ = s.store.View(id, func(p *model.Project) {
		if pp := p.Paper(pid); pp != nil {
			label = fmt.Sprintf("%s: %s", dec, shortTitle(pp.Title))
		}
	})
	if label != "" {
		_ = s.store.SaveUndo(id, label)
	}
	err := s.store.Update(id, func(p *model.Project) error {
		pp := p.Paper(pid)
		if pp == nil {
			return store.ErrNotFound
		}
		// Clicking the active choice again clears it.
		if pp.Decision == dec {
			pp.Decision, pp.DecisionBy = model.DecisionUndecided, "user"
		} else {
			pp.Decision, pp.DecisionBy = dec, "user"
		}
		return nil
	})
	if err != nil {
		http.NotFound(w, r)
		return
	}
	_ = s.store.View(id, func(p *model.Project) {
		keep, drop, und := p.Counts()
		s.render(w, "paper_with_counts", map[string]any{
			"P": p, "Paper": p.Paper(pid), "Keep": keep, "Drop": drop, "Undecided": und,
		})
	})
}

func (s *Server) handleBulk(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	action := r.FormValue("action")
	keepMin, _ := strconv.Atoi(r.FormValue("keep_min"))
	dropMax, _ := strconv.Atoi(r.FormValue("drop_max"))
	_ = s.store.SaveUndo(id, bulkLabel(action))
	_ = s.store.Update(id, func(p *model.Project) error {
		for _, pp := range p.Papers {
			switch action {
			case "threshold":
				if pp.Decision != model.DecisionUndecided {
					continue
				}
				if keepMin > 0 && pp.RankScore() >= keepMin {
					pp.Decision, pp.DecisionBy = model.DecisionKeep, "user"
				} else if dropMax > 0 && pp.RankScore() < dropMax {
					pp.Decision, pp.DecisionBy = model.DecisionDrop, "user"
				}
			case "drop-preprints":
				if sources.IsPreprint(pp) && pp.Decision != model.DecisionDrop {
					pp.Preprint = true
					pp.Decision, pp.DecisionBy = model.DecisionDrop, "user"
				}
			case "drop-jufo0":
				if pp.VenueRating != nil && pp.VenueRating.Tier == "below" && pp.Decision != model.DecisionDrop {
					pp.Decision, pp.DecisionBy = model.DecisionDrop, "user"
				}
			case "drop-unknown-venue":
				if pp.VenueRating != nil && pp.VenueRating.Tier == "unknown" && pp.Decision != model.DecisionDrop {
					pp.Decision, pp.DecisionBy = model.DecisionDrop, "user"
				}
			case "drop-non-english":
				if !sources.IsEnglish(pp) && pp.Decision != model.DecisionDrop {
					pp.NonEnglish = true
					pp.Decision, pp.DecisionBy = model.DecisionDrop, "user"
				}
			case "accept-ai":
				if pp.DecisionBy == "user" {
					continue
				}
				pp.DecisionBy = ""
				pp.ApplyVerdict()
			case "keep-undecided":
				if pp.Decision == model.DecisionUndecided {
					pp.Decision, pp.DecisionBy = model.DecisionKeep, "user"
				}
			case "drop-undecided":
				if pp.Decision == model.DecisionUndecided {
					pp.Decision, pp.DecisionBy = model.DecisionDrop, "user"
				}
			case "clear":
				pp.Decision, pp.DecisionBy = model.DecisionUndecided, ""
			}
		}
		return nil
	})
	http.Redirect(w, r, "/projects/"+id+"?view=screening&filter="+urlEsc(r.FormValue("filter")), http.StatusSeeOther)
}

func bulkLabel(action string) string {
	switch action {
	case "accept-ai":
		return "Accept AI suggestions"
	case "keep-undecided":
		return "Keep undecided"
	case "drop-undecided":
		return "Drop undecided"
	case "threshold":
		return "Apply score thresholds"
	case "drop-preprints":
		return "Drop preprints"
	case "drop-non-english":
		return "Drop non-English"
	case "drop-jufo0":
		return "Drop below-bar venues"
	case "drop-unknown-venue":
		return "Drop unknown venues"
	case "clear":
		return "Clear decisions"
	}
	return "Bulk action"
}

// --- adjudication ------------------------------------------------------------------

const adjudicateMinConfidence = 0.6

func (s *Server) handleAdjudicate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	strict := r.FormValue("standard") == "strict"
	var snap model.Project
	var todo []*model.Paper
	_ = s.store.View(id, func(p *model.Project) {
		snap = *p
		for _, pp := range p.Papers {
			if pp.Decision == model.DecisionUndecided && pp.DecisionBy != "user" {
				c := *pp
				todo = append(todo, &c)
			}
		}
	})
	if len(todo) == 0 {
		http.Redirect(w, r, "/projects/"+id+"?view=screening&error="+urlEsc("there are no undecided papers to adjudicate"), http.StatusSeeOther)
		return
	}
	if !s.ai.Enabled() {
		http.Redirect(w, r, "/projects/"+id+"?view=screening&error="+urlEsc("adjudication needs Claude"), http.StatusSeeOther)
		return
	}
	err := s.runJob(id, "adjudicate", func(ctx context.Context, lg jobLogger) error {
		client := s.ai.WithModel(s.cfg.SynthesisModel)
		lg.phase(1, 1, "Adjudicating undecided papers", len(todo))
		std := "inclusive"
		if strict {
			std = "strict"
		}
		lg.log("Third-reviewer pass (%s standard) over %d undecided papers with Claude (%s, %s), %d per call…", std, len(todo), client.BackendName(), client.ModelLabel(), 10)
		if err := client.Adjudicate(ctx, &snap, todo, strict, func(done, total int) {
			lg.progress(done, total)
			lg.log("Adjudicated %d/%d", done, total)
		}); err != nil {
			return err
		}
		keep, drop, still, low := 0, 0, 0, 0
		_ = s.store.SaveUndo(id, "Adjudicate undecided")
		_ = s.store.Update(id, func(p *model.Project) error {
			for _, t := range todo {
				pp := p.Paper(t.ID)
				if pp == nil || t.Adjudication == nil {
					continue
				}
				pp.Adjudication = t.Adjudication
				if pp.DecisionBy == "user" || pp.Decision != model.DecisionUndecided {
					continue
				}
				a := t.Adjudication
				switch {
				case a.Decision == model.VerdictUnsure:
					still++
				case a.Confidence < adjudicateMinConfidence:
					low++
				case a.Decision == model.VerdictKeep:
					pp.Decision, pp.DecisionBy = model.DecisionKeep, "ai"
					keep++
				case a.Decision == model.VerdictDrop:
					pp.Decision, pp.DecisionBy = model.DecisionDrop, "ai"
					drop++
				}
			}
			return nil
		})
		lg.log("Adjudication: %d → keep, %d → drop, %d still unsure, %d left undecided for low confidence (< %.0f%%).", keep, drop, still, low, adjudicateMinConfidence*100)
		return nil
	})
	if err != nil {
		http.Redirect(w, r, "/projects/"+id+"?view=screening&error="+urlEsc(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/projects/"+id+"?view=screening", http.StatusSeeOther)
}

// --- download job -------------------------------------------------------------

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var kept []model.Paper
	err := s.store.Update(id, func(p *model.Project) error {
		for _, pp := range p.Papers {
			if pp.Decision == model.DecisionKeep && pp.Download.Status != model.DownloadOK {
				pp.Download = model.DownloadResult{Status: model.DownloadPending}
				kept = append(kept, *pp)
			}
		}
		if len(p.Kept()) == 0 {
			return errors.New("no papers are marked keep")
		}
		if len(kept) == 0 {
			return errors.New("every kept paper already has a PDF")
		}
		if model.StageIndex(p.Stage) < 3 {
			p.Stage = model.StageDownloading
		}
		return nil
	})
	if err != nil {
		http.Redirect(w, r, "/projects/"+id+"?view=downloads&error="+urlEsc(err.Error()), http.StatusSeeOther)
		return
	}
	if err := s.runJob(id, "download", func(ctx context.Context, lg jobLogger) error {
		return s.downloadJob(ctx, id, kept, lg)
	}); err != nil {
		http.Redirect(w, r, "/projects/"+id+"?view=downloads&error="+urlEsc(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/projects/"+id+"?view=downloads", http.StatusSeeOther)
}

func (s *Server) downloadJob(ctx context.Context, id string, papers []model.Paper, lg jobLogger) error {
	dir, err := s.store.ProjectDir(id)
	if err != nil {
		return err
	}
	dir = filepath.Join(dir, "pdfs")
	lg.phase(1, 1, "Downloading PDFs", len(papers))
	lg.log("Downloading %d papers via: %s", len(papers), strings.Join(s.chain.Names(), " → "))
	const workers = 3
	jobs := make(chan model.Paper)
	var wg sync.WaitGroup
	var mu sync.Mutex
	done, ok := 0, 0
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for pp := range jobs {
				pp := pp
				_ = s.store.Update(id, func(p *model.Project) error {
					if t := p.Paper(pp.ID); t != nil {
						t.Download.Status = model.DownloadRunning
					}
					return nil
				})
				res := s.chain.Run(ctx, &pp, dir)
				var textPath string
				var textChars int
				if res.Status == model.DownloadOK && fulltext.Available() {
					if tp, n, err := fulltext.ExtractToFile(ctx, res.Path, filepath.Join(filepath.Dir(dir), "text")); err == nil {
						textPath, textChars = tp, n
					} else {
						lg.log("text extraction failed for %s: %v", shortTitle(pp.Title), err)
					}
				}
				_ = s.store.Update(id, func(p *model.Project) error {
					if t := p.Paper(pp.ID); t != nil {
						t.Download = res
						if textPath != "" {
							t.TextPath, t.TextChars = textPath, textChars
						}
					}
					return nil
				})
				mu.Lock()
				done++
				if res.Status == model.DownloadOK {
					ok++
					lg.log("✓ %s — via %s", shortTitle(pp.Title), res.Method)
				} else {
					lg.log("✗ %s — all methods failed", shortTitle(pp.Title))
				}
				lg.progress(done, len(papers))
				mu.Unlock()
			}
		}()
	}
	for _, pp := range papers {
		if ctx.Err() != nil {
			break
		}
		jobs <- pp
	}
	close(jobs)
	wg.Wait()
	_ = s.store.Update(id, func(p *model.Project) error {
		for _, pp := range p.Papers {
			if pp.Download.Status == model.DownloadRunning {
				pp.Download.Status = model.DownloadFailed
			}
		}
		if model.StageIndex(p.Stage) < 4 {
			p.Stage = model.StageDone
		}
		return nil
	})
	lg.log("Finished: %d of %d PDFs obtained.", ok, len(papers))
	return ctx.Err()
}

func (s *Server) handleRetry(w http.ResponseWriter, r *http.Request) {
	id, pid := r.PathValue("id"), r.PathValue("pid")
	var target *model.Paper
	_ = s.store.View(id, func(p *model.Project) {
		if pp := p.Paper(pid); pp != nil {
			c := *pp
			target = &c
		}
	})
	if target == nil {
		http.NotFound(w, r)
		return
	}
	err := s.runJob(id, "retry", func(ctx context.Context, lg jobLogger) error {
		return s.downloadJob(ctx, id, []model.Paper{*target}, lg)
	})
	if err != nil {
		http.Redirect(w, r, "/projects/"+id+"?view=downloads&error="+urlEsc(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/projects/"+id+"?view=downloads", http.StatusSeeOther)
}

// --- synthesis ------------------------------------------------------------------

// evidenceFor builds the reading material per paper: a full-text excerpt
// when extracted text exists (budget characters), otherwise the abstract.
// Missing text files for existing PDFs are extracted on the fly.
func (s *Server) evidenceFor(ctx context.Context, id string, papers []*model.Paper, budget int) []ai.Evidence {
	var out []ai.Evidence
	for _, pp := range papers {
		if pp.TextPath == "" && pp.Download.Status == model.DownloadOK && pp.Download.Path != "" && fulltext.Available() {
			if tp, n, err := fulltext.ExtractToFile(ctx, pp.Download.Path, filepath.Join(filepath.Dir(filepath.Dir(pp.Download.Path)), "text")); err == nil {
				pp.TextPath, pp.TextChars = tp, n
				pid := pp.ID
				_ = s.store.Update(id, func(p *model.Project) error {
					if t := p.Paper(pid); t != nil {
						t.TextPath, t.TextChars = tp, n
					}
					return nil
				})
			}
		}
		e := ai.Evidence{Paper: pp, Text: pp.Abstract}
		if pp.TextPath != "" {
			if x := fulltext.Excerpt(pp.TextPath, budget); x != "" {
				e.Text, e.FullText = x, true
			}
		}
		out = append(out, e)
	}
	return out
}

// methodSummary states, factually, how the corpus was built.
func (s *Server) methodSummary(p *model.Project) string {
	var dbs []string
	seen := map[string]bool{}
	for _, q := range p.Queries {
		if seen[q.Source] {
			continue
		}
		seen[q.Source] = true
		if src, ok := s.reg.Get(q.Source); ok {
			dbs = append(dbs, src.Label())
		} else {
			dbs = append(dbs, q.Source)
		}
	}
	snow := 0
	queries := 0
	for _, q := range p.Queries {
		if strings.HasPrefix(q.Query, "snowball") {
			snow++
		} else {
			queries++
		}
	}
	rounds := p.Rounds
	if rounds == 0 {
		rounds = 1
	}
	keep, _, _ := p.Counts()
	var sb strings.Builder
	fmt.Fprintf(&sb, "- Databases: %s.\n", strings.Join(dbs, ", "))
	fmt.Fprintf(&sb, "- %d search queries over %d round(s)", queries, rounds)
	if snow > 0 {
		fmt.Fprintf(&sb, ", plus %d citation-snowballing pass(es) via OpenAlex (backward and forward)", snow)
	}
	sb.WriteString(".\n")
	if p.YearFrom > 0 || p.YearTo > 0 {
		fmt.Fprintf(&sb, "- Publication years %d–%d.\n", p.YearFrom, p.YearTo)
	}
	fmt.Fprintf(&sb, "- %d unique candidate records after de-duplication by DOI and title.\n", len(p.Papers))
	screeners := "title/abstract screening by an LLM screener (" + s.ai.ModelLabel() + ") with a calibrated 0–100 relevance score and rationale"
	if s.useJev() {
		screeners += ", independently by a second, non-generative decision model (TypeSafe Jev, " + s.cfg.JevModel + ") answering a 5-level relevance rubric and include/exclude probabilities; disagreements were adjudicated by the researcher"
	}
	fmt.Fprintf(&sb, "- Screening: %s; final inclusion decisions confirmed by the researcher.\n", screeners)
	fmt.Fprintf(&sb, "- %d studies included.\n", keep)
	return sb.String()
}

// synthesisClient returns the AI client to use for the synthesis call.
func (s *Server) synthesisClient() *ai.Client { return s.ai.WithModel(s.cfg.SynthesisModel) }

func (s *Server) handleSynthesize(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var snap model.Project
	var kept []*model.Paper
	_ = s.store.Update(id, func(p *model.Project) error {
		p.EnsureBibKeys()
		snap = *p
		for _, pp := range p.Kept() {
			c := *pp
			kept = append(kept, &c)
		}
		return nil
	})
	if len(kept) == 0 {
		http.Redirect(w, r, "/projects/"+id+"?view=results&error="+urlEsc("no kept papers to synthesise"), http.StatusSeeOther)
		return
	}
	err := s.runJob(id, "synthesis", func(ctx context.Context, lg jobLogger) error {
		client := s.synthesisClient()
		lg.phase(1, 2, "Collecting evidence", len(kept))
		// Budget: ~150k characters of source text in total, 1k–7k per paper.
		per := 150000 / len(kept)
		if per > 7000 {
			per = 7000
		}
		if per < 1000 {
			per = 1000
		}
		ev := s.evidenceFor(ctx, id, kept, per)
		full := 0
		for _, e := range ev {
			if e.FullText {
				full++
			}
		}
		lg.log("Evidence: %d papers, %d with full text (%d chars each max), %d abstract-only.", len(ev), full, per, len(ev)-full)
		lg.phase(2, 2, "Writing background section", 0)
		lg.log("Drafting the background section with Claude (%s, %s)…", client.BackendName(), client.ModelLabel())
		text, err := client.Synthesize(ctx, &snap, ev, s.methodSummary(&snap))
		if err != nil {
			return err
		}
		valid := map[string]bool{}
		for _, pp := range kept {
			valid[pp.BibKey] = true
		}
		var unknown []string
		for _, k := range citationKeys(text) {
			if !valid[k] {
				unknown = append(unknown, k)
			}
		}
		if len(unknown) > 0 {
			lg.log("WARNING: the draft cites %d keys that are not in the included set: %s", len(unknown), strings.Join(unknown, ", "))
		}
		lg.log("Draft complete: %d words, %d distinct citations.", len(strings.Fields(text)), len(citationKeys(text)))
		_ = s.store.SaveUndo(id, "Regenerate background section")
		return s.store.Update(id, func(p *model.Project) error {
			p.Synthesis = text
			p.SynthesisModel = client.ModelLabel()
			p.SynthesisAt = time.Now()
			return nil
		})
	})
	if err != nil {
		http.Redirect(w, r, "/projects/"+id+"?view=results&error="+urlEsc(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/projects/"+id+"?view=results", http.StatusSeeOther)
}

func (s *Server) handleSynthesisExport(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var md, title string
	if err := s.store.View(id, func(p *model.Project) { md, title = p.Synthesis, p.Title }); err != nil || md == "" {
		http.NotFound(w, r)
		return
	}
	if strings.HasSuffix(r.URL.Path, ".tex") {
		w.Header().Set("Content-Type", "application/x-tex; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="background.tex"`)
		fmt.Fprintf(w, "%% %s — background section drafted by litreview; cite keys match bibliography.bib\n%% \\usepackage[natbib=true]{biblatex} or natbib; \\addbibresource{bibliography.bib}\n\n%s", title, markdownToLaTeX(md))
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="background.md"`)
	fmt.Fprintf(w, "---\ntitle: \"Background: %s\"\nbibliography: bibliography.bib\n---\n\n%s\n", strings.ReplaceAll(title, `"`, `'`), md)
}

// --- files & export ---------------------------------------------------------------

func (s *Server) handleFile(w http.ResponseWriter, r *http.Request) {
	id, name := r.PathValue("id"), filepath.Base(r.PathValue("name"))
	if strings.Contains(name, "..") || !strings.HasSuffix(name, ".pdf") {
		http.NotFound(w, r)
		return
	}
	path := filepath.Join(s.store.Dir(), id, "pdfs", name)
	if _, err := os.Stat(path); err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/pdf")
	http.ServeFile(w, r, path)
}

func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	format := strings.TrimPrefix(filepath.Ext(r.URL.Path), ".")
	var kept []*model.Paper
	var title string
	if err := s.store.Update(id, func(p *model.Project) error {
		p.EnsureBibKeys()
		title = p.Title
		for _, pp := range p.Kept() {
			c := *pp
			kept = append(kept, &c)
		}
		return nil
	}); err != nil {
		http.NotFound(w, r)
		return
	}
	switch format {
	case "json":
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Disposition", `attachment; filename="bibliography.json"`)
		writeJSON(w, kept)
	case "bib":
		w.Header().Set("Content-Type", "application/x-bibtex; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="bibliography.bib"`)
		fmt.Fprintf(w, "%% %s — %d kept papers, exported %s\n\n", title, len(kept), time.Now().Format(time.RFC3339))
		for _, pp := range kept {
			w.Write([]byte(bibtex(pp)))
		}
	default:
		http.NotFound(w, r)
	}
}

func bibtex(p *model.Paper) string {
	key := p.BibKey
	if key == "" {
		key = strings.ToLower(strings.ReplaceAll(p.FirstAuthorSurname(), " ", "")) + strconv.Itoa(p.Year)
	}
	kind := "article"
	v := strings.ToLower(p.Venue)
	switch {
	case p.Venue == "" || strings.HasPrefix(v, "arxiv") || strings.Contains(v, "ssrn") || strings.Contains(v, "preprint"):
		kind = "misc"
	case strings.Contains(v, "proceedings") || strings.Contains(v, "conference") || strings.Contains(v, "workshop") || strings.Contains(v, "symposium"):
		kind = "inproceedings"
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "@%s{%s,\n", kind, key)
	fmt.Fprintf(&sb, "  title = {%s},\n", bibEsc(p.Title))
	if len(p.Authors) > 0 {
		fmt.Fprintf(&sb, "  author = {%s},\n", bibEsc(strings.Join(p.Authors, " and ")))
	} else {
		// No author on record: give BibTeX a sort key so author-year styles
		// do not warn, and let the entry read as the venue/organisation.
		fmt.Fprintf(&sb, "  key = {%s},\n", bibEsc(key))
		if p.Venue != "" {
			fmt.Fprintf(&sb, "  author = {{%s}},\n", bibEsc(p.Venue))
		}
	}
	if p.Year > 0 {
		fmt.Fprintf(&sb, "  year = {%d},\n", p.Year)
	}
	if p.Venue != "" {
		field := "journal"
		if kind == "inproceedings" {
			field = "booktitle"
		} else if kind == "misc" {
			field = "howpublished"
		}
		fmt.Fprintf(&sb, "  %s = {%s},\n", field, bibEsc(p.Venue))
	}
	if p.DOI != "" {
		fmt.Fprintf(&sb, "  doi = {%s},\n", p.DOI)
	}
	if p.URL != "" {
		fmt.Fprintf(&sb, "  url = {%s},\n", p.URL)
	}
	if p.Download.Path != "" {
		fmt.Fprintf(&sb, "  file = {%s},\n", p.Download.Path)
	}
	if p.Abstract != "" {
		fmt.Fprintf(&sb, "  abstract = {%s},\n", bibEsc(p.Abstract))
	}
	sb.WriteString("}\n\n")
	return sb.String()
}

func bibEsc(s string) string {
	return strings.NewReplacer("{", "\\{", "}", "\\}", "%", "\\%", "&", "\\&").Replace(s)
}

// --- small utilities ---------------------------------------------------------------

func urlEsc(s string) string {
	return url.QueryEscape(s)
}

func shortTitle(t string) string {
	if len(t) > 70 {
		return t[:70] + "…"
	}
	return t
}

var nowFn = time.Now

// --- PRISMA ----------------------------------------------------------------------

func (s *Server) handlePrisma(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var p *model.Project
	var c prisma.Counts
	if err := s.store.View(id, func(pp *model.Project) {
		cp := *pp
		p = &cp
		c = prisma.Compute(pp)
	}); err != nil {
		http.NotFound(w, r)
		return
	}
	label := func(n string) string {
		if src, ok := s.reg.Get(n); ok {
			return src.Label()
		}
		return n
	}
	screeners := fmt.Sprintf("Each record was screened independently by an LLM screener (Claude %s) producing a calibrated relevance score and rationale", s.ai.ModelLabel())
	if s.useJev() {
		screeners += fmt.Sprintf(" and by a non-generative decision model (TypeSafe Jev, %s) answering a relevance rubric and include/exclude questions; disagreements were adjudicated", s.cfg.JevModel)
	}
	screeners += ", and all decisions were confirmed by the researcher."
	svg := prisma.SVG(c, label)
	if strings.HasSuffix(r.URL.Path, ".svg") {
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Content-Disposition", `attachment; filename="prisma.svg"`)
		_, _ = w.Write([]byte(svg))
		return
	}
	s.render(w, "prisma", map[string]any{
		"Base": s.base(), "P": p, "C": c, "SVG": template.HTML(svg),
		"Methods": prisma.Methods(p, c, label, screeners),
		"DBLines": c.DatabaseLines(label), "Reasons": c.ReasonLines(),
	})
}
