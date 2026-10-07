// Package fetch obtains full-text PDFs through an ordered chain of access
// methods: open-access sources first, then the publisher directly, then the
// configured university proxy.
package fetch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"literaturereview/internal/config"
	"literaturereview/internal/model"
)

// Result is a successfully retrieved PDF.
type Result struct {
	URL  string
	Data []byte
}

// Fetcher is one access method.
type Fetcher interface {
	Name() string
	// Fetch returns a PDF, or an error explaining why this method did not work.
	// Returning ErrNotApplicable means the paper lacks the identifiers needed.
	Fetch(ctx context.Context, p *model.Paper) (*Result, error)
}

// ErrNotApplicable signals a fetcher cannot be used for this paper.
var ErrNotApplicable = errors.New("not applicable")

// Chain runs fetchers in order.
type Chain struct {
	Fetchers []Fetcher
}

// NewChain builds the default chain from configuration.
func NewChain(cfg config.Config) *Chain {
	client := newClient(nil)
	ch := &Chain{}
	ch.Fetchers = append(ch.Fetchers,
		&MetadataOA{client: client, ua: cfg.UserAgent},
		&EuropePMC{client: client, ua: cfg.UserAgent},
		&Unpaywall{client: client, ua: cfg.UserAgent, email: cfg.ContactEmail},
		&Publisher{client: client, ua: cfg.UserAgent},
	)
	if cfg.ProxyConfigured() {
		if px, err := NewProxy(cfg.ProxyPrefix, cfg.ProxyCookie, cfg.UserAgent); err == nil {
			ch.Fetchers = append(ch.Fetchers, px)
		}
	}
	return ch
}

// SetProxy replaces (or removes, when nil) the university-proxy stage,
// keeping the headless-browser stage last.
func (c *Chain) SetProxy(px *Proxy) {
	var kept []Fetcher
	var browser Fetcher
	for _, f := range c.Fetchers {
		switch f.(type) {
		case *Proxy:
		case *Browser:
			browser = f
		default:
			kept = append(kept, f)
		}
	}
	if px != nil {
		kept = append(kept, px)
	}
	if browser != nil {
		kept = append(kept, browser)
	}
	c.Fetchers = kept
}

// Proxy returns the configured proxy stage, if any.
func (c *Chain) Proxy() *Proxy {
	for _, f := range c.Fetchers {
		if px, ok := f.(*Proxy); ok {
			return px
		}
	}
	return nil
}

// Names lists the methods in order (for display).
func (c *Chain) Names() []string {
	var out []string
	for _, f := range c.Fetchers {
		out = append(out, f.Name())
	}
	return out
}

// Run tries every fetcher until one yields a PDF, writing it to dir.
// The paper's Download field is updated in place and returned.
func (c *Chain) Run(ctx context.Context, p *model.Paper, dir string) model.DownloadResult {
	res := model.DownloadResult{Status: model.DownloadRunning}
	for _, f := range c.Fetchers {
		if ctx.Err() != nil {
			res.Attempts = append(res.Attempts, model.Attempt{Method: f.Name(), Message: "cancelled", At: time.Now()})
			break
		}
		budget := 90 * time.Second
		if t, ok := f.(interface{ Timeout() time.Duration }); ok {
			budget = t.Timeout()
		}
		fctx, cancel := context.WithTimeout(ctx, budget)
		r, err := f.Fetch(fctx, p)
		cancel()
		at := model.Attempt{Method: f.Name(), At: time.Now()}
		if err != nil {
			if errors.Is(err, ErrNotApplicable) {
				at.Message = "skipped: " + err.Error()
			} else {
				at.Message = err.Error()
			}
			res.Attempts = append(res.Attempts, at)
			continue
		}
		path, werr := WritePDF(dir, p, r.Data)
		at.URL = r.URL
		if werr != nil {
			at.Message = "save failed: " + werr.Error()
			res.Attempts = append(res.Attempts, at)
			continue
		}
		at.OK = true
		at.Message = fmt.Sprintf("saved %d KB", len(r.Data)/1024)
		res.Attempts = append(res.Attempts, at)
		res.Status = model.DownloadOK
		res.Method = f.Name()
		res.Path = path
		res.Bytes = int64(len(r.Data))
		return res
	}
	res.Status = model.DownloadFailed
	return res
}

// WritePDF stores PDF bytes under dir with a slugged, collision-free name.
func WritePDF(dir string, p *model.Paper, data []byte) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	name := p.Slug()
	if len(name) > 120 {
		name = name[:120]
	}
	path := filepath.Join(dir, name+".pdf")
	for i := 2; ; i++ {
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			break
		}
		path = filepath.Join(dir, fmt.Sprintf("%s-%d.pdf", name, i))
	}
	return path, os.WriteFile(path, data, 0o644)
}

// --- HTTP plumbing ----------------------------------------------------------

