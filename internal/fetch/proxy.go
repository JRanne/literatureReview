package fetch

import (
	"context"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"

	"literaturereview/internal/model"
)

// Proxy routes the publisher request through an EZproxy-style URL prefix
// using the user's authenticated session cookie, i.e. genuine university
// access via the library's subscription.
type Proxy struct {
	prefix string
	host   string
	client *http.Client
	ua     string
	hasCk  bool
	// session is the chain-wide expiry flag (set by Chain.SetProxy).
	session *SessionState
}

// NewProxy builds the proxy fetcher. prefix looks like
// "https://ezproxy.example.fi/login?url=" and cookie like "ezproxy=abc123".
func NewProxy(prefix, cookie, ua string) (*Proxy, error) {
	pu, err := url.Parse(prefix)
	if err != nil || pu.Host == "" {
		return nil, fmt.Errorf("invalid PROXY_PREFIX %q", prefix)
	}
	jar, _ := cookiejar.New(nil)
	// Apply the session cookie to the proxy host and all of its subdomains,
	// because EZproxy rewrites publisher hosts under its own domain.
	domain := pu.Hostname()
	if parts := strings.Split(domain, "."); len(parts) > 2 {
		domain = strings.Join(parts[1:], ".")
	}
	var cookies []*http.Cookie
	for _, kv := range strings.Split(cookie, ";") {
		kv = strings.TrimSpace(kv)
		if kv == "" {
			continue
		}
		name, val, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		cookies = append(cookies, &http.Cookie{Name: strings.TrimSpace(name), Value: strings.TrimSpace(val), Domain: domain, Path: "/", Secure: pu.Scheme == "https"})
	}
	jar.SetCookies(&url.URL{Scheme: pu.Scheme, Host: pu.Host, Path: "/"}, cookies)
	return &Proxy{prefix: prefix, host: pu.Host, client: newClient(jar), ua: ua, hasCk: len(cookies) > 0}, nil
}

func (f *Proxy) Name() string { return "university proxy" }

func (f *Proxy) Fetch(ctx context.Context, p *model.Paper) (*Result, error) {
	target := landingURL(p)
	if target == "" {
		return nil, fmt.Errorf("%w: no DOI or URL", ErrNotApplicable)
	}
	if !f.hasCk {
		return nil, fmt.Errorf("%w: proxy session cookie not set (Library access page)", ErrNotApplicable)
	}
	if f.session.Expired() {
		return nil, fmt.Errorf("%w: %s", ErrSessionExpired, skipMsg)
	}
	hdr := browserHeaders()
	var errs []string
	// 1. publisher patterns through the proxy
	pats := PatternURLs(ctx, f.client, p, hdr.Get("User-Agent"))
	for _, u := range pats {
		r, err := fetchPDF(ctx, f.client, proxied(f.prefix, u), f.ua, hdr)
		if err == nil {
			return r, nil
		}
		errs = append(errs, err.Error())
	}
	// 2. landing page through the proxy
	r, err := fetchPDF(ctx, f.client, proxied(f.prefix, target), f.ua, hdr)
	if err == nil {
		return r, nil
	}
	errs = append(errs, err.Error())
	msg := strings.Join(errs, "; ")
	if isLoginRedirect(msg) {
		f.session.MarkExpired(msg)
		return nil, fmt.Errorf("%w: proxy redirected to its login page (%s)", ErrSessionExpired, msg)
	}
	return nil, fmt.Errorf("%s via proxy; %s", describe(pats), msg)
}

// Test checks whether the proxy session is valid by requesting a subscribed
// publisher page through it and seeing whether we land on a login page.
func (f *Proxy) Test(ctx context.Context) (string, error) {
	if !f.hasCk {
		return "", fmt.Errorf("no session cookie set")
	}
	target := "https://www.sciencedirect.com/science/article/pii/S0167923619300715"
	pg, err := get(ctx, f.client, proxied(f.prefix, target), f.ua, browserHeaders())
	if err != nil {
		return "", err
	}
	fin := pg.finalURL
	switch {
	case isLoginRedirect(fin):
		f.session.MarkExpired("access test landed on " + shortURL(fin))
		return fin, fmt.Errorf("%w: the proxy sent us to its login page; the cookie is not accepted or has expired", ErrSessionExpired)
	case pg.status >= 400:
		return fin, fmt.Errorf("HTTP %d from %s", pg.status, shortURL(fin))
	}
	f.session.Reset()
	return fin, nil
}

// Session exposes the shared expiry state (nil-safe).
func (f *Proxy) Session() *SessionState { return f.session }

// isLoginRedirect recognises EZproxy and SSO login URLs in an error message
// or final URL.
func isLoginRedirect(s string) bool {
	l := strings.ToLower(s)
	return strings.Contains(l, "/login") || strings.Contains(l, "login.") || strings.Contains(l, "sso.") || strings.Contains(l, "/saml") || strings.Contains(l, "/idp/")
}

// HasCookie reports whether a session cookie is configured.
func (f *Proxy) HasCookie() bool { return f.hasCk }

// Prefix returns the configured login prefix.
func (f *Proxy) Prefix() string { return f.prefix }
