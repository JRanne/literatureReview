// Package sources queries open academic databases and merges their results.
package sources

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"literaturereview/internal/config"
	"literaturereview/internal/model"
	"literaturereview/internal/store"
)

// Options narrows a search.
type Options struct {
	YearFrom int
	YearTo   int
	Limit    int
}

// Result is one query's outcome: the papers, the exact request URL and the
// total number of hits the database reported.
type Result struct {
	Papers []*model.Paper
	URL    string
	Total  int
}

// Source is one academic database.
type Source interface {
	// Name is the stable machine identifier (lowercase).
	Name() string
	// Label is a human-readable name.
	Label() string
	// Syntax tells the AI how to phrase queries for this database.
	Syntax() string
	// Search runs one query. On error the returned Result still carries the
	// request URL when one was formed.
	Search(ctx context.Context, query string, opts Options) (*Result, error)
}

// Registry holds configured sources keyed by name, in display order.
type Registry struct {
	order []string
	byKey map[string]Source
}

// NewRegistry builds every source that the configuration allows.
func NewRegistry(cfg config.Config) *Registry {
	client := &http.Client{Timeout: 45 * time.Second}
	r := &Registry{byKey: map[string]Source{}}
	add := func(s Source) {
		r.order = append(r.order, s.Name())
		r.byKey[s.Name()] = s
	}
	add(&OpenAlex{client: client, email: cfg.ContactEmail, ua: cfg.UserAgent, apiKey: cfg.OpenAlexAPIKey})
	add(&SemanticScholar{client: client, apiKey: cfg.SemanticScholarAPIKey, ua: cfg.UserAgent})
	add(&CrossRef{client: client, email: cfg.ContactEmail, ua: cfg.UserAgent})
	add(&PubMed{client: client, email: cfg.ContactEmail, ua: cfg.UserAgent})
	if cfg.CoreAPIKey != "" {
		add(&Core{client: client, apiKey: cfg.CoreAPIKey, ua: cfg.UserAgent})
	}
	return r
}

// Add registers an extra source (used by tests to inject fakes).
func (r *Registry) Add(s Source) {
	if _, ok := r.byKey[s.Name()]; !ok {
		r.order = append(r.order, s.Name())
	}
	r.byKey[s.Name()] = s
}

// All returns the sources in display order.
func (r *Registry) All() []Source {
	out := make([]Source, 0, len(r.order))
	for _, k := range r.order {
		out = append(out, r.byKey[k])
	}
	return out
}

// Get returns a source by name.
func (r *Registry) Get(name string) (Source, bool) {
	s, ok := r.byKey[name]
	return s, ok
}

// --- shared helpers ---------------------------------------------------------

// limiter enforces a minimum interval between calls to one API.
type limiter struct {
	mu       sync.Mutex
	last     time.Time
	interval time.Duration
}

func (l *limiter) wait(ctx context.Context) error {
	l.mu.Lock()
	next := l.last.Add(l.interval)
	now := time.Now()
	if next.After(now) {
		l.last = next
	} else {
		l.last = now
	}
	l.mu.Unlock()
	d := time.Until(l.last)
	if d <= 0 {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// doGet performs a GET with retry-on-429 and returns the body.
func doGet(ctx context.Context, client *http.Client, url, ua string, headers map[string]string) ([]byte, error) {
	var lastErr error
	wait := 3 * time.Second
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(wait):
			}
			wait *= 2
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", ua)
		req.Header.Set("Accept", "application/json, application/xml;q=0.9, */*;q=0.8")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 50<<20))
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode == 429 || resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
			if ra, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && ra > 0 {
				if ra > 120 {
					// A long Retry-After means a quota, not a burst: retrying is pointless.
					return nil, fmt.Errorf("HTTP 429 from %s: rate limit or daily quota exhausted, retry after %s (%s)", hostOf(url), (time.Duration(ra) * time.Second).Round(time.Minute), quotaHint(url))
				}
				wait = time.Duration(ra) * time.Second
			}
			continue
		}
		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("HTTP %d from %s: %s", resp.StatusCode, url, strings.TrimSpace(truncate(string(body), 200)))
		}
		return body, nil
	}
	return nil, lastErr
}

func hostOf(u string) string {
	if i := strings.Index(u, "://"); i >= 0 {
		u = u[i+3:]
	}
	if i := strings.IndexAny(u, "/?"); i >= 0 {
		u = u[:i]
	}
	return u
}

