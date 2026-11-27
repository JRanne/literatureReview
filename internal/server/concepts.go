package server

import (
	"context"
	"encoding/csv"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"literaturereview/internal/ai"
	"literaturereview/internal/keywords"
	"literaturereview/internal/model"
)

type conceptData struct {
	Base     baseData
	P        *model.Project
	Papers   []*model.Paper
	Concepts []model.Concept
	Counts   map[string]int
	Running  bool
	Error    string
	Assigned int
}

func (s *Server) handleConcepts(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var d conceptData
	if err := s.store.View(id, func(p *model.Project) {
		d = conceptData{Base: s.base(), P: p, Papers: p.Kept(), Concepts: p.Concepts, Counts: map[string]int{}, Running: p.Job != nil && p.Job.Running}
		for _, pp := range d.Papers {
			if pp.Concepts != nil {
				d.Assigned++
			}
			for _, h := range pp.Concepts {
				d.Counts[h.Name]++
			}
		}
	}); err != nil {
		http.NotFound(w, r)
		return
	}
	d.Error = r.URL.Query().Get("error")
	s.render(w, "concepts", d)
}

// handleConceptsBuild proposes concepts (unless kept) and assigns them.
func (s *Server) handleConceptsBuild(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rebuild := r.FormValue("rebuild") == "1"
	var snap model.Project
	var kept []*model.Paper
	_ = s.store.View(id, func(p *model.Project) {
		snap = *p
		for _, pp := range p.Kept() {
			c := *pp
			kept = append(kept, &c)
		}
	})
	if len(kept) == 0 {
		http.Redirect(w, r, "/projects/"+id+"/concepts?error="+urlEsc("no included papers"), http.StatusSeeOther)
		return
	}
	if !s.ai.Enabled() {
		http.Redirect(w, r, "/projects/"+id+"/concepts?error="+urlEsc("the concept matrix needs Claude"), http.StatusSeeOther)
		return
	}
	err := s.runJob(id, "concepts", func(ctx context.Context, lg jobLogger) error {
		client := s.client(ai.StepConcepts, &snap)
		concepts := snap.Concepts
		if rebuild || len(concepts) == 0 {
			lg.phase(1, 2, "Proposing concepts", 0)
			net := keywords.Build(kept, 2, 40)
			clusters := map[int][]string{}
			for _, n := range net.Nodes {
				if len(clusters[n.Cluster]) < 8 {
					clusters[n.Cluster] = append(clusters[n.Cluster], n.Label)
				}
			}
			var lines []string
			var ids []int
			for k := range clusters {
				ids = append(ids, k)
			}
			sort.Ints(ids)
			for _, k := range ids {
				lines = append(lines, fmt.Sprintf("cluster %d: %s", k, strings.Join(clusters[k], ", ")))
			}
			lg.log("Asking Claude (%s) to propose concept-matrix columns from %d papers and %d keyword clusters…", client.ModelLabel(), len(kept), len(lines))
			cs, err := client.ProposeConcepts(ctx, &snap, kept, lines)
			if err != nil {
				return fmt.Errorf("propose concepts: %w", err)
			}
			concepts = cs
			names := make([]string, 0, len(cs))
			for _, c := range cs {
				names = append(names, c.Name)
			}
			lg.log("Concepts: %s", strings.Join(names, "; "))
		} else {
			lg.log("Reusing the %d existing concepts; assigning papers.", len(concepts))
		}
		lg.phase(2, 2, "Assigning papers to concepts", len(kept))
		if err := client.AssignConcepts(ctx, &snap, concepts, kept, func(done, total int) {
			lg.progress(done, total)
			lg.log("Assigned %d/%d", done, total)
		}); err != nil {
			return fmt.Errorf("assign concepts: %w", err)
		}
		_ = s.store.SaveUndo(id, "Build concept matrix")
		return s.store.Update(id, func(p *model.Project) error {
			p.Concepts = concepts
			p.ConceptsAt = nowFn()
			for _, k := range kept {
				if pp := p.Paper(k.ID); pp != nil {
					pp.Concepts = k.Concepts
				}
			}
			return nil
		})
	})
	if err != nil {
		http.Redirect(w, r, "/projects/"+id+"/concepts?error="+urlEsc(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/projects/"+id+"/concepts", http.StatusSeeOther)
}

// handleConceptsExport serves the matrix as CSV or Excel.
func (s *Server) handleConceptsExport(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var rows [][]string
	var title string
	if err := s.store.View(id, func(p *model.Project) {
		title = p.Title
		header := []string{"Cite key", "First author", "Year", "Title", "Venue"}
		for _, c := range p.Concepts {
			header = append(header, c.Name)
		}
		rows = append(rows, header)
		for _, pp := range p.Kept() {
			row := []string{pp.BibKey, pp.FirstAuthorSurname(), yearStr(pp.Year), pp.Title, pp.Venue}
			hit := map[string]string{}
			for _, h := range pp.Concepts {
				hit[h.Name] = h.Note
				if h.Note == "" {
					hit[h.Name] = "✓"
				}
			}
			for _, c := range p.Concepts {
				row = append(row, hit[c.Name])
			}
			rows = append(rows, row)
		}
	}); err != nil {
		http.NotFound(w, r)
		return
	}
	if strings.HasSuffix(r.URL.Path, ".xlsx") {
		w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
		w.Header().Set("Content-Disposition", `attachment; filename="concept-matrix.xlsx"`)
		if err := writeXLSXRows(w, "Concept matrix", title, rows, 5); err != nil {
			http.Error(w, err.Error(), 500)
		}
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="concept-matrix.csv"`)
	_, _ = w.Write([]byte("\xEF\xBB\xBF"))
	cw := csv.NewWriter(w)
	_ = cw.WriteAll(rows)
}
