package browserlogin

// Silent session refresh: the EZproxy cookie is short-lived, but the
// university SSO behind it often remembers the browser profile for longer.
// Opening the proxied link headlessly then yields a fresh cookie without the
// user doing anything; if the SSO shows its login form instead, we give up
// quickly and the app asks for an interactive login.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// RefreshSession opens loginURL (the proxied link) in headless Chromium with
// the saved profile and returns the proxy cookies when the page lands on the
// proxied publisher host. It fails fast when an SSO login form appears.
func (d *Downloader) RefreshSession(ctx context.Context, loginURL, proxyHost string) (string, []Cookie, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	match := DefaultMatch(proxyHost)
	ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	c, err := Launch(ctx, d.Binary, d.ProfileDir, "--headless=new", "--disable-gpu", "--window-size=1280,900", "--user-agent="+DesktopUA, "about:blank")
	if err != nil {
		return "", nil, err
	}
	defer c.Close()
	var tgt struct {
		TargetID string `json:"targetId"`
	}
	if err := c.Call(ctx, "", "Target.createTarget", map[string]any{"url": "about:blank"}, &tgt); err != nil {
		return "", nil, fmt.Errorf("createTarget: %w", err)
	}
	var att struct {
		SessionID string `json:"sessionId"`
	}
	if err := c.Call(ctx, "", "Target.attachToTarget", map[string]any{"targetId": tgt.TargetID, "flatten": true}, &att); err != nil {
		return "", nil, fmt.Errorf("attach: %w", err)
	}
	sid := att.SessionID
	_ = c.Call(ctx, sid, "Page.enable", map[string]any{}, nil)
	_ = c.Call(ctx, sid, "Runtime.enable", map[string]any{}, nil)
	_ = c.Call(ctx, sid, "Network.setUserAgentOverride", map[string]any{"userAgent": DesktopUA, "acceptLanguage": "en-US,en;q=0.9,fi;q=0.8", "platform": "Linux x86_64"}, nil)
	if err := c.Call(ctx, sid, "Page.navigate", map[string]any{"url": loginURL}, nil); err != nil {
		return "", nil, fmt.Errorf("navigate: %w", err)
	}
	cookies := func() (string, []Cookie, bool) {
		var out struct {
			Cookies []Cookie `json:"cookies"`
		}
		if err := c.Call(ctx, "", "Storage.getCookies", map[string]any{}, &out); err != nil {
			return "", nil, false
		}
		var parts []string
		var matched []Cookie
		for _, ck := range out.Cookies {
			if match(ck) {
				parts = append(parts, ck.Name+"="+ck.Value)
				matched = append(matched, ck)
			}
		}
		return strings.Join(parts, "; "), matched, len(parts) > 0
	}
	deadline := time.After(30 * time.Second)
	var loginGrace <-chan time.Time // started when an SSO page appears: it may redirect back by itself
	lastURL := loginURL
	for {
		select {
		case <-ctx.Done():
			return "", nil, ctx.Err()
		case <-c.Closed():
			return "", nil, errors.New("browser exited")
		case <-deadline:
			return "", nil, fmt.Errorf("no proxied page within 30s (last page: %s)", shorten(lastURL))
		case <-loginGrace:
			if looksLikeLogin(lastURL) && !landedOnProxy(lastURL, proxyHost) {
				return "", nil, fmt.Errorf("the university SSO asks for an interactive login (%s)", shorten(lastURL))
			}
			loginGrace = nil
		case ev := <-c.Events():
			if ev.Method != "Page.frameNavigated" {
				continue
			}
			var p struct {
				Frame struct {
					URL      string `json:"url"`
					ParentID string `json:"parentId"`
				} `json:"frame"`
			}
			if json.Unmarshal(ev.Params, &p) != nil || p.Frame.ParentID != "" || p.Frame.URL == "" {
				continue
			}
			lastURL = p.Frame.URL
			switch {
			case landedOnProxy(lastURL, proxyHost):
				// The publisher page is served through the proxy: the session is live.
				for i := 0; i < 5; i++ {
					if hdr, cks, ok := cookies(); ok {
						return hdr, cks, nil
					}
					time.Sleep(500 * time.Millisecond)
				}
				return "", nil, errors.New("reached the proxied page but no proxy cookie was set")
			case loginFormShown(lastURL):
				return "", nil, fmt.Errorf("the university SSO asks for an interactive login (%s)", shorten(lastURL))
			case looksLikeLogin(lastURL) && loginGrace == nil:
				loginGrace = time.After(8 * time.Second)
			}
		}
	}
}

// landedOnProxy reports whether u is a publisher page rewritten under the
// proxy host (e.g. www-sciencedirect-com.ezproxy.utu.fi) rather than the
// proxy's own login path.
func landedOnProxy(u, proxyHost string) bool {
	pu, err := url.Parse(u)
	if err != nil {
		return false
	}
	h := strings.ToLower(pu.Hostname())
	ph := strings.ToLower(proxyHost)
	if !(h == ph || strings.HasSuffix(h, "."+ph)) {
		return false
	}
	return !strings.Contains(strings.ToLower(pu.Path), "/login")
}
