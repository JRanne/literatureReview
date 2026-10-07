package server

// End-to-end test of the whole workflow with a fake Claude backend, a fake
// academic database and a fake PDF fetcher. No network access is needed.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"literaturereview/internal/ai"
	"literaturereview/internal/config"
	"literaturereview/internal/fetch"
	"literaturereview/internal/jev"
	"literaturereview/internal/model"
	"literaturereview/internal/sources"
	"literaturereview/internal/store"
)

// fakeBackend answers the three prompt shapes the app sends.
type fakeBackend struct {
	calls   map[string]int
	screens map[string]int // id -> score to return
}

var idRe = regexp.MustCompile(`\[id=([^\]]+)\]`)

func (f *fakeBackend) Name() string                { return "fake" }
func (f *fakeBackend) ModelLabel() string          { return "fake-model" }
func (f *fakeBackend) WithModel(string) ai.Backend { return f }
func (f *fakeBackend) Complete(_ context.Context, system, user string, _ int) (string, error) {
	switch {
	case strings.Contains(user, "complementary search queries"):
		f.calls["queries"]++
		return `[{"source":"fakedb","query":"genai adoption public sector","rationale":"broad"},{"source":"fakedb","query":"llm governance government","rationale":"narrow"},{"source":"ignored","query":"x"}]`, nil
	case strings.Contains(user, "Design 2 to 3 NEW queries"):
		f.calls["expand"]++
		if !strings.Contains(user, "Best-ranked papers") || !strings.Contains(user, "relevant") {
			return "", fmt.Errorf("expand prompt lacks yield context")
		}
		return `[{"source":"fakedb","query":"expanded query","rationale":"broader"},{"source":"fakedb","query":"genai adoption public sector","rationale":"dup must be filtered"}]`, nil
	case strings.Contains(user, "Propose between 8 and 14 concepts"):
		f.calls["propose"]++
		return `[{"name":"Governance","definition":"g"},{"name":"Adoption","definition":"a"},{"name":"Trust","definition":"t"},{"name":"Governance","definition":"dup"}]`, nil
	case strings.Contains(user, "filling a concept matrix") || strings.Contains(user, "one object per paper:\n{\"id\": \"<id>\", \"concepts\""):
		f.calls["assign"]++
		var out []map[string]any
		for _, m := range idRe.FindAllStringSubmatch(user, -1) {
			out = append(out, map[string]any{"id": m[1], "concepts": []map[string]string{{"name": "governance", "note": "sets rules"}, {"name": "Unknown", "note": "x"}}})
		}
		b, _ := json.Marshal(out)
		return string(b), nil
	case strings.Contains(user, "Undecided papers with the earlier screeners"):
		f.calls["adjudicate"]++
		var out []map[string]any
		for _, m := range idRe.FindAllStringSubmatch(user, -1) {
			id := m[1]
			dec, conf := "keep", 0.9
			if id == "B" {
				dec, conf = "drop", 0.8
			}
			if id == "U" {
				dec, conf = "keep", 0.3 // too low to apply
			}
			out = append(out, map[string]any{"id": id, "decision": dec, "confidence": conf, "reason": "adjudicated " + id})
		}
		b, _ := json.Marshal(out)
		return string(b), nil
	case strings.Contains(user, "Candidate papers:"):
		f.calls["screen"]++
		var out []ai.Screening
		for _, m := range idRe.FindAllStringSubmatch(user, -1) {
			id := m[1]
			score := f.screens[id]
			v := "unsure"
			if score >= 70 {
				v = "keep"
			} else if score < 40 {
				v = "drop"
			}
			out = append(out, ai.Screening{ID: id, Score: score, Verdict: v, Rationale: "because " + id})
		}
		b, _ := json.Marshal(out)
		return "```json\n" + string(b) + "\n```", nil
	case strings.Contains(user, "For EACH paper respond"):
		f.calls["extract"]++
		var rows []map[string]any
		for _, m := range idRe.FindAllStringSubmatch(user, -1) {
			rows = append(rows, map[string]any{"id": m[1], "objective": "obj " + m[1], "method": "survey", "context": "Finland", "findings": "f", "theory": "TAM", "contribution": "c", "limitations": "l", "relevance": "r", "keywords": []string{"k"}})
		}
		b, _ := json.Marshal(rows)
		return string(b), nil
	case strings.Contains(user, "Write the Background section"):
		f.calls["synthesis"]++
		keys := regexp.MustCompile(`\[@([^\]]+)\]`).FindAllStringSubmatch(user, -1)
		var sb strings.Builder
		sb.WriteString("## Background\n\nSynthesised from the kept papers")
		for _, k := range keys {
			sb.WriteString(" [@" + k[1] + "]")
		}
		sb.WriteString(" and a bogus one [@nokey2020x].\n\n### Themes\n\nText.")
		return sb.String(), nil
	}
	return "", fmt.Errorf("unexpected prompt: %s", user[:60])
}

// fakeDB is a Source whose results depend on the query.
type fakeDB struct {
	papers map[string][]*model.Paper
	seen   []string
}

