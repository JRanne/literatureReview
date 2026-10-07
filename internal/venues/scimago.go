package venues

import (
	"encoding/csv"
	"os"
	"strconv"
	"strings"
	"sync"
)

// Scimago holds journal ranks imported from a Scimago Journal Rank CSV
// (scimagojr.com → "Download data"; semicolon-separated, columns include
// Title, Issn, SJR, SJR Best Quartile, H index). The site blocks automated
// downloads, so the user uploads the file through the app.
type Scimago struct {
	Path    string
	mu      sync.Mutex
	loaded  bool
	byISSN  map[string]scimagoEntry
	byTitle map[string]scimagoEntry
	Count   int
}

type scimagoEntry struct {
	Title    string
	SJR      float64
	Quartile string // Q1..Q4 or ""
	HIndex   int
}

func NewScimago(path string) *Scimago { return &Scimago{Path: path} }

// Load parses the CSV (idempotent; call Reload after an upload).
func (s *Scimago) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loaded {
		return nil
	}
	s.loaded = true
	s.byISSN, s.byTitle = map[string]scimagoEntry{}, map[string]scimagoEntry{}
	b, err := os.ReadFile(s.Path)
	if err != nil {
		return err
	}
	r := csv.NewReader(strings.NewReader(string(b)))
	r.Comma = ';'
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	header, err := r.Read()
	if err != nil {
		return err
	}
	col := map[string]int{}
	for i, h := range header {
		col[strings.ToLower(strings.TrimSpace(h))] = i
	}
	get := func(rec []string, name string) string {
		if i, ok := col[name]; ok && i < len(rec) {
			return strings.TrimSpace(rec[i])
		}
		return ""
	}
	for {
		rec, err := r.Read()
		if err != nil {
			break
		}
		e := scimagoEntry{Title: get(rec, "title"), Quartile: strings.TrimSpace(get(rec, "sjr best quartile"))}
		e.SJR, _ = strconv.ParseFloat(strings.ReplaceAll(get(rec, "sjr"), ",", "."), 64)
		e.HIndex, _ = strconv.Atoi(get(rec, "h index"))
		if e.Quartile == "-" {
			e.Quartile = ""
		}
		for _, issn := range strings.Split(get(rec, "issn"), ",") {
			issn = strings.ToUpper(strings.TrimSpace(issn))
			if len(issn) == 8 {
				issn = issn[:4] + "-" + issn[4:]
			}
			if issn != "" {
				s.byISSN[issn] = e
			}
		}
		if e.Title != "" {
			s.byTitle[norm(e.Title)] = e
		}
		s.Count++
	}
	return nil
}

// Reload forces re-parsing (after an upload).
func (s *Scimago) Reload() error {
	s.mu.Lock()
	s.loaded = false
	s.mu.Unlock()
	return s.Load()
}

// Lookup returns the quartile, SJR and h-index for a venue.
func (s *Scimago) Lookup(venue, issn string) (quartile string, sjr float64, h int, ok bool) {
	if err := s.Load(); err != nil {
		return "", 0, 0, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if issn != "" {
		if e, ok := s.byISSN[strings.ToUpper(strings.TrimSpace(issn))]; ok {
			return e.Quartile, e.SJR, e.HIndex, true
		}
	}
	name, _ := Canonical(venue)
	if e, ok := s.byTitle[name]; ok {
		return e.Quartile, e.SJR, e.HIndex, true
	}
	return "", 0, 0, false
}
