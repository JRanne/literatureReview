package server

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"literaturereview/internal/model"
)

func (s *Server) handleFile(w http.ResponseWriter, r *http.Request) {
	id, name := r.PathValue("id"), filepath.Base(r.PathValue("name"))
	if strings.Contains(name, "..") || !strings.HasSuffix(name, ".pdf") {
		http.NotFound(w, r)
		return
	}
	path := filepath.Join(s.store.Dir(), id, "pdfs", name)
	if _, err := os.Stat(path); err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/pdf")
	http.ServeFile(w, r, path)
}

func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	format := strings.TrimPrefix(filepath.Ext(r.URL.Path), ".")
	var kept []*model.Paper
	var title string
	if err := s.store.Update(id, func(p *model.Project) error {
		p.EnsureBibKeys()
		title = p.Title
		for _, pp := range p.Kept() {
			c := *pp
			kept = append(kept, &c)
		}
		return nil
	}); err != nil {
		http.NotFound(w, r)
		return
	}
	switch format {
	case "json":
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Disposition", `attachment; filename="bibliography.json"`)
		writeJSON(w, kept)
	case "bib":
		w.Header().Set("Content-Type", "application/x-bibtex; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="bibliography.bib"`)
		fmt.Fprintf(w, "%% %s — %d kept papers, exported %s\n\n", title, len(kept), time.Now().Format(time.RFC3339))
		for _, pp := range kept {
			w.Write([]byte(bibtex(pp)))
		}
	default:
		http.NotFound(w, r)
	}
}

func bibtex(p *model.Paper) string {
	key := p.BibKey
	if key == "" {
		key = strings.ToLower(strings.ReplaceAll(p.FirstAuthorSurname(), " ", "")) + strconv.Itoa(p.Year)
	}
	kind := "article"
	v := strings.ToLower(p.Venue)
	switch {
	case p.Venue == "" || strings.HasPrefix(v, "arxiv") || strings.Contains(v, "ssrn") || strings.Contains(v, "preprint"):
		kind = "misc"
	case strings.Contains(v, "proceedings") || strings.Contains(v, "conference") || strings.Contains(v, "workshop") || strings.Contains(v, "symposium"):
		kind = "inproceedings"
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "@%s{%s,\n", kind, key)
	fmt.Fprintf(&sb, "  title = {%s},\n", bibEsc(p.Title))
	if len(p.Authors) > 0 {
		fmt.Fprintf(&sb, "  author = {%s},\n", bibEsc(strings.Join(p.Authors, " and ")))
	} else {
		// No author on record: give BibTeX a sort key so author-year styles
		// do not warn, and let the entry read as the venue/organisation.
		fmt.Fprintf(&sb, "  key = {%s},\n", bibEsc(key))
		if p.Venue != "" {
			fmt.Fprintf(&sb, "  author = {{%s}},\n", bibEsc(p.Venue))
		}
	}
	if p.Year > 0 {
		fmt.Fprintf(&sb, "  year = {%d},\n", p.Year)
	}
	if p.Venue != "" {
		field := "journal"
		if kind == "inproceedings" {
			field = "booktitle"
		} else if kind == "misc" {
			field = "howpublished"
		}
		fmt.Fprintf(&sb, "  %s = {%s},\n", field, bibEsc(p.Venue))
	}
	if p.DOI != "" {
		fmt.Fprintf(&sb, "  doi = {%s},\n", p.DOI)
	}
	if p.URL != "" {
		fmt.Fprintf(&sb, "  url = {%s},\n", p.URL)
	}
	if p.Download.Path != "" {
		fmt.Fprintf(&sb, "  file = {%s},\n", p.Download.Path)
	}
	if p.Abstract != "" {
		fmt.Fprintf(&sb, "  abstract = {%s},\n", bibEsc(p.Abstract))
	}
	sb.WriteString("}\n\n")
	return sb.String()
}

func bibEsc(s string) string {
	return strings.NewReplacer("{", "\\{", "}", "\\}", "%", "\\%", "&", "\\&").Replace(s)
}
