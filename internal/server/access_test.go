package server

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"literaturereview/internal/config"
	"literaturereview/internal/model"
	"literaturereview/internal/store"
)

func TestNormalizeCookie(t *testing.T) {
	if got := normalizeCookie("Cookie: ezproxy=abc; ezproxyl=1"); got != "ezproxy=abc; ezproxyl=1" {
		t.Fatal(got)
	}
	txt := "# Netscape HTTP Cookie File\n.ezproxy.x.fi\tTRUE\t/\tTRUE\t0\tezproxy\tXYZ\n.ezproxy.x.fi\tTRUE\t/\tTRUE\t0\tezproxyl\t1\n"
	if got := normalizeCookie(txt); got != "ezproxy=XYZ; ezproxyl=1" {
		t.Fatal(got)
	}
}

func TestAccessSettingsAndUpload(t *testing.T) {
	s, ts := newTestServer(t)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	// Page renders; saving a prefix + cookie enables the proxy stage at runtime.
	resp, _ := client.Get(ts.URL + "/access")
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(b), "EZproxy login prefix") {
		t.Fatalf("access page: %d", resp.StatusCode)
	}
	resp, _ = client.PostForm(ts.URL+"/access", url.Values{"proxy_prefix": {"https://ezproxy.uni.fi/login?url="}, "proxy_cookie": {"ezproxy=secret"}, "test": {"0"}})
	resp.Body.Close()
	if px := s.chain.Proxy(); px == nil || !px.HasCookie() || px.Prefix() != "https://ezproxy.uni.fi/login?url=" {
		t.Fatalf("proxy not applied: %+v", s.chain.Names())
	}
	// Secret is persisted but never echoed back.
	resp, _ = client.Get(ts.URL + "/access")
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(b), "secret") || !strings.Contains(string(b), ">set") {
		t.Fatal("cookie echoed or not marked set")
	}
	// Forget cookie removes it.
	resp, _ = client.PostForm(ts.URL+"/access", url.Values{"proxy_prefix": {"https://ezproxy.uni.fi/login?url="}, "clear_cookie": {"1"}, "test": {"0"}})
	resp.Body.Close()
	if s.chain.Proxy().HasCookie() {
		t.Fatal("cookie not cleared")
	}

	// Manual upload for a failed paper.
	p := &model.Project{ID: "up1", Title: "T", Question: "Q", Stage: model.StageDone, CreatedAt: time.Now(), Papers: []*model.Paper{
		{ID: "a", Title: "Paywalled paper", Authors: []string{"Ann Author"}, Year: 2024, Decision: model.DecisionKeep, Download: model.DownloadResult{Status: model.DownloadFailed}},
	}}
	_ = s.store.Put(p)
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("pdf", "x.pdf")
	fw.Write([]byte("%PDF-1.4\nhello\n"))
	mw.Close()
	req, _ := http.NewRequest("POST", ts.URL+"/projects/up1/papers/a/upload", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, _ = client.Do(req)
	resp.Body.Close()
	p, _ = s.store.Get("up1")
	if d := p.Paper("a").Download; d.Status != model.DownloadOK || d.Method != "manual upload" || !strings.HasSuffix(d.Path, "2024-author-paywalled-paper.pdf") {
		t.Fatalf("upload: %+v", d)
	}
	// Non-PDF upload is rejected.
	body.Reset()
	mw = multipart.NewWriter(&body)
	fw, _ = mw.CreateFormFile("pdf", "x.pdf")
	fw.Write([]byte("<html>not a pdf"))
	mw.Close()
	req, _ = http.NewRequest("POST", ts.URL+"/projects/up1/papers/a/upload", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, _ = client.Do(req)
	resp.Body.Close()
	if !strings.Contains(resp.Header.Get("Location"), "not+a+PDF") {
		t.Fatalf("non-pdf accepted: %s", resp.Header.Get("Location"))
	}
	// Downloads view shows upload form and open-via-proxy link for failures.
	_ = s.store.Update("up1", func(p *model.Project) error {
		p.Papers = append(p.Papers, &model.Paper{ID: "b", Title: "Still failing", DOI: "10.1/b", Decision: model.DecisionKeep, Download: model.DownloadResult{Status: model.DownloadFailed}})
		return nil
	})
	resp, _ = client.Get(ts.URL + "/projects/up1?view=downloads")
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	html := string(b)
	if !strings.Contains(html, "Retry all 1 missing") || !strings.Contains(html, `/papers/b/upload`) || !strings.Contains(html, "https://ezproxy.uni.fi/login?url=https://doi.org/10.1/b") {
		t.Fatalf("downloads view:\n%s", html)
	}
}

// A fake browser that immediately reports an ezproxy cookie over the
// DevTools pipe, so the whole login flow can run without a display.
const fakeChrome = `#!/usr/bin/env python3
import os, json, sys
r = os.fdopen(3, 'rb'); w = os.fdopen(4, 'wb'); buf = b''
while True:
    ch = r.read(1)
    if not ch: sys.exit(0)
    if ch != b'\x00': buf += ch; continue
    m = json.loads(buf.decode()); buf = b''
    if m['method'] == 'Browser.close':
        w.write(json.dumps({'id': m['id'], 'result': {}}).encode() + b'\x00'); w.flush(); sys.exit(0)
    w.write(json.dumps({'id': m['id'], 'result': {'cookies': [{'name': 'ezproxy', 'value': 'FROMBROWSER', 'domain': '127.0.0.1', 'expires': 0}]}}).encode() + b'\x00'); w.flush()
`

func TestBrowserLoginFlow(t *testing.T) {
	st, _ := store.Open(t.TempDir())
	cfg := config.FromEnv()
	cfg.AIBackend, cfg.AnthropicAPIKey, cfg.BrowserFetch = "api", "", "off"
	bin := filepath.Join(t.TempDir(), "fakechrome")
	if err := os.WriteFile(bin, []byte(fakeChrome), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg.Browser = bin
	// Proxy "test" target: a local server standing in for the proxied publisher.
	// Like EZproxy: with a valid session, /login?url=… redirects to the proxied article.
	pub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/login") {
			http.Redirect(w, r, "/proxied/article", http.StatusFound)
			return
		}
		w.Write([]byte("<html>article</html>"))
	}))
	defer pub.Close()
	cfg.ProxyPrefix = pub.URL + "/login?url="
	s, err := New(cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, _ := client.PostForm(ts.URL+"/access/browser", nil)
	resp.Body.Close()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, done, _, _, _, _ := s.login.snapshot(); done {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	_, done, ok, msg, logs, _ := s.login.snapshot()
	if !done || !ok {
		t.Fatalf("login flow: done=%v ok=%v msg=%q logs=%v", done, ok, msg, logs)
	}
	a := s.loadAccess()
	if a.ProxyCookie != "ezproxy=FROMBROWSER" || !a.LastTestOK || !s.chain.Proxy().HasCookie() {
		t.Fatalf("settings after login: %+v", a)
	}
	resp, _ = client.Get(ts.URL + "/access")
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), "University access is working") || strings.Contains(string(b), "FROMBROWSER") {
		t.Fatalf("access page after login:\n%s", b)
	}
}
