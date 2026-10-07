package browserlogin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// LIVE=1 go test ./internal/browserlogin -run Live -v  (needs chromium + network)
func TestLiveHeadlessDownload(t *testing.T) {
	if os.Getenv("LIVE") == "" {
		t.Skip("LIVE not set")
	}
	bin, err := FindBrowser("")
	if err != nil {
		t.Skip(err)
	}
	d := &Downloader{Binary: bin, ProfileDir: filepath.Join(t.TempDir(), "profile"), Timeout: 30 * time.Second}
	for _, u := range []string{"https://arxiv.org/pdf/1706.03762", "https://papers.ssrn.com/sol3/Delivery.cfm?abstractid=4544943"} {
		start := time.Now()
		data, fin, err := d.Fetch(context.Background(), u)
		if err != nil {
			t.Logf("%s → error after %s: %v", u, time.Since(start).Round(time.Second), err)
			continue
		}
		t.Logf("%s → %d bytes from %s in %s (pdf=%v)", u, len(data), fin, time.Since(start).Round(time.Second), strings.HasPrefix(string(data), "%PDF"))
	}
}
