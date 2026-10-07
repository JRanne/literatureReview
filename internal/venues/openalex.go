package venues

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// OpenAlexSource is the quality-relevant part of an OpenAlex source record.
type OpenAlexSource struct {
	ID          string    `json:"id"`
	DisplayName string    `json:"display_name"`
	Type        string    `json:"type"`
	IsCore      bool      `json:"is_core"`
	IsInDOAJ    bool      `json:"is_in_doaj"`
	HIndex      int       `json:"h_index"`
	Citedness2y float64   `json:"citedness_2y"`
	WorksCount  int       `json:"works_count"`
	Host        string    `json:"host"`
	CheckedAt   time.Time `json:"checked_at"`
	Found       bool      `json:"found"`
}

type oaSrc struct {
	ID           string `json:"id"`
	DisplayName  string `json:"display_name"`
	Type         string `json:"type"`
	IsCore       bool   `json:"is_core"`
	IsInDOAJ     bool   `json:"is_in_doaj"`
	WorksCount   int    `json:"works_count"`
	Host         string `json:"host_organization_name"`
	SummaryStats struct {
		HIndex      int     `json:"h_index"`
		Citedness2y float64 `json:"2yr_mean_citedness"`
	} `json:"summary_stats"`
}

// OpenAlex looks sources up by ISSN (cheap) or name (search, which OpenAlex
// rate-limits aggressively; a 429 puts name search on cooldown).
type OpenAlex struct {
	Base   string
	UA     string
	Email  string
	APIKey string
	HTTP   *http.Client

	mu       sync.Mutex
	cooldown time.Time
}

func NewOpenAlex(ua, email string) *OpenAlex {
	return &OpenAlex{Base: "https://api.openalex.org", UA: ua, Email: email, HTTP: &http.Client{Timeout: 20 * time.Second}}
}

func (o *OpenAlex) get(ctx context.Context, path string, out any) error {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	if o.Email != "" {
		path += sep + "mailto=" + url.QueryEscape(o.Email)
		sep = "&"
	}
	if o.APIKey != "" {
		path += sep + "api_key=" + url.QueryEscape(o.APIKey)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.Base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", o.UA)
	resp, err := o.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode == 429 {
		wait := 10 * time.Minute
		if ra, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && ra > 0 {
			wait = time.Duration(ra) * time.Second
		}
		o.mu.Lock()
		o.cooldown = time.Now().Add(wait)
		o.mu.Unlock()
		return errRateLimited
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("openalex sources: HTTP %d", resp.StatusCode)
	}
	return json.Unmarshal(data, out)
}

var errRateLimited = fmt.Errorf("openalex: rate limited")

func (o *OpenAlex) onCooldown() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return time.Now().Before(o.cooldown)
}

func convert(s oaSrc) OpenAlexSource {
	return OpenAlexSource{ID: s.ID, DisplayName: s.DisplayName, Type: s.Type, IsCore: s.IsCore, IsInDOAJ: s.IsInDOAJ, HIndex: s.SummaryStats.HIndex, Citedness2y: s.SummaryStats.Citedness2y, WorksCount: s.WorksCount, Host: s.Host, CheckedAt: time.Now(), Found: true}
}

// Lookup finds the source by ISSN, else by name (best similar match).
func (o *OpenAlex) Lookup(ctx context.Context, venue, issn string) OpenAlexSource {
	if issn != "" {
		var s oaSrc
		if err := o.get(ctx, "/sources/issn:"+url.PathEscape(strings.TrimSpace(issn)), &s); err == nil && s.ID != "" {
			return convert(s)
		}
	}
	name, _ := Canonical(venue)
	if name == "" || o.onCooldown() {
		return OpenAlexSource{CheckedAt: time.Now()}
	}
	var res struct {
		Results []oaSrc `json:"results"`
	}
	if err := o.get(ctx, "/sources?search="+url.QueryEscape(name)+"&per-page=5", &res); err != nil || len(res.Results) == 0 {
		return OpenAlexSource{CheckedAt: time.Now()}
	}
	var cands []string
	for _, r := range res.Results {
		cands = append(cands, r.DisplayName)
	}
	if i, _ := bestMatch(name, cands, 0.85); i >= 0 {
		return convert(res.Results[i])
	}
	return OpenAlexSource{CheckedAt: time.Now()}
}
