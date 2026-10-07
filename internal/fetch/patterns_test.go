package fetch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"literaturereview/internal/model"
)

func TestPatternURLs(t *testing.T) {
	cases := map[string]string{
		"10.1007/978-3-030-12345-6_7":   "https://link.springer.com/content/pdf/10.1007/978-3-030-12345-6_7.pdf",
		"10.1080/17517575.2024.2310846": "https://www.tandfonline.com/doi/pdf/10.1080/17517575.2024.2310846?download=true",
		"10.1002/iis2.70183":            "https://onlinelibrary.wiley.com/doi/pdfdirect/10.1002/iis2.70183?download=true",
		"10.1142/9789819816347_0009":    "https://www.worldscientific.com/doi/pdf/10.1142/9789819816347_0009?download=true",
		"10.2139/ssrn.7217180":          "https://papers.ssrn.com/sol3/Delivery.cfm?abstractid=7217180",
		"10.1145/3770855.3818090":       "https://dl.acm.org/doi/pdf/10.1145/3770855.3818090",
		"10.1177/0001839219887161":      "https://journals.sagepub.com/doi/pdf/10.1177/0001839219887161?download=true",
	}
	for doi, want := range cases {
		got := PatternURLs(context.Background(), http.DefaultClient, &model.Paper{DOI: doi}, "ua")
		if len(got) == 0 || got[0] != want {
			t.Errorf("%s → %v, want %s", doi, got, want)
		}
	}
	if got := PatternURLs(context.Background(), http.DefaultClient, &model.Paper{DOI: "10.9999/unknown"}, "ua"); len(got) != 0 {
		t.Errorf("unknown prefix should give no patterns: %v", got)
	}
}

func TestElsevierPIIResolution(t *testing.T) {
	// A fake doi.org that redirects to a linkinghub-style URL with a PII.
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer hub.Close()
	doiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, hub.URL+"/retrieve/pii/S0007681321000100", http.StatusFound)
	}))
	defer doiSrv.Close()
	client := &http.Client{Transport: rewriteHost{to: doiSrv.URL}}
	got := PatternURLs(context.Background(), client, &model.Paper{DOI: "10.1016/j.bushor.2021.02.008"}, "ua")
	if len(got) != 2 || !strings.Contains(got[0], "pii/S0007681321000100/pdfft") {
		t.Fatalf("got %v", got)
	}
}

// rewriteHost sends every request to a test server regardless of host.
type rewriteHost struct{ to string }

func (r rewriteHost) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.Contains(req.URL.Host, "doi.org") {
		u := strings.TrimPrefix(r.to, "http://")
		req.URL.Scheme, req.URL.Host = "http", u
	}
	return http.DefaultTransport.RoundTrip(req)
}

func TestProxied(t *testing.T) {
	if got := proxied("https://ezproxy.x.fi/login?url=", "https://a.b/c?d=1"); got != "https://ezproxy.x.fi/login?url=https://a.b/c?d=1" {
		t.Fatal(got)
	}
	// '&' in the target → qurl= with encoding, so EZproxy does not split it.
	if got := proxied("https://ezproxy.x.fi/login?url=", "https://a.b/c?d=1&e=2"); got != "https://ezproxy.x.fi/login?qurl=https%3A%2F%2Fa.b%2Fc%3Fd%3D1%26e%3D2" {
		t.Fatal(got)
	}
	if got := proxied("https://ezproxy.x.fi/login", "https://a.b/c?d=1"); got != "https://ezproxy.x.fi/login?qurl=https%3A%2F%2Fa.b%2Fc%3Fd%3D1" {
		t.Fatal(got)
	}
}

func TestLandingPageLinksBepress(t *testing.T) {
	html := []byte(`<a href="https://ink.library.smu.edu.sg/cgi/viewcontent.cgi?article=1868&amp;context=etd_coll">Download</a>`)
	got := PDFLinksFromHTML(html, "https://ink.library.smu.edu.sg/etd_coll/869")
	if len(got) != 1 || !strings.Contains(got[0], "viewcontent.cgi?article=1868&context=etd_coll") {
		t.Fatalf("got %v", got)
	}
}

type fakeBrowser struct{ pdfFor string }

func (f fakeBrowser) Fetch(_ context.Context, u string) ([]byte, string, error) {
	if strings.Contains(u, f.pdfFor) {
		return []byte("%PDF-1.7 fake"), u, nil
	}
	return []byte("<html>no</html>"), u, nil
}

func TestBrowserStage(t *testing.T) {
	ch := &Chain{}
	ch.SetBrowser(fakeBrowser{pdfFor: "tandfonline.com/doi/pdf"}, func() string { return "https://ezproxy.x.fi/login?url=" })
	px, _ := NewProxy("https://ezproxy.x.fi/login?url=", "ezproxy=1", "ua")
	ch.SetProxy(px)
	if n := ch.Names(); len(n) != 2 || n[0] != "university proxy" || n[1] != "headless browser" {
		t.Fatalf("order %v", n)
	}
	b := ch.Fetchers[1].(*Browser)
	r, err := b.Fetch(context.Background(), &model.Paper{DOI: "10.1080/17517575.2024.2310846"})
	if err != nil || !strings.HasPrefix(string(r.Data), "%PDF") || !strings.Contains(r.URL, "ezproxy.x.fi/login?url=https://www.tandfonline.com") {
		t.Fatalf("browser stage: %v %+v", err, r)
	}
	// SSRN is never proxied; its landing page is tried directly.
	b2 := &Browser{B: fakeBrowser{pdfFor: "doi.org/10.2139"}, Prefix: func() string { return "https://ezproxy.x.fi/login?url=" }}
	r, err = b2.Fetch(context.Background(), &model.Paper{DOI: "10.2139/ssrn.7217180"})
	if err != nil || strings.Contains(r.URL, "ezproxy") {
		t.Fatalf("ssrn: %v %+v", err, r)
	}
	// Unknown small host: landing page is tried directly, never via proxy.
	b3 := &Browser{B: fakeBrowser{pdfFor: "doi.org/10.65769"}, Prefix: func() string { return "https://ezproxy.x.fi/login?url=" }}
	r, err = b3.Fetch(context.Background(), &model.Paper{DOI: "10.65769/76jyvbjkyj"})
	if err != nil || strings.Contains(r.URL, "ezproxy") {
		t.Fatalf("unknown host: %v %+v", err, r)
	}
	if !proxiable("https://www.sciencedirect.com/x") || proxiable("https://doi.org/10.1/x") || proxiable("https://papers.ssrn.com/x") {
		t.Fatal("proxiable")
	}
	ch.SetBrowser(nil, nil)
	if len(ch.Names()) != 1 {
		t.Fatal("browser stage not removed")
	}
}
