package fetch

import (
	"context"
	"fmt"
	"strings"
	"time"

	"literaturereview/internal/model"
)

// PDFBrowser is implemented by browserlogin.Downloader: load a URL in a real
// (headless) browser with the user's profile and capture the PDF response.
type PDFBrowser interface {
	Fetch(ctx context.Context, url string) (data []byte, finalURL string, err error)
}

// Browser is the last-resort stage: a headless Chromium with the user's
// logged-in profile follows JavaScript redirects, clicks download links and
// passes publisher bot checks that plain HTTP clients fail. It tries the
// publisher pattern URLs and the landing page, through the proxy when one is
// configured and directly otherwise.
type Browser struct {
	B      PDFBrowser
	Prefix func() string // current proxy prefix ("" when none)
	ua     string
}

func (f *Browser) Name() string { return "headless browser" }

// Timeout gives this stage a longer budget: papers queue for the single
// browser and each tries up to two pages.
func (f *Browser) Timeout() time.Duration { return 4 * time.Minute }

func (f *Browser) Fetch(ctx context.Context, p *model.Paper) (*Result, error) {
	if f.B == nil {
		return nil, fmt.Errorf("%w: no Chromium-based browser available", ErrNotApplicable)
	}
	target := landingURL(p)
	if target == "" {
		return nil, fmt.Errorf("%w: no DOI or URL", ErrNotApplicable)
	}
	prefix := ""
	if f.Prefix != nil {
		prefix = f.Prefix()
	}
	// One attempt per paper keeps a large retry bounded (~40 s each): the
	// best publisher pattern URL through the proxy when the host is a
	// subscription publisher, otherwise the landing page directly
	// (JavaScript download links, bot checks, open content on unknown hosts).
	pats := PatternURLs(ctx, newClient(nil), p, browserHeaders().Get("User-Agent"))
	var attempts []string
	switch {
	case prefix != "" && len(pats) > 0 && proxiable(pats[0]):
		attempts = []string{proxied(prefix, pats[0])}
	case prefix != "" && len(pats) == 0 && proxiable(target):
		attempts = []string{proxied(prefix, target)}
	case len(pats) > 0 && !strings.Contains(pats[0], "ssrn.com"):
		attempts = []string{pats[0]}
	default:
		attempts = []string{target}
	}
	var errs []string
	seen := map[string]bool{}
	for _, u := range attempts {
		if seen[u] {
			continue
		}
		seen[u] = true
		data, fin, err := f.B.Fetch(ctx, u)
		if err == nil && IsPDF(data) {
			return &Result{URL: fin, Data: data}, nil
		}
		if err == nil {
			err = fmt.Errorf("response was not a PDF")
		}
		errs = append(errs, shortURL(u)+": "+err.Error())
	}
	return nil, fmt.Errorf("%s", strings.Join(errs, "; "))
}

// proxiable reports whether a URL's host is a subscription publisher that a
// university proxy plausibly covers; preprint servers, DOI resolvers and
// unknown small hosts are not proxied (EZproxy would bounce to its login).
func proxiable(u string) bool {
	l := strings.ToLower(u)
	for _, h := range []string{"sciencedirect.com", "springer.com", "wiley.com", "tandfonline.com", "sagepub.com", "acm.org", "ieee.org", "emerald.com", "informs.org", "worldscientific.com", "oup.com", "jstor.org", "cambridge.org", "nature.com", "ebscohost.com", "proquest.com", "scopus.com", "webofscience.com", "misq.org", "aisnet.org", "igi-global.com", "degruyter.com", "elsevier.com", "liebertpub.com", "palgrave.com", "mitpressjournals.org", "annualreviews.org", "pubsonline"} {
		if strings.Contains(l, h) {
			return true
		}
	}
	return false
}

// SetBrowser installs or removes the headless-browser stage (kept last).
func (c *Chain) SetBrowser(b PDFBrowser, prefix func() string) {
	var kept []Fetcher
	for _, f := range c.Fetchers {
		if _, ok := f.(*Browser); !ok {
			kept = append(kept, f)
		}
	}
	if b != nil {
		kept = append(kept, &Browser{B: b, Prefix: prefix})
	}
	c.Fetchers = kept
}
