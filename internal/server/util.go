package server

import (
	"encoding/json"
	"html"
	"html/template"
	"net/http"
	"regexp"
	"strings"
)

func writeJSON(w http.ResponseWriter, v any) {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

var (
	citeRe = regexp.MustCompile(`\[(@[^\]]+)\]`)
	boldRe = regexp.MustCompile(`\*\*(.+?)\*\*`)
	italRe = regexp.MustCompile(`(^|[^*])\*([^*\n]+?)\*`)
	codeRe = regexp.MustCompile("`([^`]+)`")
	olRe   = regexp.MustCompile(`^\d+[.)]\s+`)
)

// renderMarkdown converts the small Markdown subset Claude emits (headings,
// lists, bold/italic, paragraphs) to safe HTML. Text is escaped first.
func renderMarkdown(src string) template.HTML {
	var sb strings.Builder
	lines := strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n")
	inUL, inOL, inP := false, false, false
	closeLists := func() {
		if inUL {
			sb.WriteString("</ul>\n")
			inUL = false
		}
		if inOL {
			sb.WriteString("</ol>\n")
			inOL = false
		}
	}
	closeP := func() {
		if inP {
			sb.WriteString("</p>\n")
			inP = false
		}
	}
	inline := func(s string) string {
		s = html.EscapeString(s)
		s = boldRe.ReplaceAllString(s, "<strong>$1</strong>")
		s = italRe.ReplaceAllString(s, "$1<em>$2</em>")
		s = codeRe.ReplaceAllString(s, "<code>$1</code>")
		s = citeRe.ReplaceAllString(s, `<span class="cite">[$1]</span>`)
		return s
	}
	for _, raw := range lines {
		line := strings.TrimRight(raw, " \t")
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "":
			closeP()
			closeLists()
		case strings.HasPrefix(trimmed, "#"):
			closeP()
			closeLists()
			level := 0
			for level < len(trimmed) && trimmed[level] == '#' && level < 6 {
				level++
			}
			text := strings.TrimSpace(trimmed[level:])
			sb.WriteString("<h" + string(rune('0'+level+1)) + ">" + inline(text) + "</h" + string(rune('0'+level+1)) + ">\n")
		case strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "* ") || strings.HasPrefix(trimmed, "• "):
			closeP()
			if inOL {
				sb.WriteString("</ol>\n")
				inOL = false
			}
			if !inUL {
				sb.WriteString("<ul>\n")
				inUL = true
			}
			sb.WriteString("<li>" + inline(strings.TrimSpace(trimmed[2:])) + "</li>\n")
		case olRe.MatchString(trimmed):
			closeP()
			if inUL {
				sb.WriteString("</ul>\n")
				inUL = false
			}
			if !inOL {
				sb.WriteString("<ol>\n")
				inOL = true
			}
			sb.WriteString("<li>" + inline(olRe.ReplaceAllString(trimmed, "")) + "</li>\n")
		default:
			if inUL || inOL {
				// continuation line inside a list item
				sb.WriteString(" " + inline(trimmed))
				continue
			}
			if !inP {
				sb.WriteString("<p>")
				inP = true
			} else {
				sb.WriteString(" ")
			}
			sb.WriteString(inline(trimmed))
		}
	}
	closeP()
	closeLists()
	return template.HTML(sb.String())
}
