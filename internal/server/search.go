package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"literaturereview/internal/ai"
	"literaturereview/internal/model"
	"literaturereview/internal/sources"
)

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
		client := s.client(ai.StepScreening, p)
		lg.log("Screening %d %s with Claude (%s, %s), %d batches of up to %d, 3 at a time…", len(papers), label, client.BackendName(), client.ModelLabel(), (len(papers)+19)/20, 20)
		if err := client.ScreenPapers(ctx, p, papers, func(done, total int) {
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
			client := s.client(ai.StepQueries, p)
			lg.log("Asking Claude (%s, %s) to design search queries for %d databases…", client.BackendName(), client.ModelLabel(), len(active))
			qs, err := client.GenerateQueries(ctx, p, infos)
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
			qs, err := s.client(ai.StepQueries, p).ExpandQueries(ctx, p, infos, stats, good, bad, keptCount(all), p.Target)
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
