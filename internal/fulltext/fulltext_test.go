package fulltext

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClean(t *testing.T) {
	in := "Infor-\nmation   systems\n\n\n\nare  studied.\x0c"
	if got := Clean(in); got != "Information systems\n\nare studied." {
		t.Fatalf("got %q", got)
	}
}

func TestExcerpt(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	_ = os.WriteFile(p, []byte(strings.Repeat("A", 100)+strings.Repeat("Z", 100)), 0o644)
	ex := Excerpt(p, 30)
	if !strings.HasPrefix(ex, "AAAA") || !strings.HasSuffix(ex, "ZZZZ") || !strings.Contains(ex, "[…]") {
		t.Fatalf("excerpt %q", ex)
	}
}

// A minimal valid PDF with one text line, enough for pdftotext.
const tinyPDF = "%PDF-1.4\n1 0 obj<</Type/Catalog/Pages 2 0 R>>endobj\n2 0 obj<</Type/Pages/Kids[3 0 R]/Count 1>>endobj\n3 0 obj<</Type/Page/Parent 2 0 R/MediaBox[0 0 300 100]/Contents 4 0 R/Resources<</Font<</F1 5 0 R>>>>>>endobj\n4 0 obj<</Length 60>>stream\nBT /F1 18 Tf 20 40 Td (Hello literature review world) Tj ET\nendstream\nendobj\n5 0 obj<</Type/Font/Subtype/Type1/BaseFont/Helvetica>>endobj\ntrailer<</Root 1 0 R>>"

func TestExtractWithPdftotext(t *testing.T) {
	if !Available() {
		t.Skip("pdftotext not installed")
	}
	dir := t.TempDir()
	pdf := filepath.Join(dir, "x.pdf")
	_ = os.WriteFile(pdf, []byte(tinyPDF), 0o644)
	text, err := Extract(context.Background(), pdf)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "Hello literature review world") {
		t.Fatalf("text %q", text)
	}
	if _, _, err := ExtractToFile(context.Background(), pdf, dir); err == nil {
		t.Fatal("tiny text should be rejected as too short")
	}
}
