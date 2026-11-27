package server

// Library access settings (university proxy) editable at runtime, manual
// PDF upload, and retry-all for failed downloads.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"literaturereview/internal/browserlogin"
	"literaturereview/internal/fetch"
	"literaturereview/internal/fulltext"
	"literaturereview/internal/model"
)

// loginState tracks the interactive browser login.
type loginState struct {
	mu      sync.Mutex
	running bool
	done    bool
	ok      bool
	message string
	log     []string
	started time.Time
	cancel  context.CancelFunc
}

func (l *loginState) snapshot() (running, done, ok bool, message string, log []string, started time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.running, l.done, l.ok, l.message, append([]string{}, l.log...), l.started
}

func (l *loginState) add(m string) {
	l.mu.Lock()
	l.log = append(l.log, time.Now().Format("15:04:05")+"  "+m)
	l.message = m
	l.mu.Unlock()
}

// handleAccessBrowser launches the browser login flow in the background.
func (s *Server) handleAccessBrowser(w http.ResponseWriter, r *http.Request) {
	a := s.loadAccess()
	back := "/access"
	if ret := r.FormValue("return"); strings.HasPrefix(ret, "/") && !strings.HasPrefix(ret, "//") {
		back = ret
	}
	if strings.TrimSpace(a.ProxyPrefix) == "" {
		http.Redirect(w, r, "/access?error="+urlEsc("set the proxy prefix first"), http.StatusSeeOther)
		return
	}
	host, err := browserlogin.ProxyHost(a.ProxyPrefix)
	if err != nil {
		http.Redirect(w, r, "/access?error="+urlEsc(err.Error()), http.StatusSeeOther)
		return
	}
	bin, err := browserlogin.FindBrowser(s.cfg.Browser)
	if err != nil {
		http.Redirect(w, r, "/access?error="+urlEsc(err.Error()), http.StatusSeeOther)
		return
	}
	s.login.mu.Lock()
	if s.login.running {
		s.login.mu.Unlock()
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.login.running, s.login.done, s.login.ok, s.login.log, s.login.started, s.login.cancel = true, false, false, nil, time.Now(), cancel
	s.login.mu.Unlock()
	target := "https://www.sciencedirect.com/"
	sess := &browserlogin.Session{
		Binary:     bin,
		ProfileDir: filepath.Join(s.store.Dir(), "browser-profile"),
		LoginURL:   a.ProxyPrefix + target,
		ProxyHost:  host,
		Ignore:     s.browserCookies(), // the saved session is what we are replacing
		Log:        s.login.add,
	}
	go func() {
		defer cancel()
		cookie, cookies, err := sess.RunCookies(ctx)
		finish := func(ok bool, msg string) {
			s.login.mu.Lock()
			s.login.running, s.login.done, s.login.ok, s.login.message = false, true, ok, msg
			s.login.log = append(s.login.log, time.Now().Format("15:04:05")+"  "+msg)
			s.login.mu.Unlock()
		}
		if err != nil {
			finish(false, "Login did not complete: "+err.Error())
			return
		}
		a := s.loadAccess()
		a.ProxyCookie = cookie
		a.ProxyCookies = cookies
		a.UpdatedAt = time.Now()
		if err := s.applyAccess(a); err != nil {
			finish(false, err.Error())
			return
		}
		_ = s.saveAccess(a)
		s.login.add("Cookie saved. Testing access through the proxy…")
		tctx, tcancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer tcancel()
		fin, terr := s.chain.Proxy().Test(tctx)
		a = s.loadAccess()
		a.LastTestAt = time.Now()
		if terr != nil {
			a.LastTestOK, a.LastTestMsg = false, terr.Error()
			_ = s.saveAccess(a)
			finish(false, "Cookie saved but the access test failed: "+terr.Error())
			return
		}
		a.LastTestOK, a.LastTestMsg = true, "reached "+fin
		_ = s.saveAccess(a)
		finish(true, "University access is working (reached "+fin+"). Papers that were waiting for a login are being retried.")
		s.afterSessionRefresh()
	}()
	http.Redirect(w, r, back, http.StatusSeeOther)
}

func (s *Server) handleAccessBrowserCancel(w http.ResponseWriter, r *http.Request) {
	s.login.mu.Lock()
	if s.login.cancel != nil {
		s.login.cancel()
	}
	s.login.mu.Unlock()
	http.Redirect(w, r, "/access", http.StatusSeeOther)
}

// handleAccessStatus renders the polling partial.
func (s *Server) handleAccessStatus(w http.ResponseWriter, r *http.Request) {
	running, done, ok, msg, logs, started := s.login.snapshot()
	if !running {
		w.Header().Set("HX-Refresh", "true")
	}
	s.render(w, "login_status", map[string]any{"Running": running, "Done": done, "OK": ok, "Message": msg, "Log": logs, "Started": started})
}

// accessSettings is persisted in DATA_DIR/settings.json and overrides the
// PROXY_* environment variables.
type accessSettings struct {
	ProxyPrefix string `json:"proxy_prefix"`
	ProxyCookie string `json:"proxy_cookie"`
	// ProxyCookies keeps name/value/domain so the headless browser can be
	// given the exact cookies the interactive login produced.
	ProxyCookies []browserlogin.Cookie `json:"proxy_cookies,omitempty"`
	UpdatedAt    time.Time             `json:"updated_at"`
	LastTestOK   bool                  `json:"last_test_ok"`
	LastTestAt   time.Time             `json:"last_test_at,omitempty"`
	LastTestMsg  string                `json:"last_test_msg,omitempty"`
}

func (s *Server) settingsPath() string { return filepath.Join(s.store.Dir(), "settings.json") }

func (s *Server) loadAccess() accessSettings {
	a := accessSettings{ProxyPrefix: s.cfg.ProxyPrefix, ProxyCookie: s.cfg.ProxyCookie}
	if b, err := os.ReadFile(s.settingsPath()); err == nil {
		var saved accessSettings
		if json.Unmarshal(b, &saved) == nil {
			if saved.ProxyPrefix != "" {
				a.ProxyPrefix = saved.ProxyPrefix
			}
			if saved.ProxyCookie != "" {
				a.ProxyCookie = saved.ProxyCookie
				a.ProxyCookies = saved.ProxyCookies
			}
			a.UpdatedAt, a.LastTestOK, a.LastTestAt, a.LastTestMsg = saved.UpdatedAt, saved.LastTestOK, saved.LastTestAt, saved.LastTestMsg
		}
	}
	return a
}

func (s *Server) saveAccess(a accessSettings) error {
	b, _ := json.MarshalIndent(a, "", "  ")
	return os.WriteFile(s.settingsPath(), b, 0o600)
}

// applyAccess rebuilds the proxy stage of the fetch chain from settings.
func (s *Server) applyAccess(a accessSettings) error {
	if strings.TrimSpace(a.ProxyPrefix) == "" {
		s.chain.SetProxy(nil)
		return nil
	}
	px, err := fetch.NewProxy(strings.TrimSpace(a.ProxyPrefix), strings.TrimSpace(a.ProxyCookie), s.cfg.UserAgent)
	if err != nil {
		return err
	}
	s.chain.SetProxy(px)
	s.chain.Session.Reset() // a new cookie gets a fresh chance
	return nil
}

type accessData struct {
	Base           baseData
	A              accessSettings
	Chain          []string
	Error          string
	Message        string
	CookieSet      bool
	Browser        string
	LoginRunning   bool
	LoginDone      bool
	LoginOK        bool
	LoginMessage   string
	LoginLog       []string
	ScimagoCount   int
	ScimagoSet     bool
	SessionExpired bool
	SessionMsg     string
	SessionAt      time.Time
}

func (s *Server) handleAccess(w http.ResponseWriter, r *http.Request) {
	a := s.loadAccess()
	d := accessData{Base: s.base(), A: a, Chain: s.chain.Names(), Error: r.URL.Query().Get("error"), Message: r.URL.Query().Get("msg"), CookieSet: a.ProxyCookie != ""}
	d.A.ProxyCookie = "" // never echo the secret back into the form
	if bin, err := browserlogin.FindBrowser(s.cfg.Browser); err == nil {
		d.Browser = filepath.Base(bin)
	}
	d.LoginRunning, d.LoginDone, d.LoginOK, d.LoginMessage, d.LoginLog, _ = s.login.snapshot()
	d.SessionExpired, d.SessionMsg, d.SessionAt = s.chain.Session.Status()
	d.ScimagoCount, d.ScimagoSet = s.scimagoInfo()
	s.render(w, "access", d)
}

func (s *Server) handleAccessSave(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	a := s.loadAccess()
	a.ProxyPrefix = strings.TrimSpace(r.FormValue("proxy_prefix"))
	if c := strings.TrimSpace(r.FormValue("proxy_cookie")); c != "" {
		a.ProxyCookie = normalizeCookie(c)
		a.ProxyCookies = cookiesFromHeader(a.ProxyCookie, a.ProxyPrefix)
	}
	if r.FormValue("clear_cookie") == "1" {
		a.ProxyCookie, a.ProxyCookies = "", nil
	}
	a.UpdatedAt = time.Now()
	a.LastTestOK, a.LastTestMsg = false, ""
	if err := s.applyAccess(a); err != nil {
		http.Redirect(w, r, "/access?error="+urlEsc(err.Error()), http.StatusSeeOther)
		return
	}
	if err := s.saveAccess(a); err != nil {
		http.Redirect(w, r, "/access?error="+urlEsc(err.Error()), http.StatusSeeOther)
		return
	}
	if r.FormValue("test") == "1" {
		s.handleAccessTest(w, r)
		return
	}
	http.Redirect(w, r, "/access?msg="+urlEsc("saved"), http.StatusSeeOther)
}

// normalizeCookie accepts "ezproxy=abc", "Cookie: ezproxy=abc; other=1" or a
// Netscape cookies.txt line and returns a Cookie header value.
func normalizeCookie(c string) string {
	c = strings.TrimSpace(strings.TrimPrefix(c, "Cookie:"))
	// cookies.txt lines: domain \t flag \t path \t secure \t expiry \t name \t value
	if strings.Count(c, "\t") >= 6 {
		var parts []string
		for _, line := range strings.Split(c, "\n") {
			f := strings.Split(line, "\t")
			if len(f) >= 7 && !strings.HasPrefix(strings.TrimSpace(f[0]), "#") {
				parts = append(parts, strings.TrimSpace(f[5])+"="+strings.TrimSpace(f[6]))
			}
		}
		return strings.Join(parts, "; ")
	}
	return strings.TrimSpace(c)
}

func (s *Server) handleAccessTest(w http.ResponseWriter, r *http.Request) {
	a := s.loadAccess()
	px := s.chain.Proxy()
	if px == nil {
		http.Redirect(w, r, "/access?error="+urlEsc("no proxy configured"), http.StatusSeeOther)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	fin, err := px.Test(ctx)
	a.LastTestAt = time.Now()
	if err != nil {
		a.LastTestOK, a.LastTestMsg = false, err.Error()
		_ = s.saveAccess(a)
		http.Redirect(w, r, "/access?error="+urlEsc("Access test failed: "+err.Error()), http.StatusSeeOther)
		return
	}
	a.LastTestOK, a.LastTestMsg = true, "reached "+fin
	_ = s.saveAccess(a)
	go s.afterSessionRefresh()
	http.Redirect(w, r, "/access?msg="+urlEsc("Access test passed: the proxy session is valid ("+fin+")"), http.StatusSeeOther)
}

// handleUpload stores a manually downloaded PDF for a paper.
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	id, pid := r.PathValue("id"), r.PathValue("pid")
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		http.Redirect(w, r, "/projects/"+id+"?view=downloads&error="+urlEsc(err.Error()), http.StatusSeeOther)
		return
	}
	f, _, err := r.FormFile("pdf")
	if err != nil {
		http.Redirect(w, r, "/projects/"+id+"?view=downloads&error="+urlEsc("no file uploaded"), http.StatusSeeOther)
		return
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 100<<20))
	if err != nil || !fetch.IsPDF(data) {
		http.Redirect(w, r, "/projects/"+id+"?view=downloads&error="+urlEsc("the uploaded file is not a PDF"), http.StatusSeeOther)
		return
	}
	var target *model.Paper
	_ = s.store.View(id, func(p *model.Project) {
		if pp := p.Paper(pid); pp != nil {
			c := *pp
			target = &c
		}
	})
	if target == nil {
		http.NotFound(w, r)
		return
	}
	dir, err := s.store.ProjectDir(id)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	pdfDir := filepath.Join(dir, "pdfs")
	path, err := fetch.WritePDF(pdfDir, target, data)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	var textPath string
	var textChars int
	if fulltext.Available() {
		if tp, n, err := fulltext.ExtractToFile(r.Context(), path, filepath.Join(dir, "text")); err == nil {
			textPath, textChars = tp, n
		}
	}
	_ = s.store.Update(id, func(p *model.Project) error {
		pp := p.Paper(pid)
		if pp == nil {
			return errors.New("paper vanished")
		}
		pp.Download = model.DownloadResult{Status: model.DownloadOK, Method: "manual upload", Path: path, Bytes: int64(len(data)),
			Attempts: append(pp.Download.Attempts, model.Attempt{Method: "manual upload", OK: true, Message: fmt.Sprintf("uploaded %d KB", len(data)/1024), At: time.Now()})}
		if textPath != "" {
			pp.TextPath, pp.TextChars = textPath, textChars
		}
		return nil
	})
	http.Redirect(w, r, "/projects/"+id+"?view=downloads", http.StatusSeeOther)
}

