package server

import (
	"context"
	"fmt"
	"net/http"
	"strconv"

	"literaturereview/internal/ai"
	"literaturereview/internal/model"
	"literaturereview/internal/sources"
	"literaturereview/internal/store"
)

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
		if pp.Decision != model.DecisionDrop {
			pp.NotRetrieved = false // back in the analysis
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
			case "drop-not-retrieved":
				// Kept papers whose full text could not be obtained leave the
				// analysis; PRISMA reports them as "not retrieved".
				if pp.Decision == model.DecisionKeep && pp.Download.Status != model.DownloadOK {
					pp.NotRetrieved = true
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
	case "drop-not-retrieved":
		return "Remove papers without full text"
	case "drop-jufo0":
		return "Drop below-bar venues"
	case "drop-unknown-venue":
		return "Drop unknown venues"
	case "clear":
		return "Clear decisions"
	}
	return "Bulk action"
}

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
		client := s.client(ai.StepAdjudication, &snap)
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
