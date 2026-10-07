// Package venues rates publication venues: AIS Senior Scholars' basket,
// major IS conferences, and the Finnish Publication Forum (JUFO) level
// looked up through its public REST API.
package venues

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Rating summarises a venue's standing across several registries.
type Rating struct {
	Venue        string    `json:"venue"`
	ISSN         string    `json:"issn,omitempty"`
	JufoID       string    `json:"jufo_id,omitempty"`
	JufoLevel    int       `json:"jufo_level"` // -1 unknown, 0..3
	JufoName     string    `json:"jufo_name,omitempty"`
	NorwayLevel  int       `json:"norway_level"`      // -1 unknown, 0..2 (from the JUFO record)
	DenmarkLevel int       `json:"denmark_level"`     // -1 unknown, 0..2
	Basket       string    `json:"basket,omitempty"`  // "AIS basket of 8" / "AIS basket of 11"
	ISConf       string    `json:"is_conf,omitempty"` // major IS conference acronym
	CORERank     string    `json:"core_rank,omitempty"`
	OACore       bool      `json:"oa_core"`   // OpenAlex core source (≈ Scopus/WoS coverage)
	DOAJ         bool      `json:"doaj"`      // listed in DOAJ
	HIndex       int       `json:"h_index"`   // OpenAlex source h-index
	Citedness    float64   `json:"citedness"` // OpenAlex 2-year mean citedness
	SourceType   string    `json:"source_type,omitempty"`
	Publisher    string    `json:"publisher,omitempty"`
	SJRQuartile  string    `json:"sjr_quartile,omitempty"` // Scimago Q1..Q4 (from an uploaded CSV)
	SJR          float64   `json:"sjr,omitempty"`
	OAFound      bool      `json:"oa_found"`
	CheckedAt    time.Time `json:"checked_at"`
}

// Tier condenses the signals: top, good, basic, below, unknown.
func (r Rating) Tier() string {
	switch {
	case r.JufoLevel == 3 || r.Basket == "AIS basket of 8" || r.CORERank == "A*":
		return "top"
	case r.JufoLevel == 2 || r.Basket != "" || r.ISConf != "" || r.CORERank == "A" || r.NorwayLevel == 2 || r.SJRQuartile == "Q1":
		return "good"
	case r.JufoLevel == 1 || r.NorwayLevel == 1 || r.DenmarkLevel >= 1 || r.CORERank == "B" || r.CORERank == "C" || r.OACore || r.SJRQuartile == "Q2" || (r.DOAJ && r.HIndex >= 20) || ReputablePublisher(r.Publisher) || ReputablePublisher(r.Venue):
		return "basic"
	case r.JufoLevel == 0:
		return "below"
	case r.OAFound && r.SourceType == "journal" && !r.OACore && !r.DOAJ && r.HIndex < 15:
		return "below" // a known journal with no index, no DOAJ listing and almost no citations
	default:
		return "unknown"
	}
}

// ReputablePublisher recognises established scholarly publishers/societies
// in a publisher or venue string.
func ReputablePublisher(s string) bool {
	l := strings.ToLower(s)
	for _, p := range []string{"ieee", "acm", "springer", "elsevier", "wiley", "taylor & francis", "taylor and francis", "sage", "emerald", "informs", "oxford university press", "cambridge university press", "mit press", "palgrave", "lecture notes in", "ais electronic library", "association for information systems", "de gruyter", "inderscience", "igi global", "iop publishing", "nature", "plos", "mdpi", "frontiers media", "world scientific", "hicss", "scitepress", "science and technology publications", "aaai", "usenix", "lncs"} {
		if strings.Contains(l, p) {
			return true
		}
	}
	return false
}

// Label is a short badge text listing the signals found.
func (r Rating) Label() string {
	var parts []string
	if r.Basket != "" {
		parts = append(parts, r.Basket)
	}
	if r.ISConf != "" {
		parts = append(parts, r.ISConf)
	}
	if r.JufoLevel >= 0 {
		parts = append(parts, "JUFO "+strconv.Itoa(r.JufoLevel))
	}
	if r.NorwayLevel >= 0 {
		parts = append(parts, "NO "+strconv.Itoa(r.NorwayLevel))
	}
	if r.CORERank != "" && r.CORERank != "Unranked" {
		parts = append(parts, "CORE "+r.CORERank)
	}
	if r.SJRQuartile != "" {
		parts = append(parts, "SJR "+r.SJRQuartile)
	}
	if r.OACore {
		parts = append(parts, "Scopus/WoS-level")
	}
	if r.DOAJ {
		parts = append(parts, "DOAJ")
	}
	if r.Publisher != "" && ReputablePublisher(r.Publisher) {
		parts = append(parts, r.Publisher)
	}
	if r.HIndex > 0 {
		parts = append(parts, "h"+strconv.Itoa(r.HIndex))
	}
	return strings.Join(parts, " · ")
}