func newClient(jar http.CookieJar) *http.Client {
	return &http.Client{
		Timeout: 90 * time.Second,
		Jar:     jar,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 15 {
				return errors.New("too many redirects")
			}
			return nil
		},
	}
}

// IsPDF checks the magic bytes.
func IsPDF(b []byte) bool {
	head := b
	if len(head) > 1024 {
		head = head[:1024]
	}
	return bytes.Contains(head, []byte("%PDF-"))
}

const maxPDF = 100 << 20

// page is a fetched HTTP response, either a PDF or an HTML landing page.
type page struct {
	finalURL    string
	contentType string
	body        []byte
	status      int
}

func get(ctx context.Context, client *http.Client, rawURL, ua string, extra http.Header) (*page, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "application/pdf,text/html;q=0.9,*/*;q=0.8")
	for k, vs := range extra {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPDF))
	if err != nil {
		return nil, err
	}
	return &page{
		finalURL:    resp.Request.URL.String(),
		contentType: resp.Header.Get("Content-Type"),
		body:        body,
		status:      resp.StatusCode,
	}, nil
}

var (
	metaPDFRe  = regexp.MustCompile(`(?is)<meta[^>]+name=["']citation_pdf_url["'][^>]+content=["']([^"']+)["']`)
	metaPDFRe2 = regexp.MustCompile(`(?is)<meta[^>]+content=["']([^"']+)["'][^>]+name=["']citation_pdf_url["']`)
	hrefPDFRe  = regexp.MustCompile(`(?is)href=["']([^"']+\.pdf(?:\?[^"']*)?)["']`)
	linkPDFRe  = regexp.MustCompile(`(?is)<link[^>]+type=["']application/pdf["'][^>]+href=["']([^"']+)["']`)
	hrefPDFRe2 = regexp.MustCompile(`(?is)href=["']([^"']*/pdf/[^"']*)["']`)
)

// PDFLinksFromHTML extracts candidate PDF URLs from a landing page, most
// trustworthy first, resolved against base.
func PDFLinksFromHTML(html []byte, base string) []string {
	bu, err := url.Parse(base)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	add := func(raw string) {
		raw = strings.TrimSpace(strings.ReplaceAll(raw, "&amp;", "&"))
		if raw == "" {
			return
		}
		u, err := url.Parse(raw)
		if err != nil {
			return
		}
		abs := bu.ResolveReference(u).String()
		if !seen[abs] {
			seen[abs] = true
			out = append(out, abs)
		}
	}
	for _, re := range []*regexp.Regexp{metaPDFRe, metaPDFRe2, linkPDFRe} {
		for _, m := range re.FindAllSubmatch(html, -1) {
			add(string(m[1]))
		}
	}
	for _, re := range []*regexp.Regexp{hrefPDFRe, hrefPDFRe2, bepressRe, dlPDFRe} {
		for _, m := range re.FindAllSubmatch(html, 6) {
			add(string(m[1]))
		}
	}
	return out
}

// fetchPDF resolves a URL to a PDF, following one level of landing-page
// indirection via citation_pdf_url / PDF links.
func fetchPDF(ctx context.Context, client *http.Client, rawURL, ua string, extra http.Header) (*Result, error) {
	pg, err := get(ctx, client, rawURL, ua, extra)
	if err != nil {
		return nil, err
	}
	if pg.status >= 400 {
		return nil, fmt.Errorf("HTTP %d at %s", pg.status, shortURL(pg.finalURL))
	}
	if IsPDF(pg.body) {
		return &Result{URL: pg.finalURL, Data: pg.body}, nil
	}
	if !strings.Contains(strings.ToLower(pg.contentType), "html") {
		return nil, fmt.Errorf("not a PDF (%s) at %s", pg.contentType, shortURL(pg.finalURL))
	}
	links := PDFLinksFromHTML(pg.body, pg.finalURL)
	if len(links) == 0 {
		return nil, fmt.Errorf("landing page has no PDF link (%s)", shortURL(pg.finalURL))
	}
	var lastErr error
	for i, l := range links {
		if i >= 5 {
			break
		}
		sub, err := get(ctx, client, l, ua, extra)
		if err != nil {
			lastErr = err
			continue
		}
		if sub.status < 400 && IsPDF(sub.body) {
			return &Result{URL: sub.finalURL, Data: sub.body}, nil
		}
		lastErr = fmt.Errorf("HTTP %d, %s at %s", sub.status, firstWord(sub.contentType), shortURL(sub.finalURL))
	}
	return nil, fmt.Errorf("PDF link(s) did not yield a PDF: %v", lastErr)
}

func shortURL(s string) string {
	if u, err := url.Parse(s); err == nil {
		p := u.Host + u.Path
		if len(p) > 80 {
			p = p[:80] + "…"
		}
		return p
	}
	return s
}

func firstWord(s string) string {
	if i := strings.IndexAny(s, "; "); i > 0 {
		return s[:i]
	}
	return s
}
