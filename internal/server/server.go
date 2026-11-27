// Package server exposes the review workflow over HTTP.
package server

import (
	"context"
	"embed"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"literaturereview/internal/ai"
	"literaturereview/internal/browserlogin"
	"literaturereview/internal/config"
	"literaturereview/internal/fetch"
	"literaturereview/internal/jev"
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
	// browser is the headless Chromium used for downloads and for renewing
	// the proxy session silently (nil when no browser is installed).
	browser *browserlogin.Downloader

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
			s.browser = &browserlogin.Downloader{Binary: bin, ProfileDir: filepath.Join(st.Dir(), "browser-profile"), Cookies: s.browserCookies}
			s.chain.SetBrowser(s.browser, func() string {
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
	c, msg := ai.SelectBackend(ai.BackendOptions{
		Mode:     cfg.AIBackend,
		APIKey:   cfg.AnthropicAPIKey,
		APIModel: cfg.AnthropicModel,
		CLIPath:  cfg.ClaudeCLIPath,
		CLIModel: cfg.ClaudeCLIModel,
	}, ai.LookupCLI)
	log.Print(msg)
	for _, st := range ai.AllSteps {
		if m := cfg.StepModels[st.Step]; m != "" {
			log.Printf("AI model for %s: %s", st.Label, m)
		}
	}
	return c
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
	mux.HandleFunc("POST /projects/{id}/models", s.handleModels)
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
	mux.HandleFunc("POST /projects/{id}/zotero/add", s.handleZoteroAdd)
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
