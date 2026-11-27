package server

import (
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"literaturereview/internal/model"
)

func (s *Server) funcs() template.FuncMap {
	return template.FuncMap{
		"join":  strings.Join,
		"add":   func(a, b int) int { return a + b },
		"list":  func(items ...string) []string { return items },
		"split": strings.Split,
		"mulf":  func(a, b float64) float64 { return a * b },
		"dict": func(kv ...any) (map[string]any, error) {
			if len(kv)%2 != 0 {
				return nil, errors.New("dict needs key/value pairs")
			}
			m := map[string]any{}
			for i := 0; i < len(kv); i += 2 {
				k, ok := kv[i].(string)
				if !ok {
					return nil, errors.New("dict keys must be strings")
				}
				m[k] = kv[i+1]
			}
			return m, nil
		},
		"authors": func(a []string) string {
			if len(a) == 0 {
				return "Unknown authors"
			}
			if len(a) > 4 {
				return strings.Join(a[:3], ", ") + " et al."
			}
			return strings.Join(a, ", ")
		},
		"scoreClass": func(n int) string {
			switch {
			case n >= 70:
				return "hi"
			case n >= 40:
				return "mid"
			default:
				return "lo"
			}
		},
		"sourceLabel": func(name string) string {
			if src, ok := s.reg.Get(name); ok {
				return src.Label()
			}
			return name
		},
		"basename": filepath.Base,
		"fmtTime": func(t time.Time) string {
			if t.IsZero() {
				return ""
			}
			return t.Local().Format("2006-01-02 15:04")
		},
		"pct": func(done, total int) int {
			if total == 0 {
				return 0
			}
			return done * 100 / total
		},
		"kb": func(b int64) string { return fmt.Sprintf("%d KB", b/1024) },
		"elapsed": func(t time.Time) string {
			if t.IsZero() {
				return ""
			}
			d := time.Since(t).Round(time.Second)
			if d < time.Minute {
				return fmt.Sprintf("%ds", int(d.Seconds()))
			}
			return fmt.Sprintf("%dm %02ds", int(d.Minutes()), int(d.Seconds())%60)
		},
		"markdown":   renderMarkdown,
		"stageIndex": model.StageIndex,
		"seq": func(n int) []int {
			out := make([]int, n)
			for i := range out {
				out[i] = i
			}
			return out
		},
	}
}

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("template %s: %v", name, err)
		http.Error(w, "template error: "+err.Error(), 500)
	}
}

type sourceView struct {
	Name, Label, Syntax string
	Checked             bool
}

func (s *Server) sourceViews(selected []string) []sourceView {
	sel := map[string]bool{}
	for _, n := range selected {
		sel[n] = true
	}
	var out []sourceView
	for _, src := range s.reg.All() {
		out = append(out, sourceView{Name: src.Name(), Label: src.Label(), Syntax: src.Syntax(), Checked: len(selected) == 0 || sel[src.Name()]})
	}
	return out
}

type baseData struct {
	AIEnabled   bool
	AIBackend   string
	Model       string
	JevEnabled  bool
	JevModel    string
	Screener    string
	EmailSet    bool
	ProxySet    bool
	ProxyCookie bool
	ProxyPrefix string
}

func (s *Server) base() baseData {
	return baseData{
		AIEnabled:   s.ai.Enabled(),
		AIBackend:   s.ai.BackendName(),
		Model:       s.ai.ModelLabel(),
		JevEnabled:  s.useJev(),
		JevModel:    s.cfg.JevModel,
		Screener:    s.cfg.Screener,
		EmailSet:    s.cfg.ContactEmail != "",
		ProxySet:    s.chain.Proxy() != nil,
		ProxyCookie: s.chain.Proxy() != nil && s.chain.Proxy().HasCookie(),
		ProxyPrefix: func() string {
			if px := s.chain.Proxy(); px != nil {
				return px.Prefix()
			}
			return ""
		}(),
	}
}
