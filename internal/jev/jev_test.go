package jev

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"literaturereview/internal/model"
)

func TestCombine(t *testing.T) {
	cases := []struct {
		rel, inc, exc float64
		want          int
		verdict       model.Verdict
	}{
		{4, 1, 0, 100, model.VerdictKeep},
		{3.73, 0.78, 0.1, 86, model.VerdictKeep},
		{2, 0.5, 0, 50, model.VerdictUnsure},
		{0.5, 0.1, 0, 11, model.VerdictDrop},
		{4, 1, 0.9, 39, model.VerdictDrop}, // exclusion criterion matched → capped
	}
	for _, c := range cases {
		got, v := Combine(c.rel, c.inc, c.exc)
		if got != c.want || v != c.verdict {
			t.Errorf("Combine(%v,%v,%v) = %d %s, want %d %s", c.rel, c.inc, c.exc, got, v, c.want, c.verdict)
		}
	}
}

func TestAssessRequestAndParse(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if r.Header.Get("Authorization") != "Bearer k1" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("headers: %v", r.Header)
		}
		var req request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if req.Model != "jev-latest" || len(req.Questions) != 3 || req.Questions["relevance"].Type != "score" || req.Questions["exclude"].Type != "noul" {
			t.Errorf("request shape: %+v", req)
		}
		paper := req.State["paper"].(map[string]any)
		if paper["title"] != "GenAI in agencies" || req.State["research_question"] != "RQ" {
			t.Errorf("state: %+v", req.State)
		}
		if n == 1 { // first call rate limited → client must retry
			w.WriteHeader(429)
			w.Write([]byte(`{"message":"slow down"}`))
			return
		}
		w.Write([]byte(`{"model":"jev-1.13.0","answers":{"relevance":{"type":"score","score":3.2,"confidence":0.7,"probabilities":{"3":0.8,"4":0.2}},"include":{"type":"noul","noul":0.8},"exclude":{"type":"noul","noul":0.05}},"usage":{"input_tokens":1}}`))
	}))
	defer srv.Close()
	c := New("k1", "", srv.URL)
	c.HTTP = srv.Client()
	p := &model.Project{Question: "RQ", Inclusion: "empirical", Exclusion: "medical"}
	pp := &model.Paper{ID: "a", Title: "GenAI in agencies", Abstract: "abs"}
	a := c.Assess(context.Background(), p, pp)
	if a.Error != "" {
		t.Fatalf("error: %s", a.Error)
	}
	if a.Model != "jev-1.13.0" || a.Relevance != 3.2 || a.Include != 0.8 || a.Exclude != 0.05 || a.Confidence != 0.7 {
		t.Fatalf("parsed: %+v", a)
	}
	if a.Score != 80 || a.Verdict != model.VerdictKeep {
		t.Fatalf("score: %+v", a)
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("expected retry after 429, calls=%d", calls)
	}
}

func TestAssessAuthError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`{"message":"bad key"}`))
	}))
	defer srv.Close()
	c := New("bad", "", srv.URL)
	a := c.Assess(context.Background(), &model.Project{Question: "q"}, &model.Paper{Title: "t"})
	if !strings.Contains(a.Error, "401") || a.Verdict != "" {
		t.Fatalf("expected auth error, got %+v", a)
	}
}

func TestAssessAll(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"answers":{"relevance":{"score":1,"confidence":0.9},"include":{"noul":0.2}}}`))
	}))
	defer srv.Close()
	c := New("k", "", srv.URL)
	papers := []*model.Paper{{ID: "1", Title: "a"}, {ID: "2", Title: "b"}, {ID: "3", Title: "c"}}
	var last int
	if err := c.AssessAll(context.Background(), &model.Project{Question: "q"}, papers, func(d, _ int) { last = d }); err != nil {
		t.Fatal(err)
	}
	for _, p := range papers {
		if p.Jev == nil || p.Jev.Verdict != model.VerdictDrop || p.Jev.Score != 23 {
			t.Fatalf("paper %s: %+v", p.ID, p.Jev)
		}
	}
	if last != 3 {
		t.Fatalf("progress %d", last)
	}
}