// handleRetryAll re-runs the chain for every kept paper without a PDF.
func (s *Server) handleRetryAll(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var todo []model.Paper
	_ = s.store.Update(id, func(p *model.Project) error {
		for _, pp := range p.Kept() {
			if pp.Download.Status != model.DownloadOK {
				pp.Download = model.DownloadResult{Status: model.DownloadPending}
				todo = append(todo, *pp)
			}
		}
		return nil
	})
	if len(todo) == 0 {
		http.Redirect(w, r, "/projects/"+id+"?view=downloads&error="+urlEsc("every kept paper already has a PDF"), http.StatusSeeOther)
		return
	}
	if err := s.runJob(id, "download", func(ctx context.Context, lg jobLogger) error {
		return s.downloadJob(ctx, id, todo, lg)
	}); err != nil {
		http.Redirect(w, r, "/projects/"+id+"?view=downloads&error="+urlEsc(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/projects/"+id+"?view=downloads", http.StatusSeeOther)
}

// cookiesFromHeader derives domain-scoped cookies from a pasted header value,
// scoped to the proxy host so EZproxy-rewritten subdomains receive them.
func cookiesFromHeader(hdr, prefix string) []browserlogin.Cookie {
	host, err := browserlogin.ProxyHost(prefix)
	if err != nil {
		return nil
	}
	dom := "." + host
	var out []browserlogin.Cookie
	for _, kv := range strings.Split(hdr, ";") {
		name, val, ok := strings.Cut(strings.TrimSpace(kv), "=")
		if ok && name != "" {
			out = append(out, browserlogin.Cookie{Name: strings.TrimSpace(name), Value: strings.TrimSpace(val), Domain: dom})
		}
	}
	return out
}

// browserCookies returns the cookies the headless download stage injects.
func (s *Server) browserCookies() []browserlogin.Cookie {
	a := s.loadAccess()
	if len(a.ProxyCookies) > 0 {
		return a.ProxyCookies
	}
	return cookiesFromHeader(a.ProxyCookie, a.ProxyPrefix)
}
