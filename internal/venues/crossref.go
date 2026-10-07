package venues

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// CrossRefJournals resolves a journal name to its ISSN through CrossRef's
// journal registry, so that ISSN-based lookups (JUFO, OpenAlex, Scimago)
// work for records that came without one.
type CrossRefJournals struct {
	Base  string
	UA    string
	Email string
	HTTP  *http.Client
}

func NewCrossRefJournals(ua, email string) *CrossRefJournals {
	return &CrossRefJournals{Base: "https://api.crossref.org", UA: ua, Email: email, HTTP: &http.Client{Timeout: 20 * time.Second}}
}

// ISSN returns the ISSN of the journal whose title matches venue closely.
func (c *CrossRefJournals) ISSN(ctx context.Context, venue string) (string, error) {
	name, _ := Canonical(venue)
	if name == "" {
		return "", nil
	}
	q := url.Values{}
	q.Set("query", name)
	q.Set("rows", "5")
	if c.Email != "" {
		q.Set("mailto", c.Email)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Base+"/journals?"+q.Encode(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", c.UA)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("crossref journals: HTTP %d", resp.StatusCode)
	}
	var out struct {
		Message struct {
			Items []struct {
				Title string   `json:"title"`
				ISSN  []string `json:"ISSN"`
			} `json:"items"`
		} `json:"message"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", err
	}
	var cands []string
	for _, it := range out.Message.Items {
		cands = append(cands, it.Title)
	}
	if i, _ := bestMatch(name, cands, 0.9); i >= 0 && len(out.Message.Items[i].ISSN) > 0 {
		return strings.ToUpper(out.Message.Items[i].ISSN[0]), nil
	}
	return "", nil
}