func (f *fakeDB) Name() string   { return "fakedb" }
func (f *fakeDB) Label() string  { return "FakeDB" }
func (f *fakeDB) Syntax() string { return "anything" }
func (f *fakeDB) Search(_ context.Context, q string, _ sources.Options) (*sources.Result, error) {
	f.seen = append(f.seen, q)
	if q == "boom" {
		return &sources.Result{URL: "https://fake.db/search?q=boom"}, fmt.Errorf("HTTP 500 from fake")
	}
	var out []*model.Paper
	for _, p := range f.papers[q] {
		c := *p
		out = append(out, &c)
	}
	return &sources.Result{Papers: out, URL: "https://fake.db/search?q=" + url.QueryEscape(q), Total: 1000 + len(out)}, nil
}

// Snowball makes fakeDB a Snowballer: every seed "references" R and is "cited by" F.
func (f *fakeDB) Snowball(_ context.Context, seeds []*model.Paper, _ sources.Options) ([]*model.Paper, []*model.Paper, error) {
	f.seen = append(f.seen, "snowball")
	if len(seeds) == 0 {
		return nil, nil, fmt.Errorf("no seeds")
	}
	r := paper("R", "10.ok/r", "Rho referenced classic")
	r.FoundIn = []string{"fakedb (reference)"}
	fw := paper("F", "10.x/f", "Phi forward citing paper")
	fw.FoundIn = []string{"fakedb (citation)"}
	return []*model.Paper{r}, []*model.Paper{fw}, nil
}

// fakeFetcher returns a PDF for papers whose DOI starts with "10.ok".
type fakeFetcher struct{}

func (fakeFetcher) Name() string { return "fake OA" }
func (fakeFetcher) Fetch(_ context.Context, p *model.Paper) (*fetch.Result, error) {
	if strings.HasPrefix(p.DOI, "10.ok") {
		return &fetch.Result{URL: "https://fake.pdf/" + p.DOI, Data: []byte("%PDF-1.4\nfake\n")}, nil
	}
	return nil, fmt.Errorf("HTTP 403 paywalled")
}

func german() *model.Paper {
	p := paper("G", "10.x/g", "Künstliche Intelligenz in der öffentlichen Verwaltung")
	p.Language = "de"
	return p
}

func paper(id, doi, title string) *model.Paper {
	return &model.Paper{ID: id, DOI: doi, Title: title, Authors: []string{"Ann Author"}, Year: 2024, Venue: "J", Abstract: "Abstract of " + title}
}

