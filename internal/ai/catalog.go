package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// ModelInfo is one model the account can use, as the Models API lists it.
type ModelInfo struct {
	ID          string    `json:"id"`
	DisplayName string    `json:"display_name"`
	CreatedAt   time.Time `json:"created_at"`
}

// families are the model lines offered as "Latest <family>", cheapest first.
// Choosing one tracks new releases: the Claude CLI resolves these aliases to
// its newest model itself, and on the API the newest listed id is used.
var families = []string{"haiku", "sonnet", "opus", "fable"}

func isFamily(id string) bool {
	for _, f := range families {
		if id == f {
			return true
		}
	}
	return false
}

func familyOf(id string) string {
	for _, f := range families {
		if strings.HasPrefix(id, "claude-"+f+"-") {
			return f
		}
	}
	return ""
}

// builtinModels stands in for the Models API when it cannot be reached, so
// the selectors and "Latest" choices keep working offline.
var builtinModels = []ModelInfo{
	{ID: "claude-fable-5-2", DisplayName: "Claude Fable 5.2", CreatedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)},
	{ID: "claude-opus-5-5", DisplayName: "Claude Opus 5.5", CreatedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)},
	{ID: "claude-sonnet-5-5", DisplayName: "Claude Sonnet 5.5", CreatedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)},
	{ID: "claude-haiku-5-5", DisplayName: "Claude Haiku 5.5", CreatedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)},
}

var familyNotes = map[string]string{
	"haiku":  "cheapest; adequate for bulk screening",
	"sonnet": "fast and economical",
	"opus":   "strong; recommended for the background section",
	"fable":  "strongest and most expensive",
}

// Catalog lists the models offered in the selectors and resolves "Latest"
// choices. The zero value and a nil *Catalog behave like the offline API
// catalog, so tests and a disabled backend still render the selectors.
type Catalog struct {
	list func(context.Context) ([]ModelInfo, error)
	// passthrough: the backend (the Claude CLI) resolves aliases itself.
	passthrough bool
	ttl         time.Duration
	now         func() time.Time

	mu      sync.Mutex
	models  []ModelInfo // newest first
	fetched time.Time
}

// NewAPICatalog lists models with list (APIBackend.ListModels) and refreshes
// the list after ttl, so models released later appear without a restart.
func NewAPICatalog(list func(context.Context) ([]ModelInfo, error), ttl time.Duration) *Catalog {
	return &Catalog{list: list, ttl: ttl, now: time.Now}
}

// NewCLICatalog offers the CLI's family aliases, which the CLI resolves to its
// newest model; it has no command to list exact versions.
func NewCLICatalog() *Catalog { return &Catalog{passthrough: true, now: time.Now} }

// current returns the models, refreshing them when stale. A failed listing
// keeps the previous list, or the built-in one, and is retried after the TTL.
func (c *Catalog) current() []ModelInfo {
	if c == nil || c.list == nil {
		return builtinModels
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.models != nil && c.now().Sub(c.fetched) < c.ttl {
		return c.models
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ms, err := c.list(ctx)
	c.fetched = c.now()
	if err != nil || len(ms) == 0 {
		if c.models == nil {
			c.models = builtinModels
		}
		return c.models
	}
	var claude []ModelInfo
	for _, m := range ms {
		if strings.HasPrefix(m.ID, "claude-") {
			claude = append(claude, m)
		}
	}
	sort.SliceStable(claude, func(i, j int) bool { return claude[i].CreatedAt.After(claude[j].CreatedAt) })
	c.models = claude
	return c.models
}

// Options lists the selector entries: Default, one "Latest <family>" per
// family the account has, then (API only) every exact model, newest first.
func (c *Catalog) Options() []ModelOption {
	opts := []ModelOption{{ID: "", Label: "Default", Note: "the configured default for this step"}}
	if c != nil && c.passthrough {
		for _, f := range families {
			opts = append(opts, ModelOption{ID: f, Label: "Latest " + titleCase(f), Note: familyNotes[f] + "; newest release, picked by the Claude CLI"})
		}
		return opts
	}
	ms := c.current()
	for _, f := range families {
		if id := newestOf(ms, f); id != "" {
			opts = append(opts, ModelOption{ID: f, Label: "Latest " + titleCase(f) + " (" + id + ")", Note: familyNotes[f] + "; moves to new releases automatically"})
		}
	}
	for _, m := range ms {
		label := m.DisplayName
		if label == "" {
			label = m.ID
		}
		opts = append(opts, ModelOption{ID: m.ID, Label: label + " (pinned)", Note: "stays on " + m.ID})
	}
	return opts
}

// Resolve turns a "Latest" family choice into the model id to send. Exact ids
// and "" pass through unchanged, as does everything on a client without a
// catalog (it has no list to resolve against).
func (c *Catalog) Resolve(id string) string {
	if c == nil || c.passthrough || !isFamily(id) {
		return id
	}
	if m := newestOf(c.current(), id); m != "" {
		return m
	}
	return id
}

// Known reports whether id may be saved as a choice.
func (c *Catalog) Known(id string) bool {
	for _, o := range c.Options() {
		if o.ID == id {
			return true
		}
	}
	return false
}

func newestOf(ms []ModelInfo, family string) string {
	var best ModelInfo
	for _, m := range ms {
		if familyOf(m.ID) == family && (best.ID == "" || m.CreatedAt.After(best.CreatedAt)) {
			best = m
		}
	}
	return best.ID
}

func titleCase(s string) string { return strings.ToUpper(s[:1]) + s[1:] }

// ListModels pages through GET /v1/models.
func (b *APIBackend) ListModels(ctx context.Context) ([]ModelInfo, error) {
	var all []ModelInfo
	after := ""
	for page := 0; page < 20; page++ {
		q := url.Values{"limit": {"1000"}}
		if after != "" {
			q.Set("after_id", after)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.baseURL()+"/v1/models?"+q.Encode(), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("x-api-key", b.APIKey)
		req.Header.Set("anthropic-version", apiVersion)
		resp, err := b.httpClient().Do(req)
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("models api: HTTP %d: %s", resp.StatusCode, truncate(string(data), 200))
		}
		var out struct {
			Data    []ModelInfo `json:"data"`
			HasMore bool        `json:"has_more"`
			LastID  string      `json:"last_id"`
		}
		if err := json.Unmarshal(data, &out); err != nil {
			return nil, fmt.Errorf("models api: decode: %w", err)
		}
		all = append(all, out.Data...)
		if !out.HasMore || out.LastID == "" {
			return all, nil
		}
		after = out.LastID
	}
	return all, nil
}
