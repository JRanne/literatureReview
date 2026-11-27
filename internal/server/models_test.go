package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"literaturereview/internal/ai"
	"literaturereview/internal/config"
	"literaturereview/internal/model"
	"literaturereview/internal/store"
)

// routedBackend answers like fakeBackend and records which model answered
// each kind of prompt, so per-step routing is observable end to end.
type routedBackend struct {
	inner *fakeBackend
	model string
	mu    *sync.Mutex
	used  map[string]map[string]bool // prompt kind -> models
}

func newRoutedBackend() *routedBackend {
	return &routedBackend{
		inner: &fakeBackend{calls: map[string]int{}, screens: map[string]int{"A": 90, "B": 50, "C": 10}},
		model: "base-model",
		mu:    &sync.Mutex{},
		used:  map[string]map[string]bool{},
	}
}

func (b *routedBackend) Name() string       { return "fake" }
func (b *routedBackend) ModelLabel() string { return b.model }
func (b *routedBackend) WithModel(m string) ai.Backend {
	if m == "" {
		return b
	}
	c := *b
	c.model = m
	return &c
}

func (b *routedBackend) Complete(ctx context.Context, system, user string, n int) (string, error) {
	// fakeBackend's call counter is not safe for the concurrent screening
	// batches; the lock also keeps the before/after diff attributable.
	b.mu.Lock()
	defer b.mu.Unlock()
	before := map[string]int{}
	for k, v := range b.inner.calls {
		before[k] = v
	}
	out, err := b.inner.Complete(ctx, system, user, n)
	for k, v := range b.inner.calls {
		if v != before[k] {
			if b.used[k] == nil {
				b.used[k] = map[string]bool{}
			}
			b.used[k][b.model] = true
		}
	}
	return out, err
}

func (b *routedBackend) models(kind string) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for m := range b.used[kind] {
		out = append(out, m)
	}
	return out
}

func newModelsServer(t *testing.T, defaults ai.Models) (*Server, *store.Store, *routedBackend, *httptest.Server) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.FromEnv()
	cfg.AIBackend, cfg.AnthropicAPIKey, cfg.BrowserFetch = "api", "", "off"
	cfg.StepModels = defaults
	srv, err := New(cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	rb := newRoutedBackend()
	srv.ai = ai.New(rb)
	srv.reg.Add(&fakeDB{papers: map[string][]*model.Paper{
		"genai adoption public sector": {paper("A", "10.ok/a", "Alpha"), paper("B", "10.x/b", "Beta")},
		"llm governance government":    {paper("C", "10.x/c", "Gamma")},
	}})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return srv, st, rb, ts
}

