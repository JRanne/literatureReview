package fetch

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"literaturereview/internal/model"
)

// Publisher resolves the DOI at the publisher. It succeeds for open articles
// and, when the machine is inside a campus network or VPN, for subscribed
// content (the "paywall" attempt).
type Publisher struct {
	client *http.Client
	ua     string
}

func (f *Publisher) Name() string { return "publisher (direct)" }

func (f *Publisher) Fetch(ctx context.Context, p *model.Paper) (*Result, error) {
	target := landingURL(p)
	if target == "" {
		return nil, fmt.Errorf("%w: no DOI or URL", ErrNotApplicable)
	}
	hdr := browserHeaders()
	var errs []string
	// 1. publisher-specific direct PDF endpoints
	pats := PatternURLs(ctx, f.client, p, hdr.Get("User-Agent"))
	for _, u := range pats {
		r, err := fetchPDF(ctx, f.client, u, f.ua, hdr)
		if err == nil {
			return r, nil
		}
		errs = append(errs, err.Error())
	}
	// 2. generic: resolve DOI, parse landing page for PDF links
	r, err := fetchPDF(ctx, f.client, target, f.ua, hdr)
	if err == nil {
		return r, nil
	}
	errs = append(errs, err.Error())
	return nil, fmt.Errorf("%s; %s", describe(pats), strings.Join(errs, "; "))
}

func landingURL(p *model.Paper) string {
	if p.DOI != "" {
		return "https://doi.org/" + p.DOI
	}
	return p.URL
}
