package sources

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"literaturereview/internal/model"
)

// SemanticScholar queries the Semantic Scholar Graph API.
type SemanticScholar struct {
	client httpClient
	apiKey string
	ua     string
	lim    limiter
}

func (s *SemanticScholar) Name() string  { return "semanticscholar" }
func (s *SemanticScholar) Label() string { return "Semantic Scholar" }
func (s *SemanticScholar) Syntax() string {
	return "plain keywords; supports + (AND), | (OR), - (NOT), \"exact phrase\" and ~ for fuzzy; keep it short (under ~12 terms)"
}

func (s *SemanticScholar) Search(ctx context.Context, query string, opts Options) (*Result, error) {
	// The anonymous pool is shared by everyone and frequently returns 429;
	// pace conservatively. A free key (S2_API_KEY) lifts this.
	s.lim.interval = 3100 * time.Millisecond
	if s.apiKey != "" {
		s.lim.interval = 150 * time.Millisecond
	}
	if err := s.lim.wait(ctx); err != nil {
		return nil, err
	}
	q := url.Values{}
	q.Set("query", query)
	q.Set("limit", fmt.Sprint(clampLimit(opts.Limit, 100)))
	q.Set("fields", "title,abstract,year,venue,authors,externalIds,citationCount,isOpenAccess,openAccessPdf,url")
	if opts.YearFrom > 0 || opts.YearTo > 0 {
		yr := ""
		if opts.YearFrom > 0 {
			yr = fmt.Sprint(opts.YearFrom)
		}
		yr += "-"
		if opts.YearTo > 0 {
			yr += fmt.Sprint(opts.YearTo)
		}
		q.Set("year", yr)
	}
	headers := map[string]string{}
	if s.apiKey != "" {
		headers["x-api-key"] = s.apiKey
	}
	var resp struct {
		Total int `json:"total"`
		Data  []struct {
			Title    string `json:"title"`
			Abstract string `json:"abstract"`
			Year     int    `json:"year"`
			Venue    string `json:"venue"`
			URL      string `json:"url"`
			Authors  []struct {
				Name string `json:"name"`
			} `json:"authors"`
			ExternalIDs map[string]any `json:"externalIds"`
			Citations   int            `json:"citationCount"`
			IsOA        bool           `json:"isOpenAccess"`
			OAPDF       *struct {
				URL string `json:"url"`
			} `json:"openAccessPdf"`
		} `json:"data"`
	}
	reqURL := "https://api.semanticscholar.org/graph/v1/paper/search?" + q.Encode()
	res := &Result{URL: reqURL}
	if err := getJSON(ctx, s.client, reqURL, s.ua, headers, &resp); err != nil {
		if s.apiKey == "" && strings.Contains(err.Error(), "429") {
			err = fmt.Errorf("%w — the anonymous Semantic Scholar pool is rate-limited; request a free key at https://www.semanticscholar.org/product/api and set S2_API_KEY", err)
		}
		return res, err
	}
	res.Total = resp.Total
	var out []*model.Paper
	for _, d := range resp.Data {
		p := &model.Paper{
			Title:      strings.TrimSpace(d.Title),
			Abstract:   d.Abstract,
			Year:       d.Year,
			Venue:      d.Venue,
			URL:        d.URL,
			Citations:  d.Citations,
			OpenAccess: d.IsOA,
			FoundIn:    []string{s.Name()},
		}
		for _, a := range d.Authors {
			p.Authors = append(p.Authors, a.Name)
		}
		if v, ok := d.ExternalIDs["DOI"].(string); ok {
			p.DOI = model.NormalizeDOI(v)
		}
		if v, ok := d.ExternalIDs["ArXiv"].(string); ok {
			p.ArxivID = v
		}
		if v, ok := d.ExternalIDs["PubMed"].(string); ok {
			p.PMID = v
		}
		if v, ok := d.ExternalIDs["PubMedCentral"].(string); ok {
			p.PMCID = "PMC" + strings.TrimPrefix(v, "PMC")
		}
		if d.OAPDF != nil && d.OAPDF.URL != "" {
			p.PDFURLs = append(p.PDFURLs, d.OAPDF.URL)
		}
		out = append(out, p)
	}
	res.Papers = out
	return res, nil
}
