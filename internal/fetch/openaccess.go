package fetch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"literaturereview/internal/model"
)

// MetadataOA tries the open-access PDF links the databases reported.
type MetadataOA struct {
	client *http.Client
	ua     string
}

func (f *MetadataOA) Name() string { return "open-access metadata" }

func (f *MetadataOA) Fetch(ctx context.Context, p *model.Paper) (*Result, error) {
	if len(p.PDFURLs) == 0 {
		return nil, fmt.Errorf("%w: no OA links in metadata", ErrNotApplicable)
	}
	var errs []string
	for i, u := range p.PDFURLs {
		if i >= 4 {
			break
		}
		r, err := fetchPDF(ctx, f.client, u, f.ua, nil)
		if err == nil {
			return r, nil
		}
		errs = append(errs, err.Error())
	}
	return nil, fmt.Errorf("%s", strings.Join(errs, "; "))
}

// EuropePMC downloads from Europe PMC by PMCID.
type EuropePMC struct {
	client *http.Client
	ua     string
}

func (f *EuropePMC) Name() string { return "Europe PMC" }

func (f *EuropePMC) Fetch(ctx context.Context, p *model.Paper) (*Result, error) {
	if p.PMCID == "" {
		return nil, fmt.Errorf("%w: no PMCID", ErrNotApplicable)
	}
	id := "PMC" + strings.TrimPrefix(strings.ToUpper(p.PMCID), "PMC")
	return fetchPDF(ctx, f.client, "https://europepmc.org/backend/ptpmcrender.fcgi?accid="+id+"&blobtype=pdf", f.ua, nil)
}

// Unpaywall asks api.unpaywall.org for legal OA copies by DOI.
type Unpaywall struct {
	client *http.Client
	ua     string
	email  string
}

func (f *Unpaywall) Name() string { return "Unpaywall" }

func (f *Unpaywall) Fetch(ctx context.Context, p *model.Paper) (*Result, error) {
	if p.DOI == "" {
		return nil, fmt.Errorf("%w: no DOI", ErrNotApplicable)
	}
	if f.email == "" {
		return nil, fmt.Errorf("%w: CONTACT_EMAIL not configured", ErrNotApplicable)
	}
	api := "https://api.unpaywall.org/v2/" + url.PathEscape(p.DOI) + "?email=" + url.QueryEscape(f.email)
	pg, err := get(ctx, f.client, api, f.ua, nil)
	if err != nil {
		return nil, err
	}
	if pg.status == 404 {
		return nil, fmt.Errorf("DOI unknown to Unpaywall")
	}
	if pg.status != 200 {
		return nil, fmt.Errorf("Unpaywall HTTP %d", pg.status)
	}
	var body struct {
		IsOA        bool  `json:"is_oa"`
		BestOA      *loc  `json:"best_oa_location"`
		OALocations []loc `json:"oa_locations"`
	}
	if err := json.Unmarshal(pg.body, &body); err != nil {
		return nil, fmt.Errorf("decode Unpaywall: %w", err)
	}
	if !body.IsOA {
		return nil, fmt.Errorf("no open-access copy known")
	}
	var cands []string
	seen := map[string]bool{}
	add := func(l *loc) {
		if l == nil {
			return
		}
		for _, u := range []string{l.PDF, l.URL} {
			if u != "" && !seen[u] {
				seen[u] = true
				cands = append(cands, u)
			}
		}
	}
	add(body.BestOA)
	for i := range body.OALocations {
		add(&body.OALocations[i])
	}
	var errs []string
	for i, u := range cands {
		if i >= 4 {
			break
		}
		r, err := fetchPDF(ctx, f.client, u, f.ua, nil)
		if err == nil {
			return r, nil
		}
		errs = append(errs, err.Error())
	}
	return nil, fmt.Errorf("OA locations failed: %s", strings.Join(errs, "; "))
}

type loc struct {
	URL string `json:"url"`
	PDF string `json:"url_for_pdf"`
}
