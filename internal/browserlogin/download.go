package browserlogin

// Headless browser download: open a URL in Chromium with the user's logged-in
// profile, follow whatever JavaScript the publisher runs, click an obvious
// PDF/download link if the page itself is not a PDF, and capture the first
// PDF response via the Fetch domain.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"
)

var debugEvents = os.Getenv("BROWSERLOGIN_DEBUG") != ""

func dbg(format string, a ...any) {
	if debugEvents {
		log.Printf("[browser] "+format, a...)
	}
}

// DesktopUA is the user agent presented by the headless download stage.
const DesktopUA = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36"

// Downloader fetches PDFs through a headless browser. One browser process
// is used at a time (the profile directory can only be opened once).
type Downloader struct {
	Binary     string
	ProfileDir string
	Timeout    time.Duration
	// Cookies returns session cookies to inject before navigating (the
	// EZproxy session is a browser-session cookie and is not persisted in
	// the profile across restarts).
	Cookies func() []Cookie
	mu      sync.Mutex
}

// loginHosts marks pages that mean "not authenticated": failing fast beats
// waiting out the timeout.
func looksLikeLogin(u string) bool {
	l := strings.ToLower(u)
	return strings.Contains(l, "/login") || strings.Contains(l, "sso.") || strings.Contains(l, "shibboleth") || strings.Contains(l, "login-actions") || strings.Contains(l, "/idp/") || strings.Contains(l, "/saml")
}

// loginFormShown recognises URLs at which the SSO has rendered its login
// form (Keycloak "login-actions"): the session is definitely gone, so there
// is no point in waiting for an automatic redirect back.
func loginFormShown(u string) bool {
	l := strings.ToLower(u)
	return strings.Contains(l, "login-actions") || strings.Contains(l, "/password")
}

// loginGracePeriod is how long an intermediate SSO redirect may take to come
// back on its own before it is treated as "not logged in".
var loginGracePeriod = 6 * time.Second

// DownloadResult carries the captured PDF.
type DownloadResult struct {
	URL  string
	Data []byte
	Note string
}

// clickScript finds likely PDF/download links on a landing page and clicks
// them one at a time; returns how many were clicked.
const clickScript = `(() => {
  // Keep everything in this tab so the interceptor sees the response.
  window.open = (u) => { if (u) location.href = u; return window; };
  document.querySelectorAll('a[target]').forEach(a => a.target = '_self');
  const score = a => {
    const t = ((a.innerText||'') + ' ' + (a.getAttribute('aria-label')||'') + ' ' + (a.title||'') + ' ' + (a.href||'')).toLowerCase();
    let s = 0;
    if (/\bpdf\b/.test(t)) s += 3;
    if (/download/.test(t)) s += 2;
    if (/full[- ]?text|view article|get access|open/.test(t)) s += 1;
    if (/supplement|citation|cite|ris|bibtex|endnote|cover|preview/.test(t)) s -= 3;
    return s;
  };
  const els = Array.from(document.querySelectorAll('a,button')).map(a => [score(a), a]).filter(x => x[0] > 0).sort((a,b) => b[0]-a[0]).slice(0, 3);
  els.forEach(([s,a], i) => setTimeout(() => { try { a.click(); } catch(e) {} }, i * 1500));
  const m = document.querySelector('meta[name="citation_pdf_url"]');
  if (m && m.content) setTimeout(() => { try { window.location.href = m.content; } catch(e) {} }, 5000);
  return els.length + (m ? 1 : 0);
})()`

// Fetch loads url and returns the first PDF response seen, as
// (data, finalURL, err) so it satisfies fetch.PDFBrowser.
func (d *Downloader) Fetch(ctx context.Context, url string) ([]byte, string, error) {
	res, err := d.fetch(ctx, url)
	if err != nil {
		return nil, "", err
	}
	return res.Data, res.URL, nil
}

