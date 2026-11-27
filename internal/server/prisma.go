package server

import (
	"fmt"
	"html/template"
	"net/http"
	"strings"

	"literaturereview/internal/ai"
	"literaturereview/internal/model"
	"literaturereview/internal/prisma"
)

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
	screeners := fmt.Sprintf("Each record was screened independently by an LLM screener (Claude %s) producing a calibrated relevance score and rationale", s.client(ai.StepScreening, p).ModelLabel())
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
