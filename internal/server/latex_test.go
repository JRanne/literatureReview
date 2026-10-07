package server

import (
	"strings"
	"testing"
)

func TestMarkdownToLaTeX(t *testing.T) {
	md := "## Background\n\nFirms adopt GenAI [@smith2024adoption; @lee2023trust] & see 50% gains. Smith et al. [@smith2024adoption] *argue* **strongly**.\n\n### Themes\n\n- one_item\n- two\n\nAs @lee2023trust notes, R&D matters."
	tex := markdownToLaTeX(md)
	for _, want := range []string{
		`\section{Background}`, `\subsection{Themes}`,
		`\citep{smith2024adoption,lee2023trust}`, `\citet{lee2023trust}`,
		`\&`, `50\%`, `\emph{argue}`, `\textbf{strongly}`,
		"\\begin{itemize}\n  \\item one\\_item\n  \\item two\n\\end{itemize}",
	} {
		if !strings.Contains(tex, want) {
			t.Errorf("missing %q in:\n%s", want, tex)
		}
	}
	if strings.Contains(tex, "@") || strings.Contains(tex, "\x00") {
		t.Fatalf("unconverted markers:\n%s", tex)
	}
	keys := citationKeys(md)
	if len(keys) != 2 || keys[0] != "smith2024adoption" || keys[1] != "lee2023trust" {
		t.Fatalf("keys %v", keys)
	}
}
