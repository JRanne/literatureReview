package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"literaturereview/internal/model"
	"literaturereview/internal/venues"
)

// handleVenues rates venues (JUFO level, AIS basket, IS conference) for the
// kept papers, or for all screened papers with scope=all.
func (s *Server) handleVenues(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	scope := r.FormValue("scope")
	back := r.FormValue("back")
	if back == "" {
		back = "/projects/" + id + "?view=screening"
	}
	force := r.FormValue("force") == "1"
	var todo []*model.Paper
	_ = s.store.View(id, func(p *model.Project) {
		src := p.Kept()
		if scope == "all" {
			src = p.Papers
		}
		for _, pp := range src {
			if (force || pp.VenueRating == nil || pp.VenueRating.Version < ratingVersion) && (pp.Venue != "" || pp.ISSN != "") {
				c := *pp
				todo = append(todo, &c)
			}
		}
	})
	if len(todo) == 0 {
		http.Redirect(w, r, back+"&error="+urlEsc("every paper in scope is already rated (or has no venue)"), http.StatusSeeOther)
		return
	}
	err := s.runJob(id, "venues", func(ctx context.Context, lg jobLogger) error {
		rater := s.rater()
		lg.phase(1, 1, "Rating venues (JUFO, Norway, CORE, OpenAlex, AIS lists)", len(todo))
		lg.log("Rating %d venues: Finnish Publication Forum (with Norwegian/Danish levels), OpenAlex source metadata (core list, DOAJ, h-index), CORE conference ranks, AIS basket and IS conference lists…", len(todo))
		rated, known := 0, 0
		tiers := map[string]int{}
		for i, pp := range todo {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			rt := rater.Rate(ctx, pp.Venue, pp.ISSN)
			vr := &model.VenueRating{JufoLevel: rt.JufoLevel, JufoID: rt.JufoID, NorwayLevel: rt.NorwayLevel, Basket: rt.Basket, ISConf: rt.ISConf, CORERank: rt.CORERank, SJRQuartile: rt.SJRQuartile, OACore: rt.OACore, DOAJ: rt.DOAJ, HIndex: rt.HIndex, Citedness: rt.Citedness, Tier: rt.Tier(), Label: rt.Label(), Version: ratingVersion}
			tiers[vr.Tier]++
			if vr.Tier != "unknown" {
				known++
			}
			rated++
			pid := pp.ID
			_ = s.store.Update(id, func(p *model.Project) error {
				if t := p.Paper(pid); t != nil {
					t.VenueRating = vr
				}
				return nil
			})
			lg.progress(i+1, len(todo))
			if (i+1)%10 == 0 {
				lg.log("Rated %d/%d", i+1, len(todo))
			}
		}
		lg.log("Done: %d venues rated, %d with at least one signal. Tiers: top %d, good %d, basic %d, below %d, unknown %d.", rated, known, tiers["top"], tiers["good"], tiers["basic"], tiers["below"], tiers["unknown"])
		return nil
	})
	if err != nil {
		http.Redirect(w, r, back+"&error="+urlEsc(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

// ratingVersion bumps when the rating logic gains signals, so older
// ratings are refreshed on the next run.
const ratingVersion = 5

func (s *Server) rater() *venues.Rater {
	s.jufoOnce.Do(func() {
		j := venues.NewJufo(s.cfg.UserAgent, filepath.Join(s.store.Dir(), "venues.json"))
		if s.cfg.JufoBase != "" {
			j.Base = s.cfg.JufoBase
		}
		oa := venues.NewOpenAlex(s.cfg.UserAgent, s.cfg.ContactEmail)
		oa.APIKey = s.cfg.OpenAlexAPIKey
		if s.cfg.OpenAlexBase != "" {
			oa.Base = s.cfg.OpenAlexBase
		}
		core := venues.NewCORE(s.cfg.UserAgent, filepath.Join(s.store.Dir(), "core-ranks.csv"))
		if s.cfg.COREURL != "" {
			core.URL = s.cfg.COREURL
		}
		cr := venues.NewCrossRefJournals(s.cfg.UserAgent, s.cfg.ContactEmail)
		if s.cfg.CrossRefBase != "" {
			cr.Base = s.cfg.CrossRefBase
		}
		s.raterClient = &venues.Rater{Jufo: j, OA: oa, CORE: core, Scimago: venues.NewScimago(filepath.Join(s.store.Dir(), "scimago.csv")), CrossRef: cr}
	})
	return s.raterClient
}

// handleScimagoUpload stores a Scimago Journal Rank CSV for quartile lookups.
func (s *Server) handleScimagoUpload(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		http.Redirect(w, r, "/access?error="+urlEsc(err.Error()), http.StatusSeeOther)
		return
	}
	f, _, err := r.FormFile("scimago")
	if err != nil {
		http.Redirect(w, r, "/access?error="+urlEsc("no file uploaded"), http.StatusSeeOther)
		return
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 64<<20))
	if err != nil || !strings.Contains(strings.ToLower(string(data[:min(2000, len(data))])), "sjr") {
		http.Redirect(w, r, "/access?error="+urlEsc("that does not look like a Scimago Journal Rank CSV (expected a semicolon-separated file with an SJR column)"), http.StatusSeeOther)
		return
	}
	path := filepath.Join(s.store.Dir(), "scimago.csv")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		http.Redirect(w, r, "/access?error="+urlEsc(err.Error()), http.StatusSeeOther)
		return
	}
	rt := s.rater()
	if err := rt.Scimago.Reload(); err != nil {
		http.Redirect(w, r, "/access?error="+urlEsc("could not parse the file: "+err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/access?msg="+urlEsc(fmt.Sprintf("Scimago file loaded: %d journals. Re-rate venues on your projects to apply the quartiles.", rt.Scimago.Count)), http.StatusSeeOther)
}

func (s *Server) scimagoInfo() (int, bool) {
	rt := s.rater()
	if err := rt.Scimago.Load(); err != nil {
		return 0, false
	}
	return rt.Scimago.Count, rt.Scimago.Count > 0
}