func waitIdle(t *testing.T, st *store.Store, id string) *model.Project {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var running bool
		var snap model.Project
		_ = st.View(id, func(p *model.Project) { running = p.Job != nil && p.Job.Running; snap = *p })
		if !running {
			return &snap
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("job did not finish")
	return nil
}

func TestEndToEndWorkflow(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.FromEnv()
	cfg.AIBackend = "api"
	cfg.AnthropicAPIKey = ""
	srv, err := New(cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	fb := &fakeBackend{calls: map[string]int{}, screens: map[string]int{"A": 90, "B": 50, "C": 10, "D": 80}}
	srv.ai = ai.New(fb)
	db := &fakeDB{papers: map[string][]*model.Paper{
		"genai adoption public sector": {paper("A", "10.ok/a", "Alpha adoption of GenAI in agencies"), paper("B", "10.x/b", "Beta borderline study on AI")},
		"llm governance government":    {paper("C", "10.x/c", "Gamma unrelated chemistry"), paper("A", "10.ok/a", "Alpha adoption of GenAI in agencies")},
		"ti:extra":                     {paper("D", "10.ok/d", "Delta new paper found by hand-written query")},
	}}
	srv.reg.Add(db)
	srv.chain = &fetch.Chain{Fetchers: []fetch.Fetcher{fakeFetcher{}}}

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	post := func(path string, v url.Values) *http.Response {
		t.Helper()
		resp, err := client.PostForm(ts.URL+path, v)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	get := func(path string) string {
		t.Helper()
		resp, err := client.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			t.Fatalf("GET %s: %d %s", path, resp.StatusCode, b)
		}
		return string(b)
	}

	// 1. Create & start: AI generates queries, fake DB answers, AI screens.
	resp := post("/projects", url.Values{"question": {"How do agencies adopt GenAI?"}, "sources": {"fakedb"}, "start": {"1"}})
	id := strings.TrimPrefix(resp.Header.Get("Location"), "/projects/")
	id = strings.Split(id, "?")[0]
	p := waitIdle(t, st, id)
	if p.Job.Error != "" {
		t.Fatalf("search failed: %s\n%s", p.Job.Error, strings.Join(p.Job.Log, "\n"))
	}
	if p.Stage != model.StageScreening || len(p.Papers) != 3 || len(p.Queries) != 2 {
		t.Fatalf("after search: stage=%s papers=%d queries=%d", p.Stage, len(p.Papers), len(p.Queries))
	}
	// The ignored-source query was filtered; each query records URL and totals.
	for _, q := range p.Queries {
		if q.Source != "fakedb" || !strings.HasPrefix(q.URL, "https://fake.db/search?q=") || q.Total < 1000 || q.Results == 0 {
			t.Fatalf("query record incomplete: %+v", q)
		}
	}
	// Dedupe by DOI: A came from both queries → one paper found twice.
	if a := p.Paper("A"); a == nil || a.Decision != model.DecisionKeep || a.DecisionBy != "ai" || a.AIScore != 90 {
		t.Fatalf("paper A: %+v", p.Paper("A"))
	}
	if c := p.Paper("C"); c.Decision != model.DecisionDrop {
		t.Fatalf("paper C should be dropped by AI: %+v", c)
	}
	if b := p.Paper("B"); b.Decision != model.DecisionUndecided {
		t.Fatalf("paper B should be undecided: %+v", b)
	}
	if !strings.Contains(get("/projects/"+id+"?view=search"), "fake.db/search?q=genai") {
		t.Fatal("search view does not show the real request URL")
	}
	if !strings.Contains(get("/projects/"+id+"?view=screening"), "Alpha adoption") {
		t.Fatal("screening view missing papers")
	}

	// 2. User overrides: keep B by hand, drop A by hand.
	post("/projects/"+id+"/papers/B/decision", url.Values{"decision": {"keep"}})
	post("/projects/"+id+"/papers/A/decision", url.Values{"decision": {"drop"}})

	// 3. Hand-edit queries (add one, break one) and re-run: new paper D is
	//    added and screened; user decisions on A/B survive; broken query logs an error.
	post("/projects/"+id+"/queries", url.Values{"q_fakedb": {"genai adoption public sector\nti:extra\nboom"}, "run": {"1"}})
	p = waitIdle(t, st, id)
	if p.Job.Error != "" {
		t.Fatalf("re-run failed: %s", p.Job.Error)
	}
	if len(p.Papers) != 4 || p.Paper("D") == nil || p.Paper("D").Decision != model.DecisionKeep {
		t.Fatalf("incremental search: papers=%d D=%+v", len(p.Papers), p.Paper("D"))
	}
	if p.Paper("B").Decision != model.DecisionKeep || p.Paper("B").DecisionBy != "user" || p.Paper("A").Decision != model.DecisionDrop {
		t.Fatalf("user decisions lost on re-run: A=%+v B=%+v", p.Paper("A"), p.Paper("B"))
	}
	var boom *model.Query
	for i := range p.Queries {
		if p.Queries[i].Query == "boom" {
			boom = &p.Queries[i]
		}
	}
	if boom == nil || boom.Error == "" || boom.URL == "" {
		t.Fatalf("failed query not recorded with URL and error: %+v", boom)
	}
	if fb.calls["queries"] != 1 {
		t.Fatalf("re-run must not regenerate queries (calls=%d)", fb.calls["queries"])
	}

	// 4. Re-screen with changed AI opinion: C now relevant. User decisions stay.
	fb.screens["C"] = 85
	post("/projects/"+id+"/search", url.Values{"mode": {"rescreen"}})
	p = waitIdle(t, st, id)
	if p.Job.Error != "" {
		t.Fatalf("rescreen failed: %s", p.Job.Error)
	}
	if p.Paper("C").Decision != model.DecisionKeep || p.Paper("C").AIScore != 85 {
		t.Fatalf("rescreen did not update AI decision: %+v", p.Paper("C"))
	}
	if p.Paper("A").Decision != model.DecisionDrop || p.Paper("B").Decision != model.DecisionKeep {
		t.Fatal("rescreen overwrote user decisions")
	}

	// 5. Regenerate queries: AI called again, existing papers retained.
	post("/projects/"+id+"/search", url.Values{"mode": {"generate"}})
	p = waitIdle(t, st, id)
	if fb.calls["queries"] != 2 || len(p.Papers) != 4 {
		t.Fatalf("regenerate: queries calls=%d papers=%d", fb.calls["queries"], len(p.Papers))
	}

	// 6. Download kept papers (B, C, D): D and ok-DOIs succeed, B fails.
	post("/projects/"+id+"/download", nil)
	p = waitIdle(t, st, id)
	if p.Stage != model.StageDone {
		t.Fatalf("stage after download: %s", p.Stage)
	}
	if p.Paper("D").Download.Status != model.DownloadOK || p.Paper("B").Download.Status != model.DownloadFailed || p.Paper("A").Download.Status != model.DownloadPending {
		t.Fatalf("downloads: D=%+v B=%+v A=%+v", p.Paper("D").Download, p.Paper("B").Download, p.Paper("A").Download)
	}
	if !strings.HasSuffix(p.Paper("D").Download.Path, "2024-author-delta-new-paper-found-by-hand-written-query.pdf") {
		t.Fatalf("pdf path: %s", p.Paper("D").Download.Path)
	}
	body := get("/projects/" + id + "?view=downloads")
	if !strings.Contains(body, "got PDF") || !strings.Contains(body, "Retry") || !strings.Contains(body, "HTTP 403 paywalled") {
		t.Fatalf("downloads view: %s", body)
	}
	// Serving the stored file.
	if !strings.HasPrefix(get("/files/"+id+"/2024-author-delta-new-paper-found-by-hand-written-query.pdf"), "%PDF") {
		t.Fatal("pdf not served")
	}
	// Retry only re-runs the failed one and does not touch D.
	post("/projects/"+id+"/papers/B/retry", nil)
	p = waitIdle(t, st, id)
	if p.Paper("B").Download.Status != model.DownloadFailed || len(p.Paper("B").Download.Attempts) != 1 {
		t.Fatalf("retry: %+v", p.Paper("B").Download)
	}

	// 7. Go back: change keep/drop after downloading, then download only the missing one.
	post("/projects/"+id+"/papers/A/decision", url.Values{"decision": {"keep"}}) // A: drop → keep (toggle sets keep)
	p = waitIdle(t, st, id)
	if p.Paper("A").Decision != model.DecisionKeep {
		t.Fatalf("A should be keep: %+v", p.Paper("A"))
	}
	body = get("/projects/" + id + "?view=screening")
	if !strings.Contains(body, "Download 3 missing PDFs") { // A (pending), B and C (failed)
		t.Fatalf("pending count wrong:\n%s", body[strings.Index(body, "toolbar"):][:600])
	}
	post("/projects/"+id+"/download", nil)
	p = waitIdle(t, st, id)
	if p.Paper("A").Download.Status != model.DownloadOK || p.Stage != model.StageDone {
		t.Fatalf("second download: %+v stage=%s", p.Paper("A").Download, p.Stage)
	}

	// 8. Master table + extraction + exports.
	post("/projects/"+id+"/extract", nil)
	p = waitIdle(t, st, id)
	if p.Job.Error != "" {
		t.Fatalf("extract: %s", p.Job.Error)
	}
	for _, pid := range []string{"A", "B", "C", "D"} {
		if p.Paper(pid).Extract == nil || p.Paper(pid).Extract.Objective != "obj "+pid {
			t.Fatalf("extract %s: %+v", pid, p.Paper(pid).Extract)
		}
	}
	if !strings.Contains(get("/projects/"+id+"/table"), "obj D") || !strings.Contains(get("/projects/"+id+"/table.csv"), "obj D") {
		t.Fatal("table missing extraction")
	}
	// Extract again without force → nothing to do, redirect with message, no AI call.
	before := fb.calls["extract"]
	resp = post("/projects/"+id+"/extract", nil)
	if !strings.Contains(resp.Header.Get("Location"), "already") || fb.calls["extract"] != before {
		t.Fatalf("extract idempotency: %s calls=%d", resp.Header.Get("Location"), fb.calls["extract"])
	}
	post("/projects/"+id+"/extract", url.Values{"force": {"1"}})
	waitIdle(t, st, id)
	if fb.calls["extract"] != before+1 {
		t.Fatal("force re-extract did not call AI")
	}

	// 9. Synthesis and regenerate.
	post("/projects/"+id+"/synthesize", nil)
	p = waitIdle(t, st, id)
	if !strings.Contains(p.Synthesis, "Synthesised") || p.SynthesisModel != "fake-model" {
		t.Fatalf("synthesis: %q %s", p.Synthesis, p.Job.Error)
	}
	// Every kept paper has a bib key, the draft cites them, unknown keys are flagged in the log.
	for _, pp := range p.Kept() {
		if pp.BibKey == "" || !strings.Contains(p.Synthesis, "[@"+pp.BibKey+"]") {
			t.Fatalf("paper %s key %q not cited in %q", pp.ID, pp.BibKey, p.Synthesis)
		}
	}
	if !strings.Contains(strings.Join(p.Job.Log, "\n"), "nokey2020x") {
		t.Fatal("unknown citation key not flagged")
	}
	if !strings.Contains(get("/projects/"+id+"?view=results"), "Download LaTeX") {
		t.Fatal("results view missing exports")
	}
	tex := get("/projects/" + id + "/synthesis.tex")
	if !strings.Contains(tex, `\section{Background}`) || !strings.Contains(tex, `\citep{`+p.Kept()[0].BibKey) {
		t.Fatalf("tex export: %s", tex)
	}
	mdOut := get("/projects/" + id + "/synthesis.md")
	if !strings.Contains(mdOut, "bibliography: bibliography.bib") {
		t.Fatalf("md export: %s", mdOut)
	}
	bib := get("/projects/" + id + "/export.bib")
	for _, pp := range p.Kept() {
		if !strings.Contains(bib, "{"+pp.BibKey+",") {
			t.Fatalf("bib missing key %s:\n%s", pp.BibKey, bib)
		}
	}
	post("/projects/"+id+"/synthesize", nil)
	waitIdle(t, st, id)
	if fb.calls["synthesis"] != 2 {
		t.Fatal("regenerate did not call AI")
	}

	// 10. Edit settings late in the flow: nothing is lost, stage stays done.
	post("/projects/"+id+"/settings", url.Values{"question": {"Revised question"}, "sources": {"fakedb"}})
	p = waitIdle(t, st, id)
	if p.Question != "Revised question" || len(p.Papers) != 4 || p.Stage != model.StageDone || p.Synthesis == "" {
		t.Fatalf("late settings edit lost data: %+v", p)
	}
	for _, v := range []string{"setup", "search", "screening", "downloads", "results"} {
		if !strings.Contains(get("/projects/"+id+"?view="+v), "5 · Table") {
			t.Fatalf("view %s not navigable", v)
		}
	}

	// 11. Persistence: a fresh store loads the same state.
	st2, err := store.Open(st.Dir())
	if err != nil {
		t.Fatal(err)
	}
	p2, _ := st2.Get(id)
	if len(p2.Papers) != 4 || p2.Paper("A").Download.Status != model.DownloadOK || p2.Paper("D").Extract == nil {
		t.Fatal("state not persisted")
	}

	// 12. Reset clears results, keeps settings and files.
	post("/projects/"+id+"/reset", nil)
	p = waitIdle(t, st, id)
	if len(p.Papers) != 0 || p.Question != "Revised question" || p.Stage != model.StageSetup {
		t.Fatalf("reset: %+v", p)
	}
}

// TestEndToEndWithJev runs the search with both screeners: a fake Jev server
// agrees with Claude on A (keep) and C (drop), clashes on D (Claude keep, Jev
// drop) and both are unsure on B. Agreement pre-fills decisions; the clash
// and the double-unsure stay undecided.
func TestEndToEndWithJev(t *testing.T) {
	jevSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer jk" {
			w.WriteHeader(401)
			return
		}
		var req struct {
			State struct {
				Paper struct {
					Title string `json:"title"`
				} `json:"paper"`
			} `json:"state"`
			Questions map[string]json.RawMessage `json:"questions"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.Questions) != 2 { // no exclusion criteria in this project → 2 questions
			t.Errorf("questions: %d", len(req.Questions))
		}
		rel, inc := 0.5, 0.1 // drop
		switch {
		case strings.HasPrefix(req.State.Paper.Title, "Alpha"):
			rel, inc = 3.8, 0.9 // keep
		case strings.HasPrefix(req.State.Paper.Title, "Beta"):
			rel, inc = 2.0, 0.5 // unsure
		}
		fmt.Fprintf(w, `{"model":"jev-1.13.0","answers":{"relevance":{"type":"score","score":%v,"confidence":0.8},"include":{"type":"noul","noul":%v}}}`, rel, inc)
	}))
	defer jevSrv.Close()

	st, _ := store.Open(t.TempDir())
	cfg := config.FromEnv()
	cfg.AIBackend, cfg.AnthropicAPIKey, cfg.BrowserFetch = "api", "", "off"
	cfg.JevAPIKey, cfg.JevURL, cfg.Screener = "jk", jevSrv.URL, "both"
	srv, err := New(cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	fb := &fakeBackend{calls: map[string]int{}, screens: map[string]int{"A": 90, "B": 50, "C": 10, "D": 80}}
	srv.ai = ai.New(fb)
	srv.jev = jev.New("jk", "", jevSrv.URL)
	srv.reg.Add(&fakeDB{papers: map[string][]*model.Paper{
		"genai adoption public sector": {paper("A", "10.ok/a", "Alpha adoption"), paper("B", "10.x/b", "Beta borderline"), paper("D", "10.ok/d", "Delta disputed")},
		"llm governance government":    {paper("C", "10.x/c", "Gamma unrelated")},
	}})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, _ := client.PostForm(ts.URL+"/projects", url.Values{"question": {"Q"}, "sources": {"fakedb"}, "start": {"1"}})
	resp.Body.Close()
	id := strings.Split(strings.TrimPrefix(resp.Header.Get("Location"), "/projects/"), "?")[0]
	p := waitIdle(t, st, id)
	if p.Job.Error != "" {
		t.Fatalf("search: %s\n%s", p.Job.Error, strings.Join(p.Job.Log, "\n"))
	}
	a, b, c, d := p.Paper("A"), p.Paper("B"), p.Paper("C"), p.Paper("D")
	if !a.HasJev() || a.Jev.Model != "jev-1.13.0" || a.Agreement() != "agree" || a.Decision != model.DecisionKeep || a.RankScore() != (90+a.Jev.Score)/2 {
		t.Fatalf("A: %+v jev=%+v", a, a.Jev)
	}
	if c.Agreement() != "agree" || c.Decision != model.DecisionDrop {
		t.Fatalf("C: %+v jev=%+v", c, c.Jev)
	}
	if d.Agreement() != "conflict" || d.Decision != model.DecisionUndecided || d.FinalVerdict() != model.VerdictUnsure {
		t.Fatalf("D should be a conflict left undecided: %+v jev=%+v", d, d.Jev)
	}
	if b.Agreement() != "agree" || b.Decision != model.DecisionUndecided { // unsure + unsure
		t.Fatalf("B: %+v jev=%+v", b, b.Jev)
	}
	if !strings.Contains(strings.Join(p.Job.Log, "\n"), "1 screener conflicts") {
		t.Fatalf("log should mention conflicts:\n%s", strings.Join(p.Job.Log, "\n"))
	}
	// UI: conflict filter and badges; header shows the second screener.
	get := func(path string) string {
		r, err := client.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		bb, _ := io.ReadAll(r.Body)
		return string(bb)
	}
	body := get("/projects/" + id + "?view=screening&filter=conflict")
	if !strings.Contains(body, "Delta disputed") || strings.Contains(body, "Alpha adoption") || !strings.Contains(body, "screeners disagree") || !strings.Contains(body, "2nd screener: Jev") {
		t.Fatalf("conflict view:\n%s", body)
	}
	// Accept AI suggestions must not decide the conflict, and must never
	// override a decision the user made by hand.
	if r, err := client.PostForm(ts.URL+"/projects/"+id+"/papers/C/decision", url.Values{"decision": {"keep"}}); err == nil {
		r.Body.Close() // C: AI says drop, user says keep
	}
	if r, err := client.PostForm(ts.URL+"/projects/"+id+"/bulk", url.Values{"action": {"accept-ai"}}); err == nil {
		r.Body.Close()
	}
	p, _ = st.Get(id)
	if p.Paper("D").Decision != model.DecisionUndecided || p.Paper("A").Decision != model.DecisionKeep {
		t.Fatal("accept-ai mishandled conflict")
	}
	if p.Paper("C").Decision != model.DecisionKeep || p.Paper("C").DecisionBy != "user" {
		t.Fatalf("accept-ai overrode a user decision: %+v", p.Paper("C"))
	}
	// Table export carries both screeners.
	_ = st.Update(id, func(p *model.Project) error { p.Stage = model.StageDone; return nil })
	csv := get("/projects/" + id + "/table.csv")
	if !strings.Contains(csv, "Jev score") || !strings.Contains(csv, "relevance 3.80/4") || !strings.Contains(csv, "agree") {
		t.Fatalf("csv: %s", csv)
	}
	// Jev outage: every request fails → job reports it, Claude verdicts remain usable.
	jevSrv.Close()
	if r, err := client.PostForm(ts.URL+"/projects/"+id+"/search", url.Values{"mode": {"rescreen"}}); err == nil {
		r.Body.Close()
	}
	p = waitIdle(t, st, id)
	if p.Job.Error == "" || !strings.Contains(p.Job.Error, "jev") {
		t.Fatalf("expected jev failure to surface, got %q", p.Job.Error)
	}
}

// TestTargetExpansionAndSnowball: the first round includes only A; with a
// target of 3 the job expands automatically (one broader query, duplicates
// filtered) and finds E and F2 → target met after one expansion. Snowball then
// adds R (reference) and F (citation).
func TestTargetExpansionAndSnowball(t *testing.T) {
	st, _ := store.Open(t.TempDir())
	cfg := config.FromEnv()
	cfg.AIBackend, cfg.AnthropicAPIKey, cfg.BrowserFetch = "api", "", "off"
	srv, _ := New(cfg, st)
	fb := &fakeBackend{calls: map[string]int{}, screens: map[string]int{"A": 90, "B": 50, "C": 10, "E": 85, "G": 75, "R": 88, "F": 20}}
	srv.ai = ai.New(fb)
	db := &fakeDB{papers: map[string][]*model.Paper{
		"genai adoption public sector": {paper("A", "10.ok/a", "Alpha"), paper("B", "10.x/b", "Beta")},
		"llm governance government":    {paper("C", "10.x/c", "Gamma")},
		"expanded query":               {paper("E", "10.ok/e", "Epsilon broader hit"), paper("G", "10.ok/g", "Gamma-two broader hit"), paper("A", "10.ok/a", "Alpha")},
	}}
	srv.reg.Add(db)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, _ := client.PostForm(ts.URL+"/projects", url.Values{"question": {"Q"}, "sources": {"fakedb"}, "target": {"3"}, "start": {"1"}})
	resp.Body.Close()
	id := strings.Split(strings.TrimPrefix(resp.Header.Get("Location"), "/projects/"), "?")[0]
	p := waitIdle(t, st, id)
	if p.Job.Error != "" {
		t.Fatalf("search: %s\n%s", p.Job.Error, strings.Join(p.Job.Log, "\n"))
	}
	if fb.calls["expand"] != 1 || p.Rounds != 2 || len(p.Papers) != 5 || keptCount(p.Papers) != 3 {
		t.Fatalf("expansion: expandCalls=%d rounds=%d papers=%d kept=%d\n%s", fb.calls["expand"], p.Rounds, len(p.Papers), keptCount(p.Papers), strings.Join(p.Job.Log, "\n"))
	}
	// Queries: 2 from round 1, 1 new from round 2 (duplicate filtered), with yield stats.
	if len(p.Queries) != 3 || p.Queries[2].Round != 2 || p.Queries[2].Query != "expanded query" || p.Queries[2].Relevant != 3 || p.Queries[0].Relevant != 2 {
		t.Fatalf("queries: %+v", p.Queries)
	}
	if !strings.Contains(strings.Join(p.Job.Log, "\n"), "Target of 3 included papers reached") {
		t.Fatalf("log: %s", strings.Join(p.Job.Log, "\n"))
	}
	if !strings.Contains(p.Paper("A").FoundBy[0], "genai adoption") || len(p.Paper("A").FoundBy) != 2 {
		t.Fatalf("FoundBy provenance: %+v", p.Paper("A").FoundBy)
	}
	// Snowball from the 3 kept papers.
	r, _ := client.PostForm(ts.URL+"/projects/"+id+"/search", url.Values{"mode": {"snowball"}})
	r.Body.Close()
	p = waitIdle(t, st, id)
	if p.Job.Error != "" {
		t.Fatalf("snowball: %s", p.Job.Error)
	}
	if p.Rounds != 3 || len(p.Papers) != 7 || p.Paper("R") == nil || p.Paper("R").Decision != model.DecisionKeep || p.Paper("F").Decision != model.DecisionDrop {
		t.Fatalf("snowball result: rounds=%d papers=%d R=%+v F=%+v", p.Rounds, len(p.Papers), p.Paper("R"), p.Paper("F"))
	}
	if p.Paper("R").FoundIn[0] != "fakedb (reference)" || !strings.HasPrefix(p.Paper("R").FoundBy[0], "snowball") {
		t.Fatalf("snowball provenance: %+v", p.Paper("R"))
	}
	if last := p.Queries[len(p.Queries)-1]; !strings.HasPrefix(last.Query, "snowball from 3 kept") || last.Round != 3 {
		t.Fatalf("snowball query record: %+v", last)
	}
	// "run" mode never expands even when below target.
	before := fb.calls["expand"]
	_ = st.Update(id, func(p *model.Project) error { p.Target = 50; return nil })
	r, _ = client.PostForm(ts.URL+"/projects/"+id+"/search", url.Values{"mode": {"run"}})
	r.Body.Close()
	p = waitIdle(t, st, id)
	if fb.calls["expand"] != before || p.Job.Error != "" {
		t.Fatalf("run mode expanded: %d %s", fb.calls["expand"]-before, p.Job.Error)
	}
	// Setup view shows and saves the target.
	rr, _ := client.Get(ts.URL + "/projects/" + id + "?view=setup")
	bb, _ := io.ReadAll(rr.Body)
	rr.Body.Close()
	if !strings.Contains(string(bb), `name="target" value="50"`) {
		t.Fatal("target not shown in setup")
	}
}

// TestPreprintFilterAdjudicationAndThresholds: preprints are excluded at
// search time when the project is peer-reviewed-only; the third-reviewer pass
// decides undecided papers (applied only with enough confidence); threshold
// bulk actions handle the rest.
func TestPreprintFilterAdjudicationAndThresholds(t *testing.T) {
	st, _ := store.Open(t.TempDir())
	cfg := config.FromEnv()
	cfg.AIBackend, cfg.AnthropicAPIKey, cfg.BrowserFetch = "api", "", "off"
	srv, _ := New(cfg, st)
	fb := &fakeBackend{calls: map[string]int{}, screens: map[string]int{"A": 90, "B": 50, "U": 55, "T": 62, "L": 30, "P": 80, "S": 80}}
	srv.ai = ai.New(fb)
	db := &fakeDB{papers: map[string][]*model.Paper{
		"genai adoption public sector": {paper("A", "10.ok/a", "Alpha"), paper("B", "10.x/b", "Beta"), paper("P", "10.48550/arxiv.2401.01291", "Preprint on arXiv"), paper("S", "10.2139/ssrn.123", "SSRN working paper")},
		"llm governance government":    {paper("U", "10.x/u", "Upsilon"), paper("T", "10.x/t", "Tau"), paper("L", "10.x/l", "Lambda"), german()},
	}}
	srv.reg.Add(db)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, _ := client.PostForm(ts.URL+"/projects", url.Values{"question": {"Q"}, "sources": {"fakedb"}, "peer_reviewed_only": {"1"}, "english_only": {"1"}, "start": {"1"}})
	resp.Body.Close()
	id := strings.Split(strings.TrimPrefix(resp.Header.Get("Location"), "/projects/"), "?")[0]
	p := waitIdle(t, st, id)
	if p.Job.Error != "" {
		t.Fatal(p.Job.Error)
	}
	if !p.PeerReviewedOnly || !p.EnglishOnly || len(p.Papers) != 5 || p.Paper("P") != nil || p.Paper("S") != nil || p.Paper("G") != nil {
		t.Fatalf("preprints/non-English not excluded: n=%d", len(p.Papers))
	}
	if !strings.Contains(strings.Join(p.Job.Log, "\n"), "2 preprints excluded") || !strings.Contains(strings.Join(p.Job.Log, "\n"), "1 non-English excluded") {
		t.Fatalf("log: %s", strings.Join(p.Job.Log, "\n"))
	}
	// Undecided: B(50), U(55), T(62). Adjudicate: B→drop(0.8), U→keep(0.3 ⇒ stays), T→keep(0.9).
	r, _ := client.PostForm(ts.URL+"/projects/"+id+"/adjudicate", url.Values{"standard": {"strict"}})
	r.Body.Close()
	p = waitIdle(t, st, id)
	if !strings.Contains(strings.Join(p.Job.Log, "\n"), "strict standard") {
		t.Fatalf("standard not logged: %s", strings.Join(p.Job.Log, "\n"))
	}
	if p.Job.Error != "" || fb.calls["adjudicate"] != 1 {
		t.Fatalf("adjudicate: %s calls=%d", p.Job.Error, fb.calls["adjudicate"])
	}
	if p.Paper("B").Decision != model.DecisionDrop || p.Paper("T").Decision != model.DecisionKeep || p.Paper("U").Decision != model.DecisionUndecided || p.Paper("U").Adjudication == nil {
		t.Fatalf("adjudication results: B=%s T=%s U=%s", p.Paper("B").Decision, p.Paper("T").Decision, p.Paper("U").Decision)
	}
	if !strings.Contains(strings.Join(p.Job.Log, "\n"), "1 → keep, 1 → drop, 0 still unsure, 1 left undecided") {
		t.Fatalf("log: %s", strings.Join(p.Job.Log, "\n"))
	}
	// Threshold: U(55) with keep≥50 → keep by user.
	r, _ = client.PostForm(ts.URL+"/projects/"+id+"/bulk", url.Values{"action": {"threshold"}, "keep_min": {"50"}, "drop_max": {"40"}})
	r.Body.Close()
	p, _ = st.Get(id)
	if p.Paper("U").Decision != model.DecisionKeep || p.Paper("U").DecisionBy != "user" {
		t.Fatalf("threshold: %+v", p.Paper("U"))
	}
	// Turning the filters off lets preprints and non-English in, flagged; bulk actions remove them.
	fb.screens["G"] = 80
	r, _ = client.PostForm(ts.URL+"/projects/"+id+"/settings", url.Values{"question": {"Q"}, "sources": {"fakedb"}, "peer_reviewed_only": {"0"}, "english_only": {"0"}})
	r.Body.Close()
	r, _ = client.PostForm(ts.URL+"/projects/"+id+"/search", url.Values{"mode": {"run"}})
	r.Body.Close()
	p = waitIdle(t, st, id)
	if p.Paper("P") == nil || !p.Paper("P").Preprint || p.Paper("P").Decision != model.DecisionKeep {
		t.Fatalf("preprint should be present and flagged when filter off: %+v", p.Paper("P"))
	}
	body := func() string {
		rr, _ := client.Get(ts.URL + "/projects/" + id + "?view=screening")
		b, _ := io.ReadAll(rr.Body)
		rr.Body.Close()
		return string(b)
	}()
	if !strings.Contains(body, "Drop 2 preprints") || !strings.Contains(body, ">preprint<") || !strings.Contains(body, "Drop 1 non-English") || !strings.Contains(body, ">non-English<") {
		t.Fatalf("screening view lacks preprint/language controls:\n%s", body[:2000])
	}
	r, _ = client.PostForm(ts.URL+"/projects/"+id+"/bulk", url.Values{"action": {"drop-non-english"}})
	r.Body.Close()
	p, _ = st.Get(id)
	if p.Paper("G").Decision != model.DecisionDrop || !p.Paper("G").NonEnglish {
		t.Fatal("drop-non-english did not apply")
	}
	r, _ = client.PostForm(ts.URL+"/projects/"+id+"/bulk", url.Values{"action": {"drop-preprints"}})
	r.Body.Close()
	p, _ = st.Get(id)
	if p.Paper("P").Decision != model.DecisionDrop || p.Paper("S").Decision != model.DecisionDrop {
		t.Fatal("drop-preprints did not apply")
	}
}

func TestConceptMatrix(t *testing.T) {
	st, _ := store.Open(t.TempDir())
	cfg := config.FromEnv()
	cfg.AIBackend, cfg.AnthropicAPIKey, cfg.BrowserFetch = "api", "", "off"
	srv, _ := New(cfg, st)
	fb := &fakeBackend{calls: map[string]int{}, screens: map[string]int{}}
	srv.ai = ai.New(fb)
	_ = st.Put(&model.Project{ID: "cm1", Title: "CM", Question: "Q", Stage: model.StageDone, CreatedAt: time.Now(), Papers: []*model.Paper{
		{ID: "a", Title: "Alpha", Authors: []string{"Ann Author"}, Year: 2024, Decision: model.DecisionKeep, Keywords: []string{"governance", "ai"}, Extract: &model.Extraction{Objective: "o", Findings: "f"}},
		{ID: "b", Title: "Beta", Year: 2023, Decision: model.DecisionKeep, Keywords: []string{"governance", "trust"}},
		{ID: "c", Title: "Dropped", Decision: model.DecisionDrop},
	}})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	r, _ := client.PostForm(ts.URL+"/projects/cm1/concepts/build", nil)
	r.Body.Close()
	p := waitIdle(t, st, "cm1")
	if p.Job.Error != "" || fb.calls["propose"] != 1 || fb.calls["assign"] != 1 {
		t.Fatalf("build: %s calls=%v", p.Job.Error, fb.calls)
	}
	if len(p.Concepts) != 3 || p.Concepts[0].Name != "Governance" { // duplicate filtered
		t.Fatalf("concepts: %+v", p.Concepts)
	}
	if hits := p.Paper("a").Concepts; len(hits) != 1 || hits[0].Name != "Governance" || hits[0].Note != "sets rules" {
		t.Fatalf("assignment: %+v", hits) // case-insensitive match, unknown concept dropped
	}
	rr, _ := client.Get(ts.URL + "/projects/cm1/concepts")
	b, _ := io.ReadAll(rr.Body)
	rr.Body.Close()
	if rr.StatusCode != 200 || !strings.Contains(string(b), "Governance") || !strings.Contains(string(b), "●") || strings.Contains(string(b), "Dropped") {
		t.Fatalf("matrix page: %d\n%s", rr.StatusCode, b[:1500])
	}
	rr, _ = client.Get(ts.URL + "/projects/cm1/concepts.csv")
	b, _ = io.ReadAll(rr.Body)
	rr.Body.Close()
	if !strings.Contains(string(b), "Cite key,First author,Year,Title,Venue,Governance,Adoption,Trust") || !strings.Contains(string(b), "sets rules") {
		t.Fatalf("csv: %s", b)
	}
	rr, _ = client.Get(ts.URL + "/projects/cm1/concepts.xlsx")
	rr.Body.Close()
	if rr.StatusCode != 200 {
		t.Fatal("xlsx export")
	}
	// Re-assign keeps concepts, only assigns again.
	r, _ = client.PostForm(ts.URL+"/projects/cm1/concepts/build", nil)
	r.Body.Close()
	p = waitIdle(t, st, "cm1")
	if fb.calls["propose"] != 1 || fb.calls["assign"] != 2 {
		t.Fatalf("re-assign calls: %v", fb.calls)
	}
}