// basket8 are the AIS Senior Scholars' basket of eight (lowercased, normalised).
var basket8 = map[string]bool{
	"mis quarterly": true, "misq": true, "management information systems quarterly": true,
	"information systems research":                       true,
	"journal of management information systems":          true,
	"journal of the association for information systems": true,
	"european journal of information systems":            true,
	"information systems journal":                        true,
	"journal of information technology":                  true,
	"journal of strategic information systems":           true, "the journal of strategic information systems": true,
}

// basket11 adds the three journals of the 2023 extension.
var basket11 = map[string]bool{
	"decision support systems":   true,
	"information and management": true, "information & management": true,
	"information and organization": true, "information & organization": true,
}

// isConfs maps name fragments to conference acronyms.
var isConfs = []struct{ frag, acr string }{
	{"international conference on information systems", "ICIS"},
	{"european conference on information systems", "ECIS"},
	{"hawaii international conference on system sciences", "HICSS"},
	{"pacific asia conference on information systems", "PACIS"},
	{"americas conference on information systems", "AMCIS"},
	{"australasian conference on information systems", "ACIS"},
	{"wirtschaftsinformatik", "WI"},
	{"design science research in information systems", "DESRIST"},
	{"international conference on design science research", "DESRIST"},
	{"mediterranean conference on information systems", "MCIS"},
	{"scandinavian conference on information systems", "SCIS"},
}

var nonAlnum = regexp.MustCompile(`[^a-z0-9& ]+`)

func norm(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "&", "and")
	s = nonAlnum.ReplaceAllString(s, " ")
	return strings.Join(strings.Fields(s), " ")
}

// Static rates a venue from the built-in lists only.
func Static(venue string) Rating {
	r := Rating{Venue: venue, JufoLevel: -1, NorwayLevel: -1, DenmarkLevel: -1}
	n, acro := Canonical(venue)
	for _, c := range isConfs {
		if strings.EqualFold(acro, c.acr) {
			r.ISConf = c.acr
		}
	}
	switch {
	case basket8[n]:
		r.Basket = "AIS basket of 8"
	case basket11[n]:
		r.Basket = "AIS basket of 11"
	}
	if r.ISConf == "" {
		for _, c := range isConfs {
			if strings.Contains(n, c.frag) {
				r.ISConf = c.acr
				break
			}
		}
	}
	return r
}

// Jufo queries the Publication Forum REST API with a persistent cache.
type Jufo struct {
	Base      string
	UA        string
	HTTP      *http.Client
	CachePath string
	mu        sync.Mutex
	cache     map[string]Rating // key: "issn:…" or "name:…"
	lim       time.Time
}

// NewJufo creates a client; cachePath may be empty (no persistence).
func NewJufo(ua, cachePath string) *Jufo {
	j := &Jufo{Base: "https://jufo-rest.csc.fi/v1.1", UA: ua, HTTP: &http.Client{Timeout: 20 * time.Second}, CachePath: cachePath, cache: map[string]Rating{}}
	if cachePath != "" {
		if b, err := os.ReadFile(cachePath); err == nil {
			_ = json.Unmarshal(b, &j.cache)
		}
	}
	return j
}

func (j *Jufo) save() {
	if j.CachePath == "" {
		return
	}
	b, _ := json.MarshalIndent(j.cache, "", " ")
	_ = os.WriteFile(j.CachePath, b, 0o644)
}

func (j *Jufo) get(ctx context.Context, path string, out any) error {
	// polite pacing: ≤ 4 requests/second
	j.mu.Lock()
	if wait := time.Until(j.lim); wait > 0 {
		time.Sleep(wait)
	}
	j.lim = time.Now().Add(250 * time.Millisecond)
	j.mu.Unlock()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, j.Base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", j.UA)
	resp, err := j.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != 200 {
		return fmt.Errorf("jufo: HTTP %d", resp.StatusCode)
	}
	return json.Unmarshal(data, out)
}

type hit struct {
	JufoID string `json:"Jufo_ID"`
	Name   string `json:"Name"`
	Type   string `json:"Type"`
}

