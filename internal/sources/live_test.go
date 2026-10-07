package sources

import (
	"context"
	"os"
	"testing"
	"time"

	"literaturereview/internal/config"
)

// TestLiveSources hits the real APIs. Run with LIVE=1 go test ./internal/sources -run Live -v
func TestLiveSources(t *testing.T) {
	if os.Getenv("LIVE") == "" {
		t.Skip("set LIVE=1 to query real databases")
	}
	cfg := config.FromEnv()
	reg := NewRegistry(cfg)
	queries := map[string]string{
		"openalex":        "generative AI adoption public sector",
		"semanticscholar": "generative AI adoption public sector",
		"crossref":        "generative AI adoption public sector",
		"pubmed":          `("artificial intelligence"[tiab]) AND adoption[tiab] AND hospital*[tiab]`,
		"core":            `title:("generative AI") AND adoption`,
	}
	for _, src := range reg.All() {
		src := src
		t.Run(src.Name(), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			res, err := src.Search(ctx, queries[src.Name()], Options{YearFrom: 2020, YearTo: 2026, Limit: 5})
			if err != nil {
				t.Fatalf("%s: %v", src.Name(), err)
			}
			papers := res.Papers
			t.Logf("URL: %s  total hits: %d", res.URL, res.Total)
			if len(papers) == 0 {
				t.Fatalf("%s: no results", src.Name())
			}
			for _, p := range papers {
				t.Logf("%s | %d | %s | doi=%s abs=%d pdf=%d", p.Title, p.Year, firstN(p.Authors), p.DOI, len(p.Abstract), len(p.PDFURLs))
			}
		})
	}
}

func firstN(a []string) string {
	if len(a) == 0 {
		return "-"
	}
	return a[0]
}
