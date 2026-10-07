package jev

import (
	"context"
	"os"
	"testing"

	"literaturereview/internal/model"
)

// LIVE=1 JEV_API_KEY=… go test ./internal/jev -run Live -v
func TestLiveJev(t *testing.T) {
	key := os.Getenv("JEV_API_KEY")
	if os.Getenv("LIVE") == "" || key == "" {
		t.Skip("set LIVE=1 and JEV_API_KEY")
	}
	c := New(key, "", "")
	p := &model.Project{Question: "How do public-sector organisations adopt generative AI and what governance mechanisms do they use?", Exclusion: "medical diagnosis applications; purely technical model papers"}
	for _, pp := range []*model.Paper{
		{Title: "Generative AI is already widespread in the public sector", Venue: "arXiv", Year: 2024, Abstract: "Survey of 938 UK public service professionals on generative AI use, trust and workplace policy."},
		{Title: "Spectral Signatures of Large Language Models", Venue: "arXiv", Year: 2026, Abstract: "We analyse eigenvalue spectra of transformer weight matrices."},
		{Title: "Deep learning for retinal disease diagnosis", Venue: "Ophthalmology", Year: 2023, Abstract: "A CNN detects diabetic retinopathy from fundus images in a hospital setting."},
	} {
		a := c.Assess(context.Background(), p, pp)
		t.Logf("%-60s → score=%d verdict=%s rel=%.2f inc=%.2f exc=%.2f conf=%.2f err=%s", pp.Title, a.Score, a.Verdict, a.Relevance, a.Include, a.Exclude, a.Confidence, a.Error)
		if a.Error != "" {
			t.Fatal(a.Error)
		}
	}
}
