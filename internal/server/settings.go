package server

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"literaturereview/internal/model"
)

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