var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func postForm(t *testing.T, ts *httptest.Server, path string, v url.Values) *http.Response {
	t.Helper()
	resp, err := noRedirect.PostForm(ts.URL+path, v)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

func assertModels(t *testing.T, rb *routedBackend, kind string, want ...string) {
	t.Helper()
	got := rb.models(kind)
	if len(got) != len(want) {
		t.Fatalf("%s answered by %v, want %v", kind, got, want)
	}
	for _, w := range want {
		found := false
		for _, g := range got {
			found = found || g == w
		}
		if !found {
			t.Fatalf("%s answered by %v, want %v", kind, got, want)
		}
	}
}

func TestEachStepUsesItsOwnModel(t *testing.T) {
	_, st, rb, ts := newModelsServer(t, ai.Models{ai.StepQueries: "env-queries", ai.StepSynthesis: "env-synth"})

	resp := postForm(t, ts, "/projects", url.Values{"question": {"Q"}, "sources": {"fakedb"}})
	id := strings.Split(strings.TrimPrefix(resp.Header.Get("Location"), "/projects/"), "?")[0]

	// The project picks a cheap screening model; the other steps keep the
	// configured defaults (and adjudication falls back to the synthesis one).
	resp = postForm(t, ts, "/projects/"+id+"/models", url.Values{"model_screening": {"claude-haiku-5-5"}})
	if loc := resp.Header.Get("Location"); strings.Contains(loc, "error=") {
		t.Fatalf("saving models failed: %s", loc)
	}
	var p model.Project
	_ = st.View(id, func(pp *model.Project) { p = *pp })
	if p.StepModels["screening"] != "claude-haiku-5-5" {
		t.Fatalf("project step models = %v", p.StepModels)
	}

	postForm(t, ts, "/projects/"+id+"/search", url.Values{"mode": {"generate"}})
	if p := waitIdle(t, st, id); p.Job.Error != "" {
		t.Fatal(p.Job.Error)
	}
	assertModels(t, rb, "queries", "env-queries")
	assertModels(t, rb, "screen", "claude-haiku-5-5")

	postForm(t, ts, "/projects/"+id+"/adjudicate", url.Values{"standard": {"lenient"}})
	if p := waitIdle(t, st, id); p.Job.Error != "" {
		t.Fatal(p.Job.Error)
	}
	assertModels(t, rb, "adjudicate", "env-synth")

	postForm(t, ts, "/projects/"+id+"/extract", nil)
	if p := waitIdle(t, st, id); p.Job.Error != "" {
		t.Fatal(p.Job.Error)
	}
	assertModels(t, rb, "extract", "base-model")

	// The results page's per-draft picker writes the same synthesis setting.
	postForm(t, ts, "/projects/"+id+"/synthesize", url.Values{"model": {"claude-opus-5-5"}})
	p2 := waitIdle(t, st, id)
	if p2.Job.Error != "" {
		t.Fatal(p2.Job.Error)
	}
	assertModels(t, rb, "synthesis", "claude-opus-5-5")
	if p2.StepModels["synthesis"] != "claude-opus-5-5" || p2.SynthesisModel != "claude-opus-5-5" {
		t.Fatalf("synthesis choice %q, draft model %q", p2.StepModels["synthesis"], p2.SynthesisModel)
	}
}

func TestModelsFormRejectsUnknownModel(t *testing.T) {
	_, st, _, ts := newModelsServer(t, nil)
	resp := postForm(t, ts, "/projects", url.Values{"question": {"Q"}, "sources": {"fakedb"}})
	id := strings.Split(strings.TrimPrefix(resp.Header.Get("Location"), "/projects/"), "?")[0]

	resp = postForm(t, ts, "/projects/"+id+"/models", url.Values{"model_screening": {"gpt-4"}})
	if !strings.Contains(resp.Header.Get("Location"), "error=") {
		t.Fatalf("unknown model accepted: %s", resp.Header.Get("Location"))
	}
	var p model.Project
	_ = st.View(id, func(pp *model.Project) { p = *pp })
	if len(p.StepModels) != 0 {
		t.Fatalf("rejected form still saved %v", p.StepModels)
	}
}

func TestLegacySynthesisChoiceStillApplies(t *testing.T) {
	_, st, rb, ts := newModelsServer(t, nil)
	_ = st.Put(&model.Project{ID: "old", Title: "old", Question: "Q", Sources: []string{"fakedb"}, SynthesisChoice: "claude-opus-5-5",
		Papers: []*model.Paper{{ID: "A", Title: "Alpha", Decision: model.DecisionKeep, Year: 2024, Authors: []string{"Ann Author"}}}})
	postForm(t, ts, "/projects/old/synthesize", url.Values{"model": {""}})
	if p := waitIdle(t, st, "old"); p.Job.Error != "" {
		t.Fatal(p.Job.Error)
	}
	// An explicit "Default" pick clears the legacy choice.
	assertModels(t, rb, "synthesis", "base-model")
}

func TestSetupPageListsEveryStep(t *testing.T) {
	_, _, _, ts := newModelsServer(t, ai.Models{ai.StepScreening: "env-screen"})
	resp := postForm(t, ts, "/projects", url.Values{"question": {"Q"}, "sources": {"fakedb"}})
	loc := strings.Split(resp.Header.Get("Location"), "?")[0]
	r, err := http.Get(ts.URL + loc + "?view=setup")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	html := string(b)
	for _, s := range ai.AllSteps {
		if !strings.Contains(html, `name="model_`+string(s.Step)+`"`) {
			t.Errorf("setup page lacks a selector for %s", s.Step)
		}
	}
	if !strings.Contains(html, "env-screen") {
		t.Error("setup page does not show the configured default for screening")
	}
}

func TestNewReviewFormChoosesModels(t *testing.T) {
	_, st, _, ts := newModelsServer(t, ai.Models{ai.StepScreening: "env-screen"})
	r, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r.Body)
	r.Body.Close()
	for _, s := range ai.AllSteps {
		if !strings.Contains(string(b), `name="model_`+string(s.Step)+`"`) {
			t.Errorf("new review form lacks a selector for %s", s.Step)
		}
	}

	resp := postForm(t, ts, "/projects", url.Values{"question": {"Q"}, "sources": {"fakedb"}, "model_screening": {"haiku"}, "model_synthesis": {"claude-opus-5-5"}, "model_queries": {""}})
	id := strings.Split(strings.TrimPrefix(resp.Header.Get("Location"), "/projects/"), "?")[0]
	var p model.Project
	_ = st.View(id, func(pp *model.Project) { p = *pp })
	if p.StepModels["screening"] != "haiku" || p.StepModels["synthesis"] != "claude-opus-5-5" || len(p.StepModels) != 2 {
		t.Fatalf("created project models = %v", p.StepModels)
	}

	resp = postForm(t, ts, "/projects", url.Values{"question": {"Q"}, "sources": {"fakedb"}, "model_screening": {"gpt-4"}})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown model on create: status %d", resp.StatusCode)
	}
}

