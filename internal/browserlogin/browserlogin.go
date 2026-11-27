// Package browserlogin obtains a proxy session cookie by letting the user
// log in interactively in a Chromium-based browser that the app launches
// with a DevTools pipe. No passwords pass through the app; it only reads the
// resulting cookie via the DevTools protocol (Storage.getCookies).
package browserlogin

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Candidates are the Chromium-based binaries tried in order.
var Candidates = []string{"chromium", "chromium-browser", "google-chrome-stable", "google-chrome", "brave", "brave-browser", "microsoft-edge"}

// FindBrowser returns the first available Chromium-based browser.
func FindBrowser(override string) (string, error) {
	if override != "" {
		if p, err := exec.LookPath(override); err == nil {
			return p, nil
		}
		return "", fmt.Errorf("browser %q not found on PATH", override)
	}
	for _, c := range Candidates {
		if p, err := exec.LookPath(c); err == nil {
			return p, nil
		}
	}
	return "", errors.New("no Chromium-based browser found (chromium, google-chrome, brave); install one or paste the cookie manually")
}

// Cookie is a browser cookie.
type Cookie struct {
	Name    string  `json:"name"`
	Value   string  `json:"value"`
	Domain  string  `json:"domain"`
	Expires float64 `json:"expires"`
}

// Session drives one interactive login.
type Session struct {
	Binary     string
	ProfileDir string
	// LoginURL is opened in the browser (the proxied link).
	LoginURL string
	// ProxyHost is the host whose cookies we wait for, e.g. ezproxy.utu.fi.
	ProxyHost string
	// Match decides whether a cookie is the session cookie we need.
	Match func(c Cookie) bool
	// Ignore lists cookies known to be stale (the previously saved session):
	// they are never accepted, even if the profile still carries them.
	Ignore []Cookie
	// Poll interval and overall timeout.
	Poll    time.Duration
	Timeout time.Duration
	// Log receives progress lines.
	Log func(string)

	cmd    *exec.Cmd
	w      io.WriteCloser
	r      *bufio.Reader
	nextID int64
	mu     sync.Mutex
	stop   chan struct{}
	once   sync.Once
}

// DefaultMatch accepts EZproxy session cookies for the proxy's domain.
func DefaultMatch(proxyHost string) func(Cookie) bool {
	host := strings.ToLower(proxyHost)
	parent := host
	if parts := strings.Split(host, "."); len(parts) > 2 {
		parent = strings.Join(parts[1:], ".")
	}
	return func(c Cookie) bool {
		d := strings.TrimPrefix(strings.ToLower(c.Domain), ".")
		if d != host && d != parent && !strings.HasSuffix(host, "."+d) {
			return false
		}
		n := strings.ToLower(c.Name)
		return strings.HasPrefix(n, "ezproxy") && c.Value != ""
	}
}

func (s *Session) logf(format string, a ...any) {
	if s.Log != nil {
		s.Log(fmt.Sprintf(format, a...))
	}
}

// Run launches the browser, waits for the user to log in and returns the
// Cookie-header value (name=value; …) of the matching cookies. RunCookies
// returns the cookies themselves (with domains) as well.
func (s *Session) Run(ctx context.Context) (string, error) {
	hdr, _, err := s.RunCookies(ctx)
	return hdr, err
}

