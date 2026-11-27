package ai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func day(n int) time.Time { return time.Date(2026, 1, n, 0, 0, 0, 0, time.UTC) }

func optionIDs(opts []ModelOption) []string {
	var ids []string
	for _, o := range opts {
		ids = append(ids, o.ID)
	}
	return ids
}

func TestAPIBackendListsModelsAcrossPages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.Header.Get("x-api-key") != "k" || r.Header.Get("anthropic-version") == "" {
			http.Error(w, "bad request", 400)
			return
		}
		page := map[string]any{"has_more": true, "last_id": "claude-sonnet-5-5",
			"data": []map[string]string{{"id": "claude-opus-5-5", "display_name": "Claude Opus 5.5", "created_at": "2026-09-01T00:00:00Z"}, {"id": "claude-sonnet-5-5", "display_name": "Claude Sonnet 5.5", "created_at": "2026-08-01T00:00:00Z"}}}
		if r.URL.Query().Get("after_id") == "claude-sonnet-5-5" {
			page = map[string]any{"has_more": false, "data": []map[string]string{{"id": "claude-haiku-5-5", "display_name": "Claude Haiku 5.5", "created_at": "2026-07-01T00:00:00Z"}}}
		}
		_ = json.NewEncoder(w).Encode(page)
	}))
	defer srv.Close()
	b := &APIBackend{APIKey: "k", BaseURL: srv.URL, HTTP: srv.Client()}
	ms, err := b.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 3 || ms[2].ID != "claude-haiku-5-5" || ms[0].DisplayName != "Claude Opus 5.5" || !ms[0].CreatedAt.Equal(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("models = %+v", ms)
	}
}

func TestAPICatalogOffersLatestAndExactModels(t *testing.T) {
	cat := NewAPICatalog(func(context.Context) ([]ModelInfo, error) {
		return []ModelInfo{
			{ID: "claude-opus-5", DisplayName: "Claude Opus 5", CreatedAt: day(1)},
			{ID: "claude-opus-5-5", DisplayName: "Claude Opus 5.5", CreatedAt: day(9)},
			{ID: "claude-haiku-5-5", DisplayName: "Claude Haiku 5.5", CreatedAt: day(5)},
		}, nil
	}, time.Hour)
	ids := strings.Join(optionIDs(cat.Options()), ",")
	if !strings.HasPrefix(ids, ",haiku,") || !strings.Contains(ids, "opus") || !strings.Contains(ids, "claude-opus-5-5,claude-haiku-5-5,claude-opus-5") {
		t.Fatalf("options = %s (want Default, then the latest-of-family choices, then exact ids newest first)", ids)
	}
	if strings.Contains(ids, "sonnet") {
		t.Errorf("offers a family the account has no model in: %s", ids)
	}
	if got := cat.Resolve("opus"); got != "claude-opus-5-5" {
		t.Errorf("Resolve(opus) = %q, want the newest Opus", got)
	}
	if got := cat.Resolve("claude-opus-5"); got != "claude-opus-5" {
		t.Errorf("an exact id must stay pinned, got %q", got)
	}
	if !cat.Known("opus") || !cat.Known("claude-opus-5") || !cat.Known("") || cat.Known("gpt-4") {
		t.Error("Known disagrees with the listed models")
	}
}

func TestAPICatalogPicksUpNewReleases(t *testing.T) {
	models := []ModelInfo{{ID: "claude-opus-5-5", CreatedAt: day(1)}}
	calls := 0
	now := day(1)
	cat := NewAPICatalog(func(context.Context) ([]ModelInfo, error) { calls++; return models, nil }, time.Hour)
	cat.now = func() time.Time { return now }

	if got := cat.Resolve("opus"); got != "claude-opus-5-5" {
		t.Fatalf("Resolve(opus) = %q", got)
	}
	models = append(models, ModelInfo{ID: "claude-opus-6", DisplayName: "Claude Opus 6", CreatedAt: day(20)})
	if got := cat.Resolve("opus"); got != "claude-opus-5-5" || calls != 1 {
		t.Fatalf("refreshed before the TTL: %q after %d calls", got, calls)
	}
	now = now.Add(2 * time.Hour)
	if got := cat.Resolve("opus"); got != "claude-opus-6" {
		t.Fatalf("after the TTL Resolve(opus) = %q, want the new release", got)
	}
	if !cat.Known("claude-opus-6") {
		t.Fatal("new release not offered")
	}
}

func TestAPICatalogFallsBackWhenListingFails(t *testing.T) {
	cat := NewAPICatalog(func(context.Context) ([]ModelInfo, error) { return nil, errors.New("offline") }, time.Hour)
	if got := cat.Resolve("haiku"); !strings.HasPrefix(got, "claude-haiku-") {
		t.Fatalf("offline Resolve(haiku) = %q, want a built-in Haiku id", got)
	}
	if !cat.Known("sonnet") || len(cat.Options()) < 5 {
		t.Fatalf("offline options = %v", optionIDs(cat.Options()))
	}
}

func TestCLICatalogUsesAliases(t *testing.T) {
	cat := NewCLICatalog()
	ids := strings.Join(optionIDs(cat.Options()), ",")
	if ids != ",haiku,sonnet,opus,fable" {
		t.Fatalf("CLI options = %s", ids)
	}
	// The CLI resolves its own aliases to the newest model, so they pass
	// through unchanged and track releases without any listing.
	if cat.Resolve("opus") != "opus" || !cat.Known("fable") || cat.Known("gpt-4") {
		t.Fatal("CLI catalog must pass aliases through")
	}
}

func TestClientResolvesAliasPerStep(t *testing.T) {
	var seen []string
	cat := NewAPICatalog(func(context.Context) ([]ModelInfo, error) {
		return []ModelInfo{{ID: "claude-haiku-5-5", CreatedAt: day(1)}, {ID: "claude-haiku-6", CreatedAt: day(30)}}, nil
	}, time.Hour)
	c := New(&modelBackend{model: "base", seen: &seen}).WithCatalog(cat)
	if got := c.ForStep(StepScreening, Models{StepScreening: "haiku"}, nil).ModelLabel(); got != "claude-haiku-6" {
		t.Fatalf("screening on Latest Haiku answered by %q", got)
	}
	if got := c.ForStep(StepQueries, nil, nil).ModelLabel(); got != "base" {
		t.Fatalf("unset step = %q, want the base model", got)
	}
}
