package server

import (
	"regexp"
	"strings"
)

var (
	citeGroupRe = regexp.MustCompile(`\[(@[^\]]+)\]`)
	citeKeyRe   = regexp.MustCompile(`@([A-Za-z0-9_:\-]+)`)
	narrCiteRe  = regexp.MustCompile(`(^|[^\[\w@])@([A-Za-z0-9_:\-]+)`)
	mdBoldRe    = regexp.MustCompile(`\*\*(.+?)\*\*`)
	mdItalRe    = regexp.MustCompile(`(^|[^*])\*([^*\n]+?)\*`)
)

// citationKeys lists every citation key used in a pandoc-style Markdown text.
func citationKeys(md string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range citeKeyRe.FindAllStringSubmatch(md, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	return out
}

// latexEscape escapes LaTeX special characters in prose.
func latexEscape(s string) string {
	r := strings.NewReplacer(`\`, `\textbackslash{}`, `&`, `\&`, `%`, `\%`, `$`, `\$`, `#`, `\#`, `_`, `\_`, `{`, `\{`, `}`, `\}`, `~`, `\textasciitilde{}`, `^`, `\textasciicircum{}`)
	return r.Replace(s)
}

// markdownToLaTeX converts the Markdown subset the synthesis uses (headings,
// paragraphs, bold/italic, bullet lists, pandoc citations) into LaTeX with
// natbib/biblatex-compatible \citep / \citet commands.
func markdownToLaTeX(md string) string {
	// Protect citations before escaping: replace with placeholders.
	type ph struct{ tex string }
	var phs []ph
	put := func(tex string) string {
		phs = append(phs, ph{tex})
		return "\x00" + string(rune('A'+len(phs)-1)) + "\x00"
	}
	md = citeGroupRe.ReplaceAllStringFunc(md, func(m string) string {
		keys := citeKeyRe.FindAllStringSubmatch(m, -1)
		var ks []string
		for _, k := range keys {
			ks = append(ks, k[1])
		}
		return put(`\citep{` + strings.Join(ks, ",") + `}`)
	})
	md = narrCiteRe.ReplaceAllStringFunc(md, func(m string) string {
		sub := narrCiteRe.FindStringSubmatch(m)
		return sub[1] + put(`\citet{`+sub[2]+`}`)
	})

	var out strings.Builder
	lines := strings.Split(strings.ReplaceAll(md, "\r\n", "\n"), "\n")
	inList, inPara := false, false
	closeList := func() {
		if inList {
			out.WriteString("\\end{itemize}\n")
			inList = false
		}
	}
	closePara := func() {
		if inPara {
			out.WriteString("\n\n")
			inPara = false
		}
	}
	inline := func(s string) string {
		s = latexEscape(s)
		s = mdBoldRe.ReplaceAllString(s, `\textbf{$1}`)
		s = mdItalRe.ReplaceAllString(s, `$1\emph{$2}`)
		return s
	}
	for _, raw := range lines {
		t := strings.TrimSpace(raw)
		switch {
		case t == "":
			closePara()
			closeList()
		case strings.HasPrefix(t, "### "):
			closePara()
			closeList()
			out.WriteString("\\subsection{" + inline(strings.TrimPrefix(t, "### ")) + "}\n")
		case strings.HasPrefix(t, "## "):
			closePara()
			closeList()
			out.WriteString("\\section{" + inline(strings.TrimPrefix(t, "## ")) + "}\n")
		case strings.HasPrefix(t, "# "):
			closePara()
			closeList()
			out.WriteString("\\section{" + inline(strings.TrimPrefix(t, "# ")) + "}\n")
		case strings.HasPrefix(t, "- ") || strings.HasPrefix(t, "* "):
			closePara()
			if !inList {
				out.WriteString("\\begin{itemize}\n")
				inList = true
			}
			out.WriteString("  \\item " + inline(strings.TrimSpace(t[2:])) + "\n")
		default:
			if inList {
				out.WriteString("  " + inline(t) + "\n")
				continue
			}
			if inPara {
				out.WriteString("\n")
			}
			out.WriteString(inline(t))
			inPara = true
		}
	}
	closePara()
	closeList()
	tex := out.String()
	for i, p := range phs {
		tex = strings.ReplaceAll(tex, "\x00"+string(rune('A'+i))+"\x00", p.tex)
	}
	return strings.TrimSpace(tex) + "\n"
}
