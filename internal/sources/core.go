package sources

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"literaturereview/internal/model"
)

// Core queries the CORE v3 API (requires an API key).
type Core struct {
	client httpClient
	apiKey string
	ua     string
	lim    limiter
}

func (s *Core) Name() string  { return "core" }
func (s *Core) Label() string { return "CORE" }
func (s *Core) Syntax() string {
	return `Lucene-style: keywords, "phrases", AND/OR/NOT, field filters like title:(...) abstract:(...); year filtering is added automatically`
}

func (s *Core) Search(ctx context.Context, query string, opts Options) (*Result, error) {
	s.lim.interval = 1500 * time.Millisecond
	if err := s.lim.wait(ctx); err != nil {
		return nil, err
	}
	full := "(" + query + ")"
	if opts.YearFrom > 0 {
		full += fmt.Sprintf(" AND yearPublished>=%d", opts.YearFrom)
	}
	if opts.YearTo > 0 {
		full += fmt.Sprintf(" AND yearPublished<=%d", opts.YearTo)
	}
	q := url.Values{}
	q.Set("q", full)
	q.Set("limit", fmt.Sprint(clampLimit(opts.Limit, 100)))
	var resp struct {
		TotalHits int `json:"totalHits"`
		Results   []struct {
			Title    string `json:"title"`
			Abstract string `json:"abstract"`
			Year     int    `json:"yearPublished"`
			DOI      string `json:"doi"`
			Download string `json:"downloadUrl"`
			Authors  []struct {
				Name string `json:"name"`
			} `json:"authors"`
			Journals []struct {
				Title string `json:"title"`
			} `json:"journals"`
			Publisher string `json:"publisher"`
			Citations int    `json:"citationCount"`
			ArxivID   string `json:"arxivId"`
			PubmedID  string `json:"pubmedId"`
			Links     []struct {
				Type string `json:"type"`
				URL  string `json:"url"`
			} `json:"links"`
			Language struct {
				Code string `json:"code"`
			} `json:"language"`
		} `json:"results"`
	}
	headers := map[string]string{"Authorization": "Bearer " + s.apiKey}
	reqURL := "https://api.core.ac.uk/v3/search/works?" + q.Encode()
	res := &Result{URL: reqURL}
	if err := getJSON(ctx, s.client, reqURL, s.ua, headers, &resp); err != nil {
		return res, err
	}
	res.Total = resp.TotalHits
	var out []*model.Paper
	for _, r := range resp.Results {
		p := &model.Paper{
			Title:      strings.TrimSpace(r.Title),
			Abstract:   strings.TrimSpace(r.Abstract),
			Year:       r.Year,
			DOI:        model.NormalizeDOI(r.DOI),
			Citations:  r.Citations,
			ArxivID:    r.ArxivID,
			PMID:       r.PubmedID,
			OpenAccess: r.Download != "",
			Language:   r.Language.Code,
			FoundIn:    []string{s.Name()},
		}
		if len(r.Journals) > 0 {
			p.Venue = r.Journals[0].Title
		} else {
			p.Venue = r.Publisher
		}
		for _, a := range r.Authors {
			p.Authors = append(p.Authors, a.Name)
		}
		if r.Download != "" {
			p.PDFURLs = append(p.PDFURLs, r.Download)
		}
		for _, l := range r.Links {
			if l.Type == "download" {
				p.PDFURLs = append(p.PDFURLs, l.URL)
			}
			if l.Type == "display" && p.URL == "" {
				p.URL = l.URL
			}
		}
		out = append(out, p)
	}
	res.Papers = out
	return res, nil
}
