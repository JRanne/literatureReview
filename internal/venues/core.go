package venues

import (
	"context"
	"encoding/csv"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// CORE is the Australian CORE conference ranking (A*, A, B, C), loaded from
// the portal's CSV export and cached on disk for 90 days.
type CORE struct {
	URL       string
	UA        string
	CachePath string
	HTTP      *http.Client
	entries   []coreEntry
	loaded    bool
}

type coreEntry struct {
	title, acronym, rank string
}

const coreExportURL = "https://portal.core.edu.au/conf-ranks/?search=&by=all&source=CORE2023&sort=atitle&page=1&do=Export"

func NewCORE(ua, cachePath string) *CORE {
	return &CORE{URL: coreExportURL, UA: ua, CachePath: cachePath, HTTP: &http.Client{Timeout: 60 * time.Second}}
}

func (c *CORE) load(ctx context.Context) {
	if c.loaded {
		return
	}
	c.loaded = true
	var data []byte
	if st, err := os.Stat(c.CachePath); err == nil && time.Since(st.ModTime()) < 90*24*time.Hour {
		data, _ = os.ReadFile(c.CachePath)
	}
	if len(data) == 0 {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL, nil)
		if err != nil {
			return
		}
		req.Header.Set("User-Agent", c.UA)
		resp, err := c.HTTP.Do(req)
		if err != nil {
			return
		}
		data, _ = io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if resp.StatusCode == 200 && len(data) > 1000 && c.CachePath != "" {
			_ = os.WriteFile(c.CachePath, data, 0o644)
		}
	}
	r := csv.NewReader(strings.NewReader(string(data)))
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	for {
		rec, err := r.Read()
		if err != nil {
			break
		}
		if len(rec) < 5 {
			continue
		}
		c.entries = append(c.entries, coreEntry{title: strings.TrimSpace(rec[1]), acronym: strings.TrimSpace(rec[2]), rank: strings.TrimSpace(rec[4])})
	}
}

// Rank returns the CORE rank for a venue ("A*", "A", "B", "C", "Unranked")
// or "" when the venue is not a ranked computing conference.
func (c *CORE) Rank(ctx context.Context, venue string) string {
	c.load(ctx)
	if len(c.entries) == 0 {
		return ""
	}
	name, acro := Canonical(venue)
	if acro != "" {
		for _, e := range c.entries {
			if strings.EqualFold(e.acronym, acro) && Similarity(name, e.title) >= 0.3 {
				return e.rank
			}
		}
	}
	var cands []string
	for _, e := range c.entries {
		cands = append(cands, e.title)
	}
	if i, _ := bestMatch(name, cands, 0.85); i >= 0 {
		return c.entries[i].rank
	}
	return ""
}
