package fetch

import (
	"testing"

	"literaturereview/internal/model"
)

func TestPDFLinksFromHTML(t *testing.T) {
	html := []byte(`<html><head>
<meta name="citation_title" content="x">
<meta name="citation_pdf_url" content="https://pub.example.org/doi/pdf/10.1/abc">
<meta content="/alt.pdf" name="citation_pdf_url">
</head><body><a href="/content/foo.pdf?download=true">PDF</a></body></html>`)
	got := PDFLinksFromHTML(html, "https://pub.example.org/doi/10.1/abc")
	want := []string{
		"https://pub.example.org/doi/pdf/10.1/abc",
		"https://pub.example.org/alt.pdf",
		"https://pub.example.org/content/foo.pdf?download=true",
	}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("link %d: got %q want %q", i, got[i], want[i])
		}
	}
}

func TestIsPDF(t *testing.T) {
	if !IsPDF([]byte("%PDF-1.7\n...")) || IsPDF([]byte("<html>")) {
		t.Fatal("IsPDF wrong")
	}
}

func TestSlug(t *testing.T) {
	p := &model.Paper{Title: "Generative AI Adoption in Public Sector: A Multi-Case Study of Finnish Agencies", Authors: []string{"Maija Virtanen", "A. Other"}, Year: 2024}
	if got := p.Slug(); got != "2024-virtanen-generative-ai-adoption-in-public-sector-a-multi" {
		t.Fatalf("got %q", got)
	}
}
