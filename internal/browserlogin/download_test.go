package browserlogin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A fake browser for the download flow: answers the setup calls, then emits
// a navigation event, a non-PDF response (must be continued) and finally a
// PDF response (must be captured).
const fakeDownloader = `#!/usr/bin/env python3
import os, json, sys, base64
r = os.fdopen(3, 'rb'); w = os.fdopen(4, 'wb'); buf = b''
def send(o): w.write(json.dumps(o).encode() + b'\x00'); w.flush()
pdf = base64.b64encode(b'%PDF-1.4\n' + b'x' * 2000).decode()
while True:
    ch = r.read(1)
    if not ch: sys.exit(0)
    if ch != b'\x00': buf += ch; continue
    m = json.loads(buf.decode()); buf = b''
    meth = m['method']; sid = m.get('sessionId', '')
    if meth == 'Browser.close': send({'id': m['id'], 'result': {}}); sys.exit(0)
    if meth == 'Target.createTarget': send({'id': m['id'], 'result': {'targetId': 'T1'}}); continue
    if meth == 'Target.attachToTarget': send({'id': m['id'], 'result': {'sessionId': 'S1'}}); continue
    if meth == 'Page.navigate':
        send({'id': m['id'], 'result': {'frameId': 'F'}})
        send({'sessionId': 'S1', 'method': 'Page.frameNavigated', 'params': {'frame': {'url': m['params']['url']}}})
        send({'sessionId': 'S1', 'method': 'Fetch.requestPaused', 'params': {'requestId': 'R1', 'responseStatusCode': 200, 'responseHeaders': [{'name': 'Content-Type', 'value': 'text/html'}], 'request': {'url': 'https://pub/landing'}}})
        continue
    if meth == 'Fetch.continueRequest':
        send({'id': m['id'], 'result': {}})
        if m['params']['requestId'] == 'R1':
            send({'sessionId': 'S1', 'method': 'Fetch.requestPaused', 'params': {'requestId': 'R2', 'responseStatusCode': 200, 'responseHeaders': [{'name': 'content-type', 'value': 'application/pdf'}], 'request': {'url': 'https://pub/file.pdf'}}})
        continue
    if meth == 'Fetch.getResponseBody': send({'id': m['id'], 'result': {'body': pdf, 'base64Encoded': True}}); continue
    send({'id': m['id'], 'result': {}})
`

func TestDownloaderCapturesPDF(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "fakechrome")
	if err := os.WriteFile(bin, []byte(fakeDownloader), 0o755); err != nil {
		t.Fatal(err)
	}
	d := &Downloader{Binary: bin, ProfileDir: filepath.Join(dir, "profile"), Timeout: 5 * time.Second}
	data, fin, err := d.Fetch(context.Background(), "https://pub/landing")
	if err != nil {
		t.Fatal(err)
	}
	if fin != "https://pub/file.pdf" || !strings.HasPrefix(string(data), "%PDF-1.4") || len(data) < 2000 {
		t.Fatalf("result %s %d", fin, len(data))
	}
}

func TestDownloaderTimesOut(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "idle")
	// Answers everything with empty results and never emits a PDF.
	_ = os.WriteFile(bin, []byte(strings.Replace(fakeDownloader, "send({'sessionId': 'S1', 'method': 'Fetch.requestPaused'", "pass #", -1)), 0o755)
	d := &Downloader{Binary: bin, ProfileDir: filepath.Join(dir, "profile"), Timeout: 1 * time.Second}
	if _, _, err := d.Fetch(context.Background(), "https://pub/landing"); err == nil || !strings.Contains(err.Error(), "no PDF response") {
		t.Fatalf("expected timeout, got %v", err)
	}
}

func TestDownloaderInjectsCookiesAndAbortsOnLogin(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "fakechrome")
	script := strings.Replace(fakeDownloader,
		"    if meth == 'Page.navigate':",
		"    if meth == 'Storage.setCookies':\n        assert m['params']['cookies'][0]['domain'] == '.ezproxy.utu.fi', m\n        send({'id': m['id'], 'result': {}}); continue\n    if meth == 'Page.navigate':", 1)
	// After navigate, report a redirect to the SSO host instead of a PDF.
	script = strings.Replace(script,
		"        send({'sessionId': 'S1', 'method': 'Page.frameNavigated', 'params': {'frame': {'url': m['params']['url']}}})",
		"        send({'sessionId': 'S1', 'method': 'Page.frameNavigated', 'params': {'frame': {'url': 'https://sso.utu.fi/auth/realms/x/login-actions/authenticate'}}})", 1)
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	d := &Downloader{Binary: bin, ProfileDir: filepath.Join(dir, "profile"), Timeout: 5 * time.Second,
		Cookies: func() []Cookie { return []Cookie{{Name: "ezproxy", Value: "v", Domain: ".ezproxy.utu.fi"}} }}
	start := time.Now()
	_, _, err := d.Fetch(context.Background(), "https://ezproxy.utu.fi/login?url=https://www.sciencedirect.com/x")
	if err == nil || !strings.Contains(err.Error(), "login page") || time.Since(start) > 4*time.Second {
		t.Fatalf("expected fast login abort, got %v after %s", err, time.Since(start))
	}
}