// quotaHint explains how to lift a provider's limit.
func quotaHint(u string) string {
	switch {
	case strings.Contains(u, "openalex.org"):
		return "OpenAlex's free tier has a daily credit budget; set OPENALEX_API_KEY for a higher limit or wait"
	case strings.Contains(u, "semanticscholar.org"):
		return "the anonymous Semantic Scholar pool is shared; request a free key and set S2_API_KEY"
	}
	return "wait and retry"
}

func getJSON(ctx context.Context, client *http.Client, url, ua string, headers map[string]string, v any) error {
	body, err := doGet(ctx, client, url, ua, headers)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("decode JSON from %s: %w", url, err)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

var tagRe = regexp.MustCompile(`<[^>]+>`)
var spaceRe = regexp.MustCompile(`\s+`)

// stripTags removes XML/HTML markup (CrossRef JATS abstracts, PubMed).
func stripTags(s string) string {
	s = tagRe.ReplaceAllString(s, " ")
	s = strings.NewReplacer("&lt;", "<", "&gt;", ">", "&amp;", "&", "&quot;", `"`, "&#39;", "'", "&nbsp;", " ").Replace(s)
	return strings.TrimSpace(spaceRe.ReplaceAllString(s, " "))
}

func inYearRange(year int, opts Options) bool {
	if year == 0 {
		return true
	}
	if opts.YearFrom > 0 && year < opts.YearFrom {
		return false
	}
	if opts.YearTo > 0 && year > opts.YearTo {
		return false
	}
	return true
}

// --- merging ---------------------------------------------------------------

// Merge folds incoming papers into the existing list, deduplicating by DOI
// and then by normalised title. It returns the number of new papers added.
func Merge(existing []*model.Paper, incoming []*model.Paper) ([]*model.Paper, int) {
	byDOI := map[string]*model.Paper{}
	byTitle := map[string]*model.Paper{}
	for _, p := range existing {
		if d := model.NormalizeDOI(p.DOI); d != "" {
			byDOI[d] = p
		}
		if t := model.NormalizeTitle(p.Title); len(t) > 20 {
			byTitle[t] = p
		}
	}
	added := 0
	for _, in := range incoming {
		if strings.TrimSpace(in.Title) == "" {
			continue
		}
		in.DOI = model.NormalizeDOI(in.DOI)
		var target *model.Paper
		if in.DOI != "" {
			target = byDOI[in.DOI]
		}
		nt := model.NormalizeTitle(in.Title)
		if target == nil && len(nt) > 20 {
			target = byTitle[nt]
		}
		if target == nil {
			if in.ID == "" {
				in.ID = store.NewID()
			}
			existing = append(existing, in)
			added++
			if in.DOI != "" {
				byDOI[in.DOI] = in
			}
			if len(nt) > 20 {
				byTitle[nt] = in
			}
			continue
		}
		mergeInto(target, in)
		if target.DOI != "" {
			byDOI[target.DOI] = target
		}
	}
	return existing, added
}

func mergeInto(dst, src *model.Paper) {
	if dst.DOI == "" {
		dst.DOI = src.DOI
	}
	if len(src.Abstract) > len(dst.Abstract) {
		dst.Abstract = src.Abstract
	}
	if len(dst.Authors) == 0 {
		dst.Authors = src.Authors
	}
	if dst.Year == 0 {
		dst.Year = src.Year
	}
	if dst.Venue == "" {
		dst.Venue = src.Venue
	}
	if dst.URL == "" {
		dst.URL = src.URL
	}
	if dst.ArxivID == "" {
		dst.ArxivID = src.ArxivID
	}
	if dst.OpenAlexID == "" {
		dst.OpenAlexID = src.OpenAlexID
	}
	if dst.PMID == "" {
		dst.PMID = src.PMID
	}
	if dst.PMCID == "" {
		dst.PMCID = src.PMCID
	}
	if src.Citations > dst.Citations {
		dst.Citations = src.Citations
	}
	dst.OpenAccess = dst.OpenAccess || src.OpenAccess
	dst.Preprint = dst.Preprint || src.Preprint
	if dst.Language == "" {
		dst.Language = src.Language
	}
	if dst.ISSN == "" {
		dst.ISSN = src.ISSN
	}
	dst.NonEnglish = dst.NonEnglish || src.NonEnglish
	dst.PDFURLs = unionStrings(dst.PDFURLs, src.PDFURLs)
	dst.FoundIn = unionStrings(dst.FoundIn, src.FoundIn)
	dst.FoundBy = unionStrings(dst.FoundBy, src.FoundBy)
	dst.Keywords = unionStrings(dst.Keywords, src.Keywords)
}

// preprintPrefixes are DOI prefixes of preprint servers and general-purpose
// repositories whose records are not peer reviewed.
var preprintPrefixes = []string{
	"10.48550/", // arXiv
	"10.2139/",  // SSRN
	"10.31219/", // OSF Preprints
	"10.31234/", // PsyArXiv
	"10.31235/", // SocArXiv
	"10.1101/",  // bioRxiv / medRxiv
	"10.20944/", // Preprints.org
	"10.21203/", // Research Square
	"10.36227/", // TechRxiv
	"10.5281/",  // Zenodo
	"10.6084/",  // figshare
	"10.13140/", // ResearchGate
	"10.22541/", // Authorea
}

// IsPreprint reports whether a record comes from a preprint server or
// repository rather than a peer-reviewed venue.
func IsPreprint(p *model.Paper) bool {
	if p.Preprint {
		return true
	}
	d := strings.ToLower(p.DOI)
	for _, pre := range preprintPrefixes {
		if strings.HasPrefix(d, pre) {
			return true
		}
	}
	v := strings.ToLower(p.Venue)
	for _, w := range []string{"arxiv", "ssrn", "preprint", "biorxiv", "medrxiv", "research square", "techrxiv", "zenodo", "osf preprints", "researchgate"} {
		if strings.Contains(v, w) {
			return true
		}
	}
	return false
}

// FilterPreprints splits papers into (peer-reviewed, preprints).
func FilterPreprints(ps []*model.Paper) (kept, dropped []*model.Paper) {
	for _, p := range ps {
		if IsPreprint(p) {
			p.Preprint = true
			dropped = append(dropped, p)
		} else {
			kept = append(kept, p)
		}
	}
	return
}

// Tag records which query surfaced each paper (before merging) and flags
// preprint records so the UI can show them even when they are not filtered.
func Tag(papers []*model.Paper, label string) {
	for _, p := range papers {
		p.FoundBy = unionStrings(p.FoundBy, []string{label})
		if IsPreprint(p) {
			p.Preprint = true
		}
		if !IsEnglish(p) {
			p.NonEnglish = true
		}
	}
}

// Enricher is implemented by sources that can fill keywords by DOI.
type Enricher interface {
	Enrich(ctx context.Context, papers []*model.Paper, progress func(done, total int)) (int, error)
}

// FirstEnricher returns a keyword-enriching source if registered.
func (r *Registry) FirstEnricher() (Enricher, bool) {
	for _, s := range r.All() {
		if e, ok := s.(Enricher); ok {
			return e, true
		}
	}
	return nil, false
}

// Snowballer is implemented by sources that can follow citation links.
type Snowballer interface {
	// Snowball returns the works referenced by (backward) and citing
	// (forward) the seed papers. Seeds without a resolvable identifier are
	// skipped. limitPerSeed bounds forward citations per seed.
	Snowball(ctx context.Context, seeds []*model.Paper, opts Options) (backward, forward []*model.Paper, err error)
}

// SnowballerFor returns a snowball-capable source, preferring the named
// (project-selected) sources and falling back to any registered one.
func (r *Registry) SnowballerFor(preferred []string) (Snowballer, bool) {
	for _, n := range preferred {
		if s, ok := r.byKey[n]; ok {
			if sb, ok := s.(Snowballer); ok {
				return sb, true
			}
		}
	}
	for _, s := range r.All() {
		if sb, ok := s.(Snowballer); ok {
			return sb, true
		}
	}
	return nil, false
}

func unionStrings(a, b []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range append(append([]string{}, a...), b...) {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// SortByScore orders papers by AI score descending, then citations.
func SortByScore(ps []*model.Paper) {
	sort.SliceStable(ps, func(i, j int) bool {
		if a, b := ps[i].RankScore(), ps[j].RankScore(); a != b {
			return a > b
		}
		return ps[i].Citations > ps[j].Citations
	})
}
