package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"literaturereview/internal/keywords"
	"literaturereview/internal/model"
)

type keywordData struct {
	Base       baseData
	P          *model.Project
	Net        *keywords.Network
	NetJSON    string
	MinCount   int
	MaxNodes   int
	Scope      string
	Covered    int // papers in scope with at least one keyword
	InScope    int
	Error      string
	Running    bool
	Enrichable bool
}

func (s *Server) keywordScope(p *model.Project, scope string) []*model.Paper {
	if scope == "all" {
		return p.Papers
	}
	return p.Kept()
}

func (s *Server) handleKeywords(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	minCount, _ := strconv.Atoi(r.URL.Query().Get("min"))
	if minCount <= 0 {
		minCount = 2
	}
	maxNodes, _ := strconv.Atoi(r.URL.Query().Get("max"))
	if maxNodes <= 0 {
		maxNodes = 80
	}
	scope := r.URL.Query().Get("scope")
	if scope != "all" {
		scope = "kept"
	}
	var d keywordData
	if err := s.store.View(id, func(p *model.Project) {
		papers := s.keywordScope(p, scope)
		net := keywords.Build(papers, minCount, maxNodes)
		b, _ := json.Marshal(net)
		covered := 0
		for _, pp := range papers {
			if len(keywords.PaperKeywords(pp)) > 0 {
				covered++
			}
		}
		d = keywordData{Base: s.base(), P: p, Net: net, NetJSON: string(b), MinCount: minCount, MaxNodes: maxNodes, Scope: scope, Covered: covered, InScope: len(papers), Running: p.Job != nil && p.Job.Running}
	}); err != nil {
		http.NotFound(w, r)
		return
	}
	_, d.Enrichable = s.reg.FirstEnricher()
	d.Error = r.URL.Query().Get("error")
	s.render(w, "keywords", d)
}

// handleKeywordsEnrich fetches database keywords for papers lacking them.
func (s *Server) handleKeywordsEnrich(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	scope := r.FormValue("scope")
	enr, ok := s.reg.FirstEnricher()
	if !ok {
		http.Redirect(w, r, "/projects/"+id+"/keywords?error="+urlEsc("no keyword-capable database (OpenAlex) registered"), http.StatusSeeOther)
		return
	}
	force := r.FormValue("force") == "1"
	var todo []*model.Paper
	_ = s.store.View(id, func(p *model.Project) {
		for _, pp := range s.keywordScope(p, scope) {
			if (force || len(pp.Keywords) == 0) && pp.DOI != "" {
				c := *pp
				c.Keywords = nil
				todo = append(todo, &c)
			}
		}
	})
	if len(todo) == 0 {
		http.Redirect(w, r, "/projects/"+id+"/keywords?scope="+scope+"&error="+urlEsc("every paper with a DOI already has database keywords"), http.StatusSeeOther)
		return
	}
	err := s.runJob(id, "keywords", func(ctx context.Context, lg jobLogger) error {
		lg.phase(1, 1, "Fetching keywords from OpenAlex", len(todo))
		lg.log("Fetching OpenAlex keywords and concepts for %d papers…", len(todo))
		n, err := enr.Enrich(ctx, todo, func(done, total int) { lg.progress(done, total) })
		_ = s.store.Update(id, func(p *model.Project) error {
			for _, t := range todo {
				if pp := p.Paper(t.ID); pp != nil && (len(t.Keywords) > 0 || force) {
					pp.Keywords = t.Keywords
					if pp.Language == "" {
						pp.Language = t.Language
					}
				}
			}
			return nil
		})
		lg.log("Keywords filled for %d of %d papers.", n, len(todo))
		return err
	})
	if err != nil {
		http.Redirect(w, r, "/projects/"+id+"/keywords?scope="+scope+"&error="+urlEsc(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/projects/"+id+"/keywords?scope="+scope, http.StatusSeeOther)
}

// handleKeywordsExport serves VOSviewer map/network files and a CSV.
func (s *Server) handleKeywordsExport(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	minCount, _ := strconv.Atoi(r.URL.Query().Get("min"))
	if minCount <= 0 {
		minCount = 2
	}
	maxNodes, _ := strconv.Atoi(r.URL.Query().Get("max"))
	if maxNodes <= 0 {
		maxNodes = 80
	}
	scope := r.URL.Query().Get("scope")
	var net *keywords.Network
	if err := s.store.View(id, func(p *model.Project) { net = keywords.Build(s.keywordScope(p, scope), minCount, maxNodes) }); err != nil {
		http.NotFound(w, r)
		return
	}
	switch {
	case strings.HasSuffix(r.URL.Path, "/vosviewer-map.txt"):
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="vosviewer-map.txt"`)
		_, _ = w.Write([]byte(net.VOSviewerMap()))
	case strings.HasSuffix(r.URL.Path, "/vosviewer-network.txt"):
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="vosviewer-network.txt"`)
		_, _ = w.Write([]byte(net.VOSviewerNetwork()))
	case strings.HasSuffix(r.URL.Path, "/keywords.csv"):
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="keywords.csv"`)
		fmt.Fprintln(w, "keyword,papers,cluster,links,total_link_strength")
		for _, n := range net.Nodes {
			fmt.Fprintf(w, "%q,%d,%d,%d,%d\n", n.Label, n.Count, n.Cluster, n.Links, n.Weight)
		}
	default:
		http.NotFound(w, r)
	}
}
