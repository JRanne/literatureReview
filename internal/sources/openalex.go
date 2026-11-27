package sources

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"literaturereview/internal/model"
)

// OpenAlex queries https://api.openalex.org.
type OpenAlex struct {
	client httpClient
	email  string
	ua     string
	apiKey string
	lim    limiter
}

// auth adds mailto (polite pool) and api_key when configured.
func (s *OpenAlex) auth(q url.Values) {
	if s.email != "" {
		q.Set("mailto", s.email)
	}
	if s.apiKey != "" {
		q.Set("api_key", s.apiKey)
	}
}

func (s *OpenAlex) Name() string  { return "openalex" }
func (s *OpenAlex) Label() string { return "OpenAlex" }
func (s *OpenAlex) Syntax() string {
	return "plain natural-language keywords; no boolean operators needed (full-text relevance search over title, abstract and fulltext)"
}

type oaWork struct {
	ID              string `json:"id"`
	DOI             string `json:"doi"`
	Type            string `json:"type"`
	Language        string `json:"language"`
	Title           string `json:"title"`
	PublicationYear int    `json:"publication_year"`
	Authorships     []struct {
		Author struct {
			DisplayName string `json:"display_name"`
		} `json:"author"`
	} `json:"authorships"`
	PrimaryLocation struct {
		Source struct {
			DisplayName string `json:"display_name"`
			Type        string `json:"type"`
			ISSNL       string `json:"issn_l"`
		} `json:"source"`
		LandingPageURL string `json:"landing_page_url"`
	} `json:"primary_location"`
	AbstractInvertedIndex map[string][]int `json:"abstract_inverted_index"`
	CitedByCount          int              `json:"cited_by_count"`
	OpenAccess            struct {
		IsOA  bool   `json:"is_oa"`
		OAURL string `json:"oa_url"`
	} `json:"open_access"`
	BestOALocation *struct {
		PDFURL string `json:"pdf_url"`
	} `json:"best_oa_location"`
	IDs struct {
		PMID  string `json:"pmid"`
		PMCID string `json:"pmcid"`
	} `json:"ids"`
	ReferencedWorks []string `json:"referenced_works"`
	Keywords        []struct {
		DisplayName string  `json:"display_name"`
		Score       float64 `json:"score"`
	} `json:"keywords"`
	Concepts []struct {
		DisplayName string  `json:"display_name"`
		Level       int     `json:"level"`
		Score       float64 `json:"score"`
	} `json:"concepts"`
}

// keywordsOf returns OpenAlex's curated keywords. The older "concepts"
// taxonomy is deliberately ignored: it is noisy (e.g. "generative grammar"
// for generative AI papers).
func keywordsOf(w oaWork) []string {
	var out []string
	for _, k := range w.Keywords {
		if k.DisplayName != "" && k.Score >= 0.3 {
			out = append(out, k.DisplayName)
		}
	}
	return out
}

func (s *OpenAlex) Search(ctx context.Context, query string, opts Options) (*Result, error) {
	s.lim.interval = 150 * time.Millisecond
	if err := s.lim.wait(ctx); err != nil {
		return nil, err
	}
	q := url.Values{}
	q.Set("search", query)
	q.Set("per-page", fmt.Sprint(clampLimit(opts.Limit, 200)))
	q.Set("select", oaSelect)
	var filters []string
	if opts.YearFrom > 0 || opts.YearTo > 0 {
		from, to := opts.YearFrom, opts.YearTo
		if from == 0 {
			from = 1900
		}
		if to == 0 {
			to = time.Now().Year() + 1
		}
		filters = append(filters, fmt.Sprintf("publication_year:%d-%d", from, to))
	}
	if len(filters) > 0 {
		q.Set("filter", strings.Join(filters, ","))
	}
	s.auth(q)
	var resp struct {
		Meta struct {
			Count int `json:"count"`
		} `json:"meta"`
		Results []oaWork `json:"results"`
	}
	reqURL := "https://api.openalex.org/works?" + q.Encode()
	res := &Result{URL: reqURL}
	if err := getJSON(ctx, s.client, reqURL, s.ua, nil, &resp); err != nil {
		return res, err
	}
	res.Total = resp.Meta.Count
	res.Papers = s.toPapers(resp.Results)
	return res, nil
}

const oaSelect = "id,doi,type,language,keywords,title,publication_year,authorships,primary_location,abstract_inverted_index,cited_by_count,open_access,best_oa_location,ids"