func (d *Downloader) fetch(ctx context.Context, url string) (*DownloadResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	timeout := d.Timeout
	if timeout == 0 {
		timeout = 35 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout+15*time.Second)
	defer cancel()
	// Headless Chromium advertises "HeadlessChrome" in its user agent, which
	// several publishers (e.g. Elsevier) reject outright; present the normal
	// desktop Chrome UA instead.
	c, err := Launch(ctx, d.Binary, d.ProfileDir, "--headless=new", "--disable-gpu", "--window-size=1280,900", "--hide-scrollbars", "--user-agent="+DesktopUA, "about:blank")
	if err != nil {
		return nil, err
	}
	defer c.Close()

	var tgt struct {
		TargetID string `json:"targetId"`
	}
	if err := c.Call(ctx, "", "Target.createTarget", map[string]any{"url": "about:blank"}, &tgt); err != nil {
		return nil, fmt.Errorf("createTarget: %w", err)
	}
	var att struct {
		SessionID string `json:"sessionId"`
	}
	if err := c.Call(ctx, "", "Target.attachToTarget", map[string]any{"targetId": tgt.TargetID, "flatten": true}, &att); err != nil {
		return nil, fmt.Errorf("attach: %w", err)
	}
	sid := att.SessionID
	if d.Cookies != nil {
		var cks []map[string]any
		for _, ck := range d.Cookies() {
			dom := ck.Domain
			if dom == "" {
				continue
			}
			cks = append(cks, map[string]any{"name": ck.Name, "value": ck.Value, "domain": dom, "path": "/", "secure": true})
		}
		if len(cks) > 0 {
			if err := c.Call(ctx, "", "Storage.setCookies", map[string]any{"cookies": cks}, nil); err != nil {
				return nil, fmt.Errorf("set cookies: %w", err)
			}
		}
	}
	_ = c.Call(ctx, sid, "Page.enable", map[string]any{}, nil)
	_ = c.Call(ctx, sid, "Runtime.enable", map[string]any{}, nil)
	_ = c.Call(ctx, sid, "Network.setUserAgentOverride", map[string]any{"userAgent": DesktopUA, "acceptLanguage": "en-US,en;q=0.9,fi;q=0.8", "platform": "Linux x86_64"}, nil)
	// navigator.webdriver is another headless tell some publishers check.
	_ = c.Call(ctx, sid, "Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": "Object.defineProperty(navigator, 'webdriver', {get: () => undefined});"}, nil)
	// Intercept responses so a PDF can be captured even when the browser
	// would otherwise hand it to its viewer or a download.
	// Only navigations and XHR/fetch responses can carry the PDF; leaving
	// images, scripts and styles alone keeps the event volume small.
	fetchPatterns := map[string]any{"patterns": []map[string]any{
		{"urlPattern": "*", "resourceType": "Document", "requestStage": "Response"},
		{"urlPattern": "*", "resourceType": "XHR", "requestStage": "Response"},
		{"urlPattern": "*", "resourceType": "Fetch", "requestStage": "Response"},
		{"urlPattern": "*", "resourceType": "Other", "requestStage": "Response"},
	}}
	if err := c.Call(ctx, sid, "Fetch.enable", fetchPatterns, nil); err != nil {
		return nil, fmt.Errorf("fetch.enable: %w", err)
	}
	dbg("fetch enabled on %s; navigating to %s", sid, url)
	// Pop-ups (publishers often open PDFs in a new tab): attach to them and
	// intercept there as well.
	_ = c.Call(ctx, sid, "Target.setAutoAttach", map[string]any{"autoAttach": true, "waitForDebuggerOnStart": false, "flatten": true}, nil)
	// Navigate asynchronously: with response interception enabled the main
	// document request is paused until we continue it from the event loop
	// below, so waiting for Page.navigate here would deadlock.
	navErr := make(chan error, 1)
	go func() { navErr <- c.Call(ctx, sid, "Page.navigate", map[string]any{"url": url}, nil) }()

	deadline := time.After(timeout)
	clickTick := time.NewTicker(6 * time.Second) // retry clicking while challenges/redirects settle
	defer clickTick.Stop()
	clicks := 0
	lastURL := url
	// A login URL is not final: an SSO that still remembers the profile
	// redirects back within a few seconds, so give it a short grace period.
	var loginGrace <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-c.Closed():
			return nil, errors.New("browser exited")
		case err := <-navErr:
			if err != nil {
				return nil, fmt.Errorf("navigate: %w", err)
			}
		case <-deadline:
			return nil, fmt.Errorf("no PDF response within %s (last page: %s)", timeout, shorten(lastURL))
		case <-loginGrace:
			if looksLikeLogin(lastURL) && !strings.HasPrefix(lastURL, url) {
				return nil, fmt.Errorf("redirected to a login page (%s): no valid session for this host", shorten(lastURL))
			}
			loginGrace = nil
		case <-clickTick.C:
			if clicks < 4 {
				clicks++
				var res struct {
					Result struct {
						Value int `json:"value"`
					} `json:"result"`
				}
				_ = c.Call(ctx, sid, "Runtime.evaluate", map[string]any{"expression": clickScript, "returnByValue": true}, &res)
			}
		case ev := <-c.Events():
			if debugEvents && ev.Method != "Network.dataReceived" {
				dbg("event %s session=%s %s", ev.Method, ev.SessionID, shorten(string(ev.Params)))
			}
			switch ev.Method {
			case "Target.attachedToTarget":
				// A pop-up or child target: enable interception there too.
				// Fire-and-forget: a blocking call inside the event loop would
				// stall event processing if the reply were delayed.
				var p struct {
					SessionID  string `json:"sessionId"`
					TargetInfo struct {
						Type string `json:"type"`
					} `json:"targetInfo"`
				}
				if json.Unmarshal(ev.Params, &p) == nil && p.SessionID != "" && p.SessionID != sid && p.TargetInfo.Type == "page" {
					_ = c.Send(p.SessionID, "Fetch.enable", fetchPatterns)
					_ = c.Send(p.SessionID, "Network.setUserAgentOverride", map[string]any{"userAgent": DesktopUA})
				}
			case "Page.frameNavigated":
				var p struct {
					Frame struct {
						URL      string `json:"url"`
						ParentID string `json:"parentId"`
					} `json:"frame"`
				}
				if json.Unmarshal(ev.Params, &p) == nil && p.Frame.ParentID == "" && p.Frame.URL != "" {
					lastURL = p.Frame.URL
					if looksLikeLogin(lastURL) && !strings.HasPrefix(lastURL, url) {
						if loginFormShown(lastURL) {
							return nil, fmt.Errorf("redirected to a login page (%s): no valid session for this host", shorten(lastURL))
						}
						if loginGrace == nil {
							loginGrace = time.After(loginGracePeriod)
						}
					}
				}
			case "Fetch.requestPaused":
				var p struct {
					RequestID       string `json:"requestId"`
					ResponseStatus  int    `json:"responseStatusCode"`
					ResponseHeaders []struct {
						Name  string `json:"name"`
						Value string `json:"value"`
					} `json:"responseHeaders"`
					Request struct {
						URL string `json:"url"`
					} `json:"request"`
				}
				if json.Unmarshal(ev.Params, &p) != nil || p.RequestID == "" {
					continue
				}
				ct := ""
				for _, h := range p.ResponseHeaders {
					if strings.EqualFold(h.Name, "content-type") {
						ct = strings.ToLower(h.Value)
					}
				}
				isPDF := strings.Contains(ct, "application/pdf") || strings.Contains(ct, "application/x-pdf") || (ct == "application/octet-stream" && strings.Contains(strings.ToLower(p.Request.URL), "pdf"))
				if !isPDF || p.ResponseStatus >= 400 {
					_ = c.Send(ev.SessionID, "Fetch.continueRequest", map[string]any{"requestId": p.RequestID})
					continue
				}
				var body struct {
					Body          string `json:"body"`
					Base64Encoded bool   `json:"base64Encoded"`
				}
				if err := c.Call(ctx, ev.SessionID, "Fetch.getResponseBody", map[string]any{"requestId": p.RequestID}, &body); err != nil {
					_ = c.Send(ev.SessionID, "Fetch.continueRequest", map[string]any{"requestId": p.RequestID})
					continue
				}
				var data []byte
				if body.Base64Encoded {
					data, _ = base64.StdEncoding.DecodeString(body.Body)
				} else {
					data = []byte(body.Body)
				}
				_ = c.Send(ev.SessionID, "Fetch.fulfillRequest", map[string]any{"requestId": p.RequestID, "responseCode": 200, "body": base64.StdEncoding.EncodeToString([]byte("done"))})
				if len(data) > 1024 && strings.Contains(string(data[:min(1024, len(data))]), "%PDF-") {
					return &DownloadResult{URL: p.Request.URL, Data: data, Note: "captured in headless browser"}, nil
				}
			}
		}
	}
}

func shorten(s string) string {
	if len(s) > 100 {
		return s[:100] + "…"
	}
	return s
}