// RunCookies is Run returning the matched cookies too.
func (s *Session) RunCookies(ctx context.Context) (string, []Cookie, error) {
	if s.Poll == 0 {
		s.Poll = 2 * time.Second
	}
	if s.Timeout == 0 {
		s.Timeout = 10 * time.Minute
	}
	if s.Match == nil {
		s.Match = DefaultMatch(s.ProxyHost)
	}
	if err := os.MkdirAll(s.ProfileDir, 0o700); err != nil {
		return "", nil, err
	}
	// Pipe protocol: the browser reads commands from fd 3 and writes
	// responses to fd 4, each message a JSON object terminated by NUL.
	cmdIn, ourOut, err := os.Pipe() // we write ourOut → browser reads cmdIn (fd 3)
	if err != nil {
		return "", nil, err
	}
	ourIn, cmdOut, err := os.Pipe() // browser writes cmdOut (fd 4) → we read ourIn
	if err != nil {
		return "", nil, err
	}
	args := []string{
		"--remote-debugging-pipe",
		"--user-data-dir=" + s.ProfileDir,
		"--no-first-run", "--no-default-browser-check", "--disable-sync",
		"--password-store=basic",
		"--new-window", s.LoginURL,
	}
	cmd := exec.CommandContext(ctx, s.Binary, args...)
	cmd.ExtraFiles = []*os.File{cmdIn, cmdOut} // fd 3, fd 4
	cmd.Env = os.Environ()
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		return "", nil, fmt.Errorf("start browser: %w", err)
	}
	cmdIn.Close()
	cmdOut.Close()
	s.cmd, s.w, s.r = cmd, ourOut, bufio.NewReaderSize(ourIn, 1<<20)
	s.stop = make(chan struct{})
	defer s.Close()
	s.logf("Browser opened (%s). Log in to the proxy in that window; the app is waiting for the session cookie…", filepath.Base(s.Binary))

	deadline := time.Now().Add(s.Timeout)
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	// Cookies that exist before the user has logged in are leftovers of an
	// expired session (the profile keeps them across restarts); accepting
	// one would end the flow instantly with a dead cookie.
	stale := map[string]bool{}
	for _, c := range s.Ignore {
		stale[c.Name+"="+c.Value] = true
	}
	first := true
	for {
		cookies, err := s.getCookies()
		if err != nil {
			return "", nil, fmt.Errorf("devtools: %w", err)
		}
		if first {
			first = false
			n := 0
			for _, c := range cookies {
				if s.Match(c) && !stale[c.Name+"="+c.Value] {
					stale[c.Name+"="+c.Value] = true
					n++
				}
			}
			if n > 0 {
				s.logf("Ignoring %d stale proxy cookie(s) left from an earlier session; waiting for a fresh login.", n)
			}
		}
		var parts []string
		var matched []Cookie
		for _, c := range cookies {
			if s.Match(c) && !stale[c.Name+"="+c.Value] {
				parts = append(parts, c.Name+"="+c.Value)
				matched = append(matched, c)
			}
		}
		if len(parts) > 0 {
			s.logf("Session cookie received (%d cookie(s) for %s).", len(parts), s.ProxyHost)
			return strings.Join(parts, "; "), matched, nil
		}
		select {
		case <-ctx.Done():
			return "", nil, ctx.Err()
		case err := <-exited:
			return "", nil, fmt.Errorf("browser window was closed before a session cookie appeared (%v)", err)
		case <-time.After(s.Poll):
		}
		if time.Now().After(deadline) {
			return "", nil, errors.New("timed out waiting for the login (10 minutes)")
		}
	}
}

// Close asks the browser to quit and releases the pipes.
func (s *Session) Close() {
	s.once.Do(func() {
		if s.w != nil {
			_ = s.send("Browser.close", nil)
			s.w.Close()
		}
		if s.cmd != nil && s.cmd.Process != nil {
			done := make(chan struct{})
			go func() { _, _ = s.cmd.Process.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				_ = s.cmd.Process.Kill()
			}
		}
	})
}

type cdpMsg struct {
	ID     int64           `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params any             `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func (s *Session) send(method string, params any) error {
	id := atomic.AddInt64(&s.nextID, 1)
	b, _ := json.Marshal(cdpMsg{ID: id, Method: method, Params: params})
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.w.Write(append(b, 0))
	return err
}

// call sends a command and waits for its response (events are skipped).
func (s *Session) call(method string, params any) (json.RawMessage, error) {
	id := atomic.AddInt64(&s.nextID, 1)
	b, _ := json.Marshal(cdpMsg{ID: id, Method: method, Params: params})
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.w.Write(append(b, 0)); err != nil {
		return nil, err
	}
	for {
		raw, err := s.r.ReadBytes(0)
		if err != nil {
			return nil, err
		}
		raw = raw[:len(raw)-1]
		var m cdpMsg
		if json.Unmarshal(raw, &m) != nil || m.ID != id {
			continue
		}
		if m.Error != nil {
			return nil, errors.New(m.Error.Message)
		}
		return m.Result, nil
	}
}

func (s *Session) getCookies() ([]Cookie, error) {
	res, err := s.call("Storage.getCookies", map[string]any{})
	if err != nil {
		return nil, err
	}
	var out struct {
		Cookies []Cookie `json:"cookies"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return nil, err
	}
	return out.Cookies, nil
}

// ProxyHost extracts the host from an EZproxy prefix.
func ProxyHost(prefix string) (string, error) {
	u, err := url.Parse(prefix)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("invalid proxy prefix %q", prefix)
	}
	return u.Hostname(), nil
}
