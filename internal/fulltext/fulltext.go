// Package fulltext extracts plain text from downloaded PDFs using the
// poppler `pdftotext` tool when it is installed. Without it the app falls
// back to abstracts.
package fulltext

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// MaxPages bounds extraction; background sections rarely need more.
const MaxPages = 40

// Available reports whether pdftotext is on PATH.
func Available() bool {
	_, err := exec.LookPath("pdftotext")
	return err == nil
}

var (
	multiSpace = regexp.MustCompile(`[ \t\x0c]+`)
	multiNL    = regexp.MustCompile(`\n{3,}`)
	hyphenNL   = regexp.MustCompile(`(\w)-\n(\w)`)
)

// Extract converts a PDF to cleaned text.
func Extract(ctx context.Context, pdfPath string) (string, error) {
	if !Available() {
		return "", errors.New("pdftotext not installed")
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "pdftotext", "-l", fmt.Sprint(MaxPages), "-enc", "UTF-8", "-nopgbrk", pdfPath, "-")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("pdftotext: %v: %s", err, strings.TrimSpace(errb.String()))
	}
	return Clean(out.String()), nil
}

// Clean normalises whitespace and re-joins hyphenated line breaks.
func Clean(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = hyphenNL.ReplaceAllString(s, "$1$2")
	s = multiSpace.ReplaceAllString(s, " ")
	var lines []string
	for _, l := range strings.Split(s, "\n") {
		lines = append(lines, strings.TrimSpace(l))
	}
	s = strings.Join(lines, "\n")
	s = multiNL.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

// ExtractToFile extracts pdfPath and writes the text next to the PDFs under
// dir, returning the text file path and its length in characters.
func ExtractToFile(ctx context.Context, pdfPath, dir string) (string, int, error) {
	text, err := Extract(ctx, pdfPath)
	if err != nil {
		return "", 0, err
	}
	if len(text) < 500 {
		return "", 0, fmt.Errorf("extracted only %d characters (scanned or image-only PDF?)", len(text))
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", 0, err
	}
	name := strings.TrimSuffix(filepath.Base(pdfPath), ".pdf") + ".txt"
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		return "", 0, err
	}
	return path, len(text), nil
}

// Excerpt returns up to budget characters of a text file, keeping the start
// (title, abstract, introduction) and the end (discussion, conclusion).
func Excerpt(path string, budget int) string {
	b, err := os.ReadFile(path)
	if err != nil || budget <= 0 {
		return ""
	}
	s := string(b)
	if len(s) <= budget {
		return s
	}
	head := budget * 2 / 3
	tail := budget - head
	return s[:head] + "\n[…]\n" + s[len(s)-tail:]
}