// Rate looks a venue up by ISSN first, then by exact name; the result is
// merged with the static lists. Unknown venues get JufoLevel -1.
func (j *Jufo) Rate(ctx context.Context, venue, issn string) Rating {
	r := Static(venue)
	r.ISSN = issn
	key := "v3:name:" + norm(venue)
	if issn != "" {
		key = "v3:issn:" + strings.ToUpper(strings.TrimSpace(issn))
	}
	j.mu.Lock()
	cached, ok := j.cache[key]
	j.mu.Unlock()
	if ok && cached.CheckedAt.After(time.Now().Add(-180*24*time.Hour)) {
		cached.Basket, cached.ISConf, cached.Venue = r.Basket, r.ISConf, venue
		return cached
	}
	var hits []hit
	if issn != "" {
		_ = j.get(ctx, "/etsi.php?issn="+url.QueryEscape(issn), &hits)
	}
	if len(hits) == 0 && strings.TrimSpace(venue) != "" {
		canon, _ := Canonical(venue)
		queries := []string{canon}
		// a shorter query (first 4 informative words) helps JUFO's substring search
		if words := strings.Fields(noiseRe.ReplaceAllString(canon, " ")); len(words) > 4 {
			queries = append(queries, strings.Join(words[:4], " "))
		}
		for _, q := range queries {
			var byName []hit
			_ = j.get(ctx, "/etsi.php?nimi="+url.QueryEscape(q), &byName)
			var cands []string
			for _, h := range byName {
				cands = append(cands, h.Name)
			}
			// Strict: only near-identical token sets count (JMIS must not match
			// "Journal of Information Systems Engineering and Management").
			if i, _ := bestMatch(canon, cands, 0.9); i >= 0 {
				hits = []hit{byName[i]}
				break
			}
		}
	}
	if len(hits) > 0 {
		var det []map[string]any
		if err := j.get(ctx, "/kanava/"+url.PathEscape(hits[0].JufoID), &det); err == nil && len(det) > 0 {
			r.JufoID = hits[0].JufoID
			r.JufoName = fmt.Sprint(det[0]["Name"])
			r.JufoLevel = levelOf(det[0])
			r.NorwayLevel = intField(det[0], "Norway_Level")
			r.DenmarkLevel = intField(det[0], "Denmark_Level")
		}
	}
	r.CheckedAt = time.Now()
	j.mu.Lock()
	j.cache[key] = r
	j.save()
	j.mu.Unlock()
	return r
}

func intField(d map[string]any, key string) int {
	if v, ok := d[key]; ok {
		if n, err := strconv.Atoi(strings.TrimSpace(fmt.Sprint(v))); err == nil {
			return n
		}
	}
	return -1
}

// levelOf reads the current JUFO level: "Level" when present, else the
// most recent Jufo_<year> field.
func levelOf(d map[string]any) int {
	if v, ok := d["Level"]; ok {
		if n, err := strconv.Atoi(strings.TrimSpace(fmt.Sprint(v))); err == nil {
			return n
		}
	}
	best, bestYear := -1, 0
	for k, v := range d {
		if !strings.HasPrefix(k, "Jufo_") {
			continue
		}
		year, err := strconv.Atoi(strings.TrimPrefix(k, "Jufo_"))
		if err != nil || year < bestYear {
			continue
		}
		if n, err := strconv.Atoi(strings.TrimSpace(fmt.Sprint(v))); err == nil {
			best, bestYear = n, year
		}
	}
	return best
}

// Rater combines JUFO, OpenAlex, CORE, CrossRef (ISSN resolution) and an
// uploaded Scimago file.
type Rater struct {
	Jufo     *Jufo
	OA       *OpenAlex
	CORE     *CORE
	Scimago  *Scimago
	CrossRef *CrossRefJournals
}

// Rate produces the combined rating. Without an ISSN it first tries to
// resolve one from the name via CrossRef, so the ISSN-keyed registries can
// be used (far more reliable than name matching).
func (rt *Rater) Rate(ctx context.Context, venue, issn string) Rating {
	if issn == "" && rt.CrossRef != nil && rt.Jufo != nil {
		key := "v3:crossref-issn:" + norm(venue)
		rt.Jufo.mu.Lock()
		cached, ok := rt.Jufo.cache[key]
		rt.Jufo.mu.Unlock()
		if ok {
			issn = cached.ISSN
		} else if found, err := rt.CrossRef.ISSN(ctx, venue); err == nil {
			issn = found
			rt.Jufo.mu.Lock()
			rt.Jufo.cache[key] = Rating{ISSN: found, CheckedAt: time.Now()}
			rt.Jufo.save()
			rt.Jufo.mu.Unlock()
		}
	}
	r := Static(venue)
	if rt.Jufo != nil {
		r = rt.Jufo.Rate(ctx, venue, issn)
	}
	if rt.OA != nil {
		src := rt.OA.Lookup(ctx, venue, issn)
		r.OAFound, r.OACore, r.DOAJ, r.HIndex, r.Citedness, r.SourceType, r.Publisher = src.Found, src.IsCore, src.IsInDOAJ, src.HIndex, src.Citedness2y, src.Type, src.Host
	}
	if rt.CORE != nil {
		r.CORERank = rt.CORE.Rank(ctx, venue)
	}
	if rt.Scimago != nil {
		if q, sjr, h, ok := rt.Scimago.Lookup(venue, issn); ok {
			r.SJRQuartile, r.SJR = q, sjr
			if r.HIndex == 0 {
				r.HIndex = h
			}
		}
	}
	r.ISSN = issn
	return r
}
