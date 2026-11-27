package server

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"literaturereview/internal/ai"
	"literaturereview/internal/model"
	"literaturereview/internal/sources"
	"literaturereview/internal/store"
)

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
	HasPapers       bool
	HasKept         bool
	HasQueries      bool
	StageIdx        int
	JobRunning      bool
	JobName         string
	PendingPDFs     int    // kept papers without a PDF yet
	NeedsLogin      int    // kept papers skipped only because the proxy session expired
	SynthesisNext   string // model the next background-section call will use
	SynthesisChoice string // the user's saved choice (may be empty)
	ModelChoices    []ai.ModelOption
	StepModels      []stepModelView
	SessionExpired  bool
	SessionMsg      string
	LoginRunning    bool
	LoginDone       bool
	LoginOK         bool
	LoginMessage    string
	QueryText       map[string]string
	Extracted       int
	ProxyMissing    bool
	Conflicts       int
	FullTextCount   int
	Preprints       int
	NonEnglish      int
	UndoLabel       string
	RedoLabel       string
	FailedSources   []string // one line per database whose queries all failed in the last round
	Rated           int      // papers with a venue rating
	Jufo0           int      // tier "below" and not dropped
	UnknownVenue    int      // tier "unknown" and not dropped
	Unrated         int      // kept papers without a rating
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
			if pp.Download.NeedsLogin {
				d.NeedsLogin++
			}
		}
		if pp.Extract != nil {
			d.Extracted++
		}
		if pp.TextPath != "" {
			d.FullTextCount++
		}
	}
	d.SynthesisNext = s.ai.ForStep(ai.StepSynthesis, nil, s.cfg.StepModels).ModelLabel()
	d.SynthesisChoice = p.StepModel(string(ai.StepSynthesis))
	d.ModelChoices = ai.ModelChoices
	d.StepModels = s.stepModelViews(p)
	d.SessionExpired, d.SessionMsg, _ = s.chain.Session.Status()
	d.LoginRunning, d.LoginDone, d.LoginOK, d.LoginMessage, _, _ = s.login.snapshot()
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