func (s *OpenAlex) toPapers(works []oaWork) []*model.Paper {
	var out []*model.Paper
	for _, w := range works {
		p := &model.Paper{
			DOI:        model.NormalizeDOI(w.DOI),
			Title:      strings.TrimSpace(w.Title),
			Year:       w.PublicationYear,
			Venue:      w.PrimaryLocation.Source.DisplayName,
			Abstract:   ReconstructAbstract(w.AbstractInvertedIndex),
			URL:        w.PrimaryLocation.LandingPageURL,
			Citations:  w.CitedByCount,
			OpenAccess: w.OpenAccess.IsOA,
			FoundIn:    []string{s.Name()},
			PMID:       strings.TrimPrefix(w.IDs.PMID, "https://pubmed.ncbi.nlm.nih.gov/"),
			PMCID:      strings.TrimPrefix(w.IDs.PMCID, "https://www.ncbi.nlm.nih.gov/pmc/articles/"),
		}
		if p.URL == "" {
			p.URL = w.ID
		}
		for _, a := range w.Authorships {
			if a.Author.DisplayName != "" {
				p.Authors = append(p.Authors, a.Author.DisplayName)
			}
		}
		if w.BestOALocation != nil && w.BestOALocation.PDFURL != "" {
			p.PDFURLs = append(p.PDFURLs, w.BestOALocation.PDFURL)
		}
		if w.OpenAccess.OAURL != "" {
			p.PDFURLs = append(p.PDFURLs, w.OpenAccess.OAURL)
		}
		if strings.Contains(p.DOI, "10.48550/arxiv.") {
			p.ArxivID = strings.TrimPrefix(p.DOI, "10.48550/arxiv.")
		}
		if w.ID != "" {
			p.OpenAlexID = strings.TrimPrefix(w.ID, "https://openalex.org/")
		}
		if w.Type == "preprint" || w.PrimaryLocation.Source.Type == "repository" && w.Type != "article" {
			p.Preprint = true
		}
		p.Language = w.Language
		p.Keywords = keywordsOf(w)
		p.ISSN = w.PrimaryLocation.Source.ISSNL
		out = append(out, p)
	}
	return out
}

// work fetches one OpenAlex work by DOI, including its reference list.
func (s *OpenAlex) work(ctx context.Context, doi string) (*oaWork, error) {
	q := url.Values{}
	q.Set("select", oaSelect+",referenced_works")
	s.auth(q)
	var w oaWork
	if err := getJSON(ctx, s.client, "https://api.openalex.org/works/https://doi.org/"+url.PathEscape(doi)+"?"+q.Encode(), s.ua, nil, &w); err != nil {
		return nil, err
	}
	return &w, nil
}

// Enrich fetches OpenAlex keywords/concepts (and language) for papers that
// lack them, by DOI. Papers without a DOI or unknown to OpenAlex are skipped.
func (s *OpenAlex) Enrich(ctx context.Context, papers []*model.Paper, progress func(done, total int)) (int, error) {
	s.lim.interval = 150 * time.Millisecond
	filled := 0
	for i, p := range papers {
		if progress != nil {
			progress(i, len(papers))
		}
		if p.DOI == "" {
			continue
		}
		if err := s.lim.wait(ctx); err != nil {
			return filled, err
		}
		w, err := s.work(ctx, p.DOI)
		if err != nil {
			continue
		}
		p.Keywords = keywordsOf(*w)
		if p.Language == "" {
			p.Language = w.Language
		}
		if len(p.Keywords) > 0 {
			filled++
		}
	}
	if progress != nil {
		progress(len(papers), len(papers))
	}
	return filled, nil
}

