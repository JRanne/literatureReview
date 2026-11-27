package server

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"literaturereview/internal/ai"
	"literaturereview/internal/fulltext"
	"literaturereview/internal/model"
)

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
	screeners := "title/abstract screening by an LLM screener (" + s.client(ai.StepScreening, p).ModelLabel() + ") with a calibrated 0–100 relevance score and rationale"
	if s.useJev() {
		screeners += ", independently by a second, non-generative decision model (TypeSafe Jev, " + s.cfg.JevModel + ") answering a 5-level relevance rubric and include/exclude probabilities; disagreements were adjudicated by the researcher"
	}
	fmt.Fprintf(&sb, "- Screening: %s; final inclusion decisions confirmed by the researcher.\n", screeners)
	fmt.Fprintf(&sb, "- %d studies included.\n", keep)
	return sb.String()
}

func (s *Server) handleSynthesize(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	choice := strings.TrimSpace(r.FormValue("model"))
	var saved string
	_ = s.store.View(id, func(p *model.Project) { saved = p.StepModel(string(ai.StepSynthesis)) })
	if !s.ai.Catalog().Known(choice) && choice != saved {
		http.Redirect(w, r, "/projects/"+id+"?view=results&error="+urlEsc("unknown model "+choice), http.StatusSeeOther)
		return
	}
	var snap model.Project
	var kept []*model.Paper
	_ = s.store.Update(id, func(p *model.Project) error {
		p.EnsureBibKeys()
		p.SetStepModel(string(ai.StepSynthesis), choice)
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
		client := s.client(ai.StepSynthesis, &snap)
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
