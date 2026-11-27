package browserlogin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDefaultMatch(t *testing.T) {
	m := DefaultMatch("ezproxy.utu.fi")
	cases := []struct {
		c    Cookie
		want bool
	}{
		{Cookie{Name: "ezproxy", Value: "abc", Domain: "ezproxy.utu.fi"}, true},
		{Cookie{Name: "ezproxyl", Value: "1", Domain: ".utu.fi"}, true},
		{Cookie{Name: "ezproxy", Value: "", Domain: "ezproxy.utu.fi"}, false},
		{Cookie{Name: "ezproxy", Value: "x", Domain: "ezproxy.other.fi"}, false},
		{Cookie{Name: "_ga", Value: "x", Domain: ".utu.fi"}, false},
	}
	for _, c := range cases {
		if got := m(c.c); got != c.want {
			t.Errorf("%+v → %v", c.c, got)
		}
	}
}

// fakeBrowser is a Python script speaking the DevTools pipe protocol on
// fds 3/4: it answers Storage.getCookies with no cookies twice, then with
// an ezproxy cookie, and exits on Browser.close.
const fakeBrowser = `#!/usr/bin/env python3
import os, json, sys
r = os.fdopen(3, 'rb'); w = os.fdopen(4, 'wb')
calls = 0
buf = b''
while True:
    ch = r.read(1)
    if not ch: sys.exit(0)
    if ch != b'\x00':
        buf += ch; continue
    m = json.loads(buf.decode()); buf = b''
    if m['method'] == 'Browser.close':
        w.write(json.dumps({'id': m['id'], 'result': {}}).encode() + b'\x00'); w.flush(); sys.exit(0)
    if m['method'] == 'Storage.getCookies':
        calls += 1
        # interleave an event to make sure events are skipped
        w.write(json.dumps({'method': 'Target.targetCreated', 'params': {}}).encode() + b'\x00')
        cookies = [] if calls < 3 else [
            {'name': '_ga', 'value': 'ga', 'domain': '.utu.fi', 'expires': 0},
            {'name': 'ezproxy', 'value': 'SESSION123', 'domain': 'ezproxy.utu.fi', 'expires': 0},
            {'name': 'ezproxyl', 'value': '1', 'domain': 'ezproxy.utu.fi', 'expires': 0}]
        w.write(json.dumps({'id': m['id'], 'result': {'cookies': cookies}}).encode() + b'\x00'); w.flush()
`

func TestRunWithFakeBrowser(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "fakechrome")
	if err := os.WriteFile(bin, []byte(fakeBrowser), 0o755); err != nil {
		t.Fatal(err)
	}
	var logs []string
	s := &Session{Binary: bin, ProfileDir: filepath.Join(dir, "profile"), LoginURL: "https://ezproxy.utu.fi/login?url=https://x", ProxyHost: "ezproxy.utu.fi", Poll: 20 * time.Millisecond, Timeout: 5 * time.Second, Log: func(m string) { logs = append(logs, m) }}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cookie, err := s.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cookie != "ezproxy=SESSION123; ezproxyl=1" {
		t.Fatalf("cookie %q", cookie)
	}
	if len(logs) < 2 || !strings.Contains(logs[len(logs)-1], "Session cookie received") {
		t.Fatalf("logs %v", logs)
	}
}

func TestRunBrowserClosedEarly(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "quitter")
	_ = os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755)
	s := &Session{Binary: bin, ProfileDir: dir, LoginURL: "https://x", ProxyHost: "ezproxy.utu.fi", Poll: 20 * time.Millisecond, Timeout: 2 * time.Second}
	if _, err := s.Run(context.Background()); err == nil {
		t.Fatal("expected error when the browser exits")
	}
}

func TestProxyHost(t *testing.T) {
	h, err := ProxyHost("https://ezproxy.utu.fi/login?url=")
	if err != nil || h != "ezproxy.utu.fi" {
		t.Fatal(h, err)
	}
}

// A cookie that is already in the profile when the browser opens (or that
// matches the saved session) must not end the login flow.
func TestRunIgnoresStaleCookie(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "fakechrome")
	// Always report the old cookie; report the new one only from the 3rd poll.
	script := strings.Replace(fakeBrowser,
		"cookies = [] if calls < 3 else [",
		"cookies = [{'name': 'ezproxy', 'value': 'OLD', 'domain': '.ezproxy.utu.fi'}] + ([] if calls < 3 else [", 1)
	script = strings.Replace(script, "]\n        w.write", "])\n        w.write", 1)
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	s := &Session{Binary: bin, ProfileDir: filepath.Join(dir, "profile"), LoginURL: "https://ezproxy.utu.fi/login?url=https://x", ProxyHost: "ezproxy.utu.fi", Poll: 20 * time.Millisecond, Timeout: 5 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cookie, err := s.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(cookie, "OLD") || !strings.Contains(cookie, "ezproxy=") {
		t.Fatalf("stale cookie accepted: %q", cookie)
	}
}
