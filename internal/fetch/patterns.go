package fetch

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"literaturereview/internal/model"
)

// Publisher-specific direct PDF URL patterns. Most large publishers expose a
// stable PDF endpoint derivable from the DOI; landing pages often hide the
// link behind JavaScript, so trying these first raises the yield a lot, both
// on campus and through the university proxy.

var (
	piiRe      = regexp.MustCompile(`/pii/([A-Z0-9]{17})`)
	ieeeDocRe  = regexp.MustCompile(`ieeexplore\.ieee\.org/(?:abstract/)?document/(\d+)`)
	ssrnIDRe   = regexp.MustCompile(`^10\.2139/ssrn\.(\d+)$`)
	natureIDRe = regexp.MustCompile(`^10\.1038/(.+)$`)
	bepressRe  = regexp.MustCompile(`(?i)href=["']([^"']*viewcontent\.cgi[^"']*)["']`)
	dlPDFRe    = regexp.MustCompile(`(?i)href=["']([^"']*(?:/pdf|pdf/|download|fulltext)[^"']*)["']`)
)

// PatternURLs returns candidate direct-PDF URLs for a paper, derived from the
// DOI prefix (and, where needed, from the resolved landing URL).
func PatternURLs(ctx context.Context, client *http.Client, p *model.Paper, ua string) []string {
	doi := strings.ToLower(strings.TrimSpace(p.DOI))
	var out []string
	add := func(u string) { out = append(out, u) }
	prefix := doi
	if i := strings.Index(doi, "/"); i > 0 {
		prefix = doi[:i]
	}
	switch prefix {
	case "10.1007", "10.1057", "10.1186", "10.1140", "10.1065": // Springer, Palgrave, BMC
		add("https://link.springer.com/content/pdf/" + doi + ".pdf")
	case "10.1016", "10.1053", "10.1067": // Elsevier: need the PII from the linkinghub redirect
		if pii := resolvePII(ctx, client, doi, ua); pii != "" {
			add("https://www.sciencedirect.com/science/article/pii/" + pii + "/pdfft?isDTMRedir=true&download=true")
			add("https://www.sciencedirect.com/science/article/pii/" + pii + "/pdf")
		}
	case "10.1080": // Taylor & Francis
		add("https://www.tandfonline.com/doi/pdf/" + doi + "?download=true")
	case "10.1002", "10.1111", "10.1046": // Wiley
		add("https://onlinelibrary.wiley.com/doi/pdfdirect/" + doi + "?download=true")
		add("https://onlinelibrary.wiley.com/doi/pdf/" + doi)
	case "10.1142": // World Scientific
		add("https://www.worldscientific.com/doi/pdf/" + doi + "?download=true")
	case "10.1145": // ACM
		add("https://dl.acm.org/doi/pdf/" + doi)
	case "10.1177": // SAGE
		add("https://journals.sagepub.com/doi/pdf/" + doi + "?download=true")
	case "10.1108": // Emerald
		add("https://www.emerald.com/insight/content/doi/" + doi + "/full/pdf")
	case "10.1287": // INFORMS
		add("https://pubsonline.informs.org/doi/pdf/" + doi)
	case "10.1109": // IEEE: need the document number from the landing page
		if id := resolveIEEE(ctx, client, doi, ua); id != "" {
			add("https://ieeexplore.ieee.org/stampPDF/getPDF.jsp?tp=&arnumber=" + id)
			add("https://ieeexplore.ieee.org/stamp/stamp.jsp?tp=&arnumber=" + id)
		}
	case "10.2139": // SSRN
		if m := ssrnIDRe.FindStringSubmatch(doi); m != nil {
			add("https://papers.ssrn.com/sol3/Delivery.cfm?abstractid=" + m[1])
			add("https://papers.ssrn.com/sol3/Delivery.cfm/SSRN_ID" + m[1] + ".pdf?abstractid=" + m[1] + "&mirid=1")
		}
	case "10.1038": // Nature
		if m := natureIDRe.FindStringSubmatch(doi); m != nil {
			add("https://www.nature.com/articles/" + m[1] + ".pdf")
		}
	case "10.1371": // PLOS
		add("https://journals.plos.org/plosone/article/file?id=" + doi + "&type=printable")
	case "10.3389": // Frontiers
		add("https://www.frontiersin.org/articles/" + doi + "/pdf")
	case "10.1093": // OUP: pdf path is not derivable; landing-page parsing handles it
	case "10.24251": // HICSS (ScholarSpace): landing page has a direct link
	case "10.17705": // AIS eLibrary (JAIS/CAIS): landing page has viewcontent link
	}
	return out
}

// resolvePII follows the DOI to Elsevier's linkinghub and extracts the PII.
func resolvePII(ctx context.Context, client *http.Client, doi, ua string) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, "https://doi.org/"+doi, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("User-Agent", ua)
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	resp.Body.Close()
	if m := piiRe.FindStringSubmatch(resp.Request.URL.String()); m != nil {
		return m[1]
	}
	return ""
}

// resolveDOI follows the DOI redirect chain and returns the publisher URL
// ("" when the resolver cannot be reached).
func resolveDOI(ctx context.Context, client *http.Client, doi, ua string) string {
	if doi == "" {
		return ""
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, "https://doi.org/"+doi, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("User-Agent", ua)
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	resp.Body.Close()
	fin := resp.Request.URL.String()
	if strings.Contains(fin, "doi.org/") {
		return ""
	}
	return fin
}

// resolveIEEE follows the DOI to IEEE Xplore and extracts the document id.
func resolveIEEE(ctx context.Context, client *http.Client, doi, ua string) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, "https://doi.org/"+doi, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("User-Agent", ua)
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	resp.Body.Close()
	if m := ieeeDocRe.FindStringSubmatch(resp.Request.URL.String()); m != nil {
		return m[1]
	}
	return ""
}

// browserHeaders makes publisher requests look like an ordinary browser
// session; many publishers return 403 to non-browser user agents even for
// entitled or open content.
func browserHeaders() http.Header {
	h := http.Header{}
	h.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36")
	h.Set("Accept", "application/pdf,text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	h.Set("Accept-Language", "en-US,en;q=0.9,fi;q=0.8")
	h.Set("Upgrade-Insecure-Requests", "1")
	return h
}

// proxied rewrites a target URL through an EZproxy login prefix. Targets
// containing '&' use EZproxy's qurl= (URL-encoded) form, otherwise the plain
// url= form that every prefix is written for.
func proxied(prefix, target string) string {
	if strings.HasSuffix(prefix, "url=") {
		if strings.Contains(target, "&") {
			return strings.TrimSuffix(prefix, "url=") + "qurl=" + url.QueryEscape(target)
		}
		return prefix + target
	}
	sep := "?"
	if strings.Contains(prefix, "?") {
		sep = "&"
	}
	return prefix + sep + "qurl=" + url.QueryEscape(target)
}

// describe formats a candidate list for attempt logs.
func describe(urls []string) string {
	if len(urls) == 0 {
		return "no publisher pattern for this DOI"
	}
	return fmt.Sprintf("%d publisher pattern(s)", len(urls))
}
