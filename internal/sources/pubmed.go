package sources

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"literaturereview/internal/model"
)

// PubMed queries NCBI E-utilities.
type PubMed struct {
	client httpClient
	email  string
	ua     string
	lim    limiter
}

func (s *PubMed) Name() string  { return "pubmed" }
func (s *PubMed) Label() string { return "PubMed" }
func (s *PubMed) Syntax() string {
	return `PubMed boolean syntax with field tags, e.g. ("artificial intelligence"[tiab] OR "machine learning"[tiab]) AND adoption[tiab] AND hospital*[tiab]; MeSH terms via [mh]`
}

type pubmedSet struct {
	Articles []struct {
		Citation struct {
			PMID    string `xml:"PMID"`
			Article struct {
				Language string `xml:"Language"`
				Title    string `xml:"ArticleTitle"`
				Abstract struct {
					Texts []struct {
						Label string `xml:"Label,attr"`
						Text  string `xml:",innerxml"`
					} `xml:"AbstractText"`
				} `xml:"Abstract"`
				Authors []struct {
					Last    string `xml:"LastName"`
					Fore    string `xml:"ForeName"`
					Collect string `xml:"CollectiveName"`
				} `xml:"AuthorList>Author"`
				Journal struct {
					Title string `xml:"Title"`
					Issue struct {
						PubDate struct {
							Year        string `xml:"Year"`
							MedlineDate string `xml:"MedlineDate"`
						} `xml:"PubDate"`
					} `xml:"JournalIssue"`
				} `xml:"Journal"`
			} `xml:"Article"`
		} `xml:"MedlineCitation"`
		Data struct {
			IDs []struct {
				Type string `xml:"IdType,attr"`
				Val  string `xml:",chardata"`
			} `xml:"ArticleIdList>ArticleId"`
		} `xml:"PubmedData"`
	} `xml:"PubmedArticle"`
}

func (s *PubMed) base(q url.Values) {
	q.Set("tool", "litreview")
	if s.email != "" {
		q.Set("email", s.email)
	}
}

func (s *PubMed) Search(ctx context.Context, query string, opts Options) (*Result, error) {
	s.lim.interval = 400 * time.Millisecond // ≤3 req/s without an API key
	if err := s.lim.wait(ctx); err != nil {
		return nil, err
	}
	q := url.Values{}
	q.Set("db", "pubmed")
	q.Set("term", query)
	q.Set("retmax", fmt.Sprint(clampLimit(opts.Limit, 200)))
	q.Set("retmode", "json")
	q.Set("sort", "relevance")
	if opts.YearFrom > 0 || opts.YearTo > 0 {
		from, to := opts.YearFrom, opts.YearTo
		if from == 0 {
			from = 1900
		}
		if to == 0 {
			to = time.Now().Year() + 1
		}
		q.Set("datetype", "pdat")
		q.Set("mindate", fmt.Sprint(from))
		q.Set("maxdate", fmt.Sprint(to))
	}
	s.base(q)
	var es struct {
		Result struct {
			Count  string   `json:"count"`
			IDList []string `json:"idlist"`
		} `json:"esearchresult"`
	}
	reqURL := "https://eutils.ncbi.nlm.nih.gov/entrez/eutils/esearch.fcgi?" + q.Encode()
	res := &Result{URL: reqURL}
	if err := getJSON(ctx, s.client, reqURL, s.ua, nil, &es); err != nil {
		return res, err
	}
	res.Total, _ = strconv.Atoi(es.Result.Count)
	if len(es.Result.IDList) == 0 {
		return res, nil
	}
	if err := s.lim.wait(ctx); err != nil {
		return res, err
	}
	fq := url.Values{}
	fq.Set("db", "pubmed")
	fq.Set("id", strings.Join(es.Result.IDList, ","))
	fq.Set("retmode", "xml")
	s.base(fq)
	body, err := doGet(ctx, s.client, "https://eutils.ncbi.nlm.nih.gov/entrez/eutils/efetch.fcgi?"+fq.Encode(), s.ua, nil)
	if err != nil {
		return res, err
	}
	var set pubmedSet
	if err := xml.Unmarshal(body, &set); err != nil {
		return res, fmt.Errorf("decode PubMed XML: %w", err)
	}
	var out []*model.Paper
	for _, a := range set.Articles {
		art := a.Citation.Article
		p := &model.Paper{
			Title:    stripTags(art.Title),
			Language: strings.ToLower(art.Language),
			Venue:    art.Journal.Title,
			PMID:     strings.TrimSpace(a.Citation.PMID),
			FoundIn:  []string{s.Name()},
		}
		p.URL = "https://pubmed.ncbi.nlm.nih.gov/" + p.PMID + "/"
		var abs []string
		for _, t := range art.Abstract.Texts {
			txt := stripTags(t.Text)
			if t.Label != "" {
				txt = t.Label + ": " + txt
			}
			abs = append(abs, txt)
		}
		p.Abstract = strings.Join(abs, " ")
		for _, au := range art.Authors {
			switch {
			case au.Last != "":
				p.Authors = append(p.Authors, strings.TrimSpace(au.Fore+" "+au.Last))
			case au.Collect != "":
				p.Authors = append(p.Authors, au.Collect)
			}
		}
		yr := art.Journal.Issue.PubDate.Year
		if yr == "" && len(art.Journal.Issue.PubDate.MedlineDate) >= 4 {
			yr = art.Journal.Issue.PubDate.MedlineDate[:4]
		}
		p.Year, _ = strconv.Atoi(yr)
		for _, id := range a.Data.IDs {
			switch id.Type {
			case "doi":
				p.DOI = model.NormalizeDOI(id.Val)
			case "pmc":
				p.PMCID = strings.TrimSpace(id.Val)
				p.OpenAccess = true
			}
		}
		out = append(out, p)
	}
	res.Papers = out
	return res, nil
}