func TestSelectorsFollowTheLiveModelList(t *testing.T) {
	srv, _, _, ts := newModelsServer(t, nil)
	released := []ai.ModelInfo{{ID: "claude-opus-5-5", DisplayName: "Claude Opus 5.5"}}
	srv.ai = srv.ai.WithCatalog(ai.NewAPICatalog(func(context.Context) ([]ai.ModelInfo, error) { return released, nil }, 0))
	page := func() string {
		r, err := http.Get(ts.URL + "/")
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		return string(b)
	}
	if strings.Contains(page(), "claude-opus-6") {
		t.Fatal("unreleased model offered")
	}
	released = append(released, ai.ModelInfo{ID: "claude-opus-6", DisplayName: "Claude Opus 6", CreatedAt: time.Now()})
	html := page()
	if !strings.Contains(html, `value="claude-opus-6"`) || !strings.Contains(html, "Latest Opus (claude-opus-6)") {
		t.Fatal("a newly released model did not appear in the selectors")
	}
}

func TestSavedModelOutsideTheListIsKept(t *testing.T) {
	srv, st, _, ts := newModelsServer(t, nil)
	srv.ai = srv.ai.WithCatalog(ai.NewCLICatalog()) // offers only the Latest aliases
	_ = st.Put(&model.Project{ID: "pin", Title: "pin", Question: "Q", Sources: []string{"fakedb"}, StepModels: map[string]string{"synthesis": "claude-opus-5"}})

	r, err := http.Get(ts.URL + "/projects/pin?view=setup")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if !strings.Contains(string(b), `value="claude-opus-5" selected`) {
		t.Fatal("the saved model is not shown as selected")
	}
	resp := postForm(t, ts, "/projects/pin/models", url.Values{"model_synthesis": {"claude-opus-5"}, "model_screening": {"haiku"}})
	if strings.Contains(resp.Header.Get("Location"), "error=") {
		t.Fatalf("re-saving the form rejected the saved model: %s", resp.Header.Get("Location"))
	}
	// The results page's per-draft picker must keep it too.
	_ = st.Update("pin", func(p *model.Project) error {
		p.Papers = []*model.Paper{{ID: "A", Title: "Alpha", Decision: model.DecisionKeep, Year: 2024, Authors: []string{"Ann Author"}}}
		p.Stage = model.StageDone // the results view is shown only from this stage
		return nil
	})
	r, err = http.Get(ts.URL + "/projects/pin?view=results")
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(r.Body)
	r.Body.Close()
	if !strings.Contains(string(b), `<select name="model"`) || !strings.Contains(string(b), `value="claude-opus-5" selected`) {
		t.Fatalf("the synthesis picker drops the saved model:\n%s", grepLines(string(b), "<select name=\"model\""))
	}
	resp = postForm(t, ts, "/projects/pin/synthesize", url.Values{"model": {"claude-opus-5"}})
	if strings.Contains(resp.Header.Get("Location"), "error=") {
		t.Fatalf("regenerating with the saved model was rejected: %s", resp.Header.Get("Location"))
	}
	waitIdle(t, st, "pin")
}

func grepLines(s, sub string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		for _, p := range strings.Split(sub, "|") {
			if strings.Contains(l, p) {
				out = append(out, strings.TrimSpace(l))
				break
			}
		}
	}
	return strings.Join(out, "\n")
}
