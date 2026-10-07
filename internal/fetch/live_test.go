package fetch

import (
	"context"
	"os"
	"testing"
	"time"

	"literaturereview/internal/config"
	"literaturereview/internal/model"
)

// TestLiveChain downloads real open-access PDFs. LIVE=1 go test ./internal/fetch -run Live -v
func TestLiveChain(t *testing.T) {
	if os.Getenv("LIVE") == "" {
		t.Skip("set LIVE=1 to download real PDFs")
	}
	cfg := config.FromEnv()
	ch := NewChain(cfg)
	dir := t.TempDir()
	papers := []*model.Paper{
		{ID: "b", Title: "PLOS OA article", Authors: []string{"Someone"}, Year: 2020, DOI: "10.1371/journal.pone.0229017"},
		{ID: "c", Title: "Paywalled Nature article (expected to fail off-campus)", Authors: []string{"X"}, Year: 2021, DOI: "10.1038/nature14539"},
	}
	for _, p := range papers {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		res := ch.Run(ctx, p, dir)
		cancel()
		t.Logf("%s → %s via %q (%d bytes)", p.Title, res.Status, res.Method, res.Bytes)
		for _, a := range res.Attempts {
			t.Logf("   %s: ok=%v %s", a.Method, a.OK, a.Message)
		}
	}
}