// Snowball implements Snowballer via OpenAlex referenced_works and cites:.
func (s *OpenAlex) Snowball(ctx context.Context, seeds []*model.Paper, opts Options) ([]*model.Paper, []*model.Paper, error) {
	s.lim.interval = 150 * time.Millisecond
	var backward, forward []*model.Paper
	var refIDs []string
	seenRef := map[string]bool{}
	type seed struct {
		id    string
		title string
	}
	var resolved []seed
	for _, sp := range seeds {
		if sp.DOI == "" {
			continue
		}
		if err := s.lim.wait(ctx); err != nil {
			return nil, nil, err
		}
		w, err := s.work(ctx, sp.DOI)
		if err != nil {
			continue // seed unknown to OpenAlex
		}
		id := strings.TrimPrefix(w.ID, "https://openalex.org/")
		resolved = append(resolved, seed{id: id, title: sp.Title})
		for _, r := range w.ReferencedWorks {
			r = strings.TrimPrefix(r, "https://openalex.org/")
			if !seenRef[r] {
				seenRef[r] = true
				refIDs = append(refIDs, r)
			}
		}
	}
	if len(resolved) == 0 {
		return nil, nil, fmt.Errorf("none of the %d seed papers could be resolved in OpenAlex (DOI needed)", len(seeds))
	}
	// Backward: fetch referenced works in batches of 50.
	for i := 0; i < len(refIDs); i += 50 {
		end := i + 50
		if end > len(refIDs) {
			end = len(refIDs)
		}
		if err := s.lim.wait(ctx); err != nil {
			return backward, forward, err
		}
		q := url.Values{}
		q.Set("filter", "ids.openalex:"+strings.Join(refIDs[i:end], "|"))
		q.Set("per-page", "50")
		q.Set("select", oaSelect)
		s.auth(q)
		var resp struct {
			Results []oaWork `json:"results"`
		}
		if err := getJSON(ctx, s.client, "https://api.openalex.org/works?"+q.Encode(), s.ua, nil, &resp); err != nil {
			return backward, forward, err
		}
		for _, p := range s.toPapers(resp.Results) {
			if inYearRange(p.Year, opts) {
				p.FoundIn = []string{s.Name() + " (reference)"}
				backward = append(backward, p)
			}
		}
	}
	// Forward: works citing each seed, most cited first.
	for _, sd := range resolved {
		if err := s.lim.wait(ctx); err != nil {
			return backward, forward, err
		}
		q := url.Values{}
		q.Set("filter", "cites:"+sd.id)
		q.Set("per-page", fmt.Sprint(clampLimit(opts.Limit, 100)))
		q.Set("sort", "cited_by_count:desc")
		q.Set("select", oaSelect)
		s.auth(q)
		var resp struct {
			Results []oaWork `json:"results"`
		}
		if err := getJSON(ctx, s.client, "https://api.openalex.org/works?"+q.Encode(), s.ua, nil, &resp); err != nil {
			return backward, forward, err
		}
		ps := s.toPapers(resp.Results)
		for _, p := range ps {
			if inYearRange(p.Year, opts) {
				p.FoundIn = []string{s.Name() + " (citation)"}
				p.FoundBy = []string{"cites: " + truncate(sd.title, 60)}
				forward = append(forward, p)
			}
		}
	}
	return backward, forward, nil
}

// ReconstructAbstract rebuilds text from OpenAlex's inverted index.
func ReconstructAbstract(idx map[string][]int) string {
	if len(idx) == 0 {
		return ""
	}
	type pos struct {
		i int
		w string
	}
	var ps []pos
	for w, positions := range idx {
		for _, i := range positions {
			ps = append(ps, pos{i, w})
		}
	}
	sort.Slice(ps, func(a, b int) bool { return ps[a].i < ps[b].i })
	words := make([]string, 0, len(ps))
	for _, p := range ps {
		words = append(words, p.w)
	}
	return strings.Join(words, " ")
}

func clampLimit(n, max int) int {
	if n <= 0 {
		return 25
	}
	if n > max {
		return max
	}
	return n
}

// Lookup resolves one reference to a paper via OpenAlex. ref may be a DOI
// (bare or as a doi.org URL), an OpenAlex work id or URL, or a free-text
// title (the best search hit is returned).
func (s *OpenAlex) Lookup(ctx context.Context, ref string) (*model.Paper, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, fmt.Errorf("empty reference")
	}
	q := url.Values{}
	q.Set("select", oaSelect)
	s.auth(q)
	var works []oaWork
	switch {
	case strings.Contains(ref, "10.") && strings.Contains(ref, "/") && !strings.Contains(ref, " "):
		doi := model.NormalizeDOI(ref)
		var w oaWork
		if err := getJSON(ctx, s.client, "https://api.openalex.org/works/https://doi.org/"+url.PathEscape(doi)+"?"+q.Encode(), s.ua, nil, &w); err != nil {
			return nil, fmt.Errorf("DOI %s: %w", doi, err)
		}
		works = []oaWork{w}
	case regexp.MustCompile(`(?i)^(https?://openalex\.org/)?W\d+$`).MatchString(ref):
		id := ref[strings.LastIndex(ref, "/")+1:]
		var w oaWork
		if err := getJSON(ctx, s.client, "https://api.openalex.org/works/"+id+"?"+q.Encode(), s.ua, nil, &w); err != nil {
			return nil, fmt.Errorf("OpenAlex %s: %w", id, err)
		}
		works = []oaWork{w}
	default:
		q.Set("search", ref)
		q.Set("per-page", "1")
		var resp struct {
			Results []oaWork `json:"results"`
		}
		if err := getJSON(ctx, s.client, "https://api.openalex.org/works?"+q.Encode(), s.ua, nil, &resp); err != nil {
			return nil, fmt.Errorf("title search %q: %w", ref, err)
		}
		if len(resp.Results) == 0 {
			return nil, fmt.Errorf("no OpenAlex record matches %q", ref)
		}
		works = resp.Results[:1]
	}
	ps := s.toPapers(works)
	if len(ps) == 0 || ps[0].Title == "" {
		return nil, fmt.Errorf("OpenAlex returned no usable record for %q", ref)
	}
	return ps[0], nil
}
