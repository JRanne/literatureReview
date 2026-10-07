package sources

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"literaturereview/internal/model"
)

// CrossRef queries https://api.crossref.org.
type CrossRef struct {
	client httpClient
	email  string
	ua     string
	lim    limiter
}

func (s *CrossRef) Name() string  { return "crossref" }
func (s *CrossRef) Label() string { return "CrossRef" }
func (s *CrossRef) Syntax() string {
	return "plain keywords matched against bibliographic metadata (title, abstract where deposited); no boolean operators"
}

func (s *CrossRef) Search(ctx context.Context, query string, opts Options) (*Result, error) {
	s.lim.interval = 1100 * time.Millisecond
	if err := s.lim.wait(ctx); err != nil {
		return nil, err
	}
	q := url.Values{}
	q.Set("query", query)
	q.Set("rows", fmt.Sprint(clampLimit(opts.Limit, 100)))
	// NB: "language" is not a selectable field in CrossRef's API (400).
	q.Set("select", "DOI,title,author,issued,container-title,abstract,is-referenced-by-count,URL,link,type,ISSN")
	var filters []string
	if opts.YearFrom > 0 {
		filters = append(filters, fmt.Sprintf("from-pub-date:%d", opts.YearFrom))
	}
	if opts.YearTo > 0 {
		filters = append(filters, fmt.Sprintf("until-pub-date:%d", opts.YearTo))
	}
	if len(filters) > 0 {
		q.Set("filter", strings.Join(filters, ","))
	}
	if s.email != "" {
		q.Set("mailto", s.email)
	}
	var resp struct {
		Message struct {
			Total int `json:"total-results"`
			Items []struct {
				DOI      string   `json:"DOI"`
				Language string   `json:"language"`
				ISSN     []string `json:"ISSN"`
				Title    []string `json:"title"`
				Author   []struct {
					Given  string `json:"given"`
					Family string `json:"family"`
					Name   string `json:"name"`
				} `json:"author"`
				Issued struct {
					DateParts [][]int `json:"date-parts"`
				} `json:"issued"`
				Container []string `json:"container-title"`
				Abstract  string   `json:"abstract"`
				Cited     int      `json:"is-referenced-by-count"`
				URL       string   `json:"URL"`
				Link      []struct {
					URL         string `json:"URL"`
					ContentType string `json:"content-type"`
				} `json:"link"`
			} `json:"items"`
		} `json:"message"`
	}
	reqURL := "https://api.crossref.org/works?" + q.Encode()
	res := &Result{URL: reqURL}
	if err := getJSON(ctx, s.client, reqURL, s.ua, nil, &resp); err != nil {
		return res, err
	}
	// CrossRef's total-results counts loose relevance matches (often millions)
	// and is not comparable to other databases' hit counts; leave it unset.
	_ = resp.Message.Total
	var out []*model.Paper
	for _, it := range resp.Message.Items {
		if len(it.Title) == 0 {
			continue
		}
		p := &model.Paper{
			DOI:       model.NormalizeDOI(it.DOI),
			Title:     strings.TrimSpace(it.Title[0]),
			Abstract:  stripTags(it.Abstract),
			URL:       it.URL,
			Citations: it.Cited,
			Language:  it.Language,
			FoundIn:   []string{s.Name()},
		}
		if len(it.Container) > 0 {
			p.Venue = it.Container[0]
		}
		if len(it.ISSN) > 0 {
			p.ISSN = it.ISSN[0]
		}
		if len(it.Issued.DateParts) > 0 && len(it.Issued.DateParts[0]) > 0 {
			p.Year = it.Issued.DateParts[0][0]
		}
		for _, a := range it.Author {
			switch {
			case a.Family != "" && a.Given != "":
				p.Authors = append(p.Authors, a.Given+" "+a.Family)
			case a.Family != "":
				p.Authors = append(p.Authors, a.Family)
			case a.Name != "":
				p.Authors = append(p.Authors, a.Name)
			}
		}
		for _, l := range it.Link {
			if l.ContentType == "application/pdf" || strings.HasSuffix(strings.ToLower(l.URL), ".pdf") {
				p.PDFURLs = append(p.PDFURLs, l.URL)
			}
		}
		out = append(out, p)
	}
	res.Papers = out
	return res, nil
}
