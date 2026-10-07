package server

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"literaturereview/internal/config"
	"literaturereview/internal/model"
	"literaturereview/internal/store"
)

func newTestServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.FromEnv()
	cfg.AnthropicAPIKey = ""
	cfg.AIBackend = "api" // no key → AI disabled, keeps the test offline
	cfg.BrowserFetch = "off"
	cfg.ProxyPrefix = "https://ezproxy.example.fi/login?url="
	cfg.ProxyCookie = "ezproxy=abc"
	s, err := New(cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts
}

func TestPagesRender(t *testing.T) {
	s, ts := newTestServer(t)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	get := func(path string) (int, string) {
		resp, err := client.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var sb strings.Builder
		buf := make([]byte, 1<<20)
		for {
			n, err := resp.Body.Read(buf)
			sb.Write(buf[:n])
			if err != nil {
				break
			}
		}
		return resp.StatusCode, sb.String()
	}

	if code, body := get("/"); code != 200 || !strings.Contains(body, "New review") {
		t.Fatalf("index: %d %s", code, body[:200])
	}

	form := url.Values{"question": {"How do organisations adopt generative AI?"}, "sources": {"openalex", "pubmed"}, "year_from": {"2018"}, "year_to": {"2025"}}
	resp, err := client.PostForm(ts.URL+"/projects", form)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 303 {
		t.Fatalf("create: %d", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	id := strings.TrimPrefix(loc, "/projects/")

	if code, body := get(loc); code != 200 || !strings.Contains(body, "Save &amp; go to search strategy") {
		t.Fatalf("setup page: %d\n%s", code, body)
	}

	// Inject screened papers and move to screening.
	if err := s.store.Update(id, func(p *model.Project) error {
		p.Stage = model.StageScreening
		p.Queries = []model.Query{{Source: "openalex", Query: "generative AI adoption", Results: 2, Total: 1234, URL: "https://api.openalex.org/works?search=generative+AI+adoption"}}
		p.Papers = []*model.Paper{
			{ID: "p1", Title: "GenAI in the public sector", Authors: []string{"A. One", "B. Two"}, Year: 2024, Venue: "MISQ", DOI: "10.1/x", Abstract: "An abstract.", AIScore: 85, AIVerdict: model.VerdictKeep, AIRationale: "Directly relevant.", Decision: model.DecisionKeep, FoundIn: []string{"openalex"}, OpenAccess: true, Citations: 12},
			{ID: "p2", Title: "Unrelated ML paper <script>", Year: 2020, AIScore: 10, AIVerdict: model.VerdictDrop, AIRationale: "Off-topic.", Decision: model.DecisionDrop, FoundIn: []string{"openalex"}},
			{ID: "p3", Title: "Borderline", Year: 2021, AIScore: 55, AIVerdict: model.VerdictUnsure},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	code, body := get(loc)
	if code != 200 || !strings.Contains(body, "GenAI in the public sector") || !strings.Contains(body, "&lt;script&gt;") || !strings.Contains(body, "undecided 1") {
		t.Fatalf("screening page: %d\n%s", code, body)
	}
	if code, body := get(loc + "?filter=keep"); code != 200 || strings.Contains(body, "Borderline") {
		t.Fatalf("filter keep failed: %d", code)
	}
	// Search-strategy view shows the query, hit count and the real request URL.
	if code, body := get(loc + "?view=search"); code != 200 || !strings.Contains(body, "1234") || !strings.Contains(body, "api.openalex.org/works?search=generative") || !strings.Contains(body, "Re-screen all papers") {
		t.Fatalf("search view: %d\n%s", code, body)
	}
	// Setup view is editable and saving keeps the papers.
	resp0, _ := client.PostForm(ts.URL+"/projects/"+id+"/settings", url.Values{"question": {"Changed question"}, "sources": {"pubmed"}, "max_per_source": {"10"}, "discipline": {"Information Systems research"}})
	resp0.Body.Close()
	if p0, _ := s.store.Get(id); p0.Question != "Changed question" || len(p0.Papers) != 3 || len(p0.Sources) != 1 || p0.Discipline != "Information Systems research" {
		t.Fatalf("settings not applied or papers lost: %+v", p0)
	}
	// Hand-edited queries, two lines → two queries; unchanged text keeps its record.
	resp0, _ = client.PostForm(ts.URL+"/projects/"+id+"/queries", url.Values{"q_pubmed": {"all:foo AND abs:bar\n\nti:baz"}, "run": {"0"}})
	resp0.Body.Close()
	if p0, _ := s.store.Get(id); len(p0.Queries) != 2 || p0.Queries[1].Query != "ti:baz" || p0.Queries[0].Rationale != "edited by hand" {
		t.Fatalf("queries not saved: %+v", p0.Queries)
	}

	// Toggle decision via htmx endpoint.
	resp, err = client.PostForm(ts.URL+"/projects/"+id+"/papers/p3/decision", url.Values{"decision": {"keep"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	p, _ := s.store.Get(id)
	if p.Paper("p3").Decision != model.DecisionKeep || p.Paper("p3").DecisionBy != "user" {
		t.Fatal("toggle did not set keep by user")
	}
	resp, _ = client.PostForm(ts.URL+"/projects/"+id+"/papers/p3/decision", url.Values{"decision": {"keep"}})
	resp.Body.Close()
	if p.Paper("p3").Decision != model.DecisionUndecided {
		t.Fatal("second click did not clear decision")
	}

	// Bulk actions.
	resp, _ = client.PostForm(ts.URL+"/projects/"+id+"/bulk", url.Values{"action": {"keep-undecided"}})
	resp.Body.Close()
	if k, _, u := p.Counts(); k != 2 || u != 0 {
		t.Fatalf("bulk keep-undecided: keep=%d undecided=%d", k, u)
	}

	// Downloads / done view with attempts.
	_ = s.store.Update(id, func(p *model.Project) error {
		p.Stage = model.StageDone
		p.Paper("p1").Download = model.DownloadResult{Status: model.DownloadOK, Method: "arXiv", Path: "/x/pdfs/a.pdf", Bytes: 2048, Attempts: []model.Attempt{{Method: "arXiv", OK: true, Message: "saved", At: time.Now()}}}
		p.Paper("p3").Download = model.DownloadResult{Status: model.DownloadFailed, Attempts: []model.Attempt{{Method: "publisher (direct)", Message: "HTTP 403", At: time.Now()}}}
		p.Job = &model.JobStatus{Name: "download", Log: []string{"done"}}
		p.Synthesis = "# Overview\n\nSome **bold** text.\n\n- bullet one\n- bullet two\n\n1. first\n2. second"
		return nil
	})
	code, body = get(loc)
	if code != 200 || !strings.Contains(body, "<strong>bold</strong>") || !strings.Contains(body, "<ol>") || !strings.Contains(body, "Master table") {
		t.Fatalf("results view: %d\n%s", code, body)
	}
	if code, body := get(loc + "?view=downloads"); code != 200 || !strings.Contains(body, "got PDF") || !strings.Contains(body, "Retry") {
		t.Fatalf("downloads view: %d\n%s", code, body)
	}
	// Going back to keep/drop and setup works without losing anything.
	if code, body := get(loc + "?view=screening"); code != 200 || !strings.Contains(body, "GenAI in the public sector") {
		t.Fatalf("back to screening: %d", code)
	}
	if code, body := get(loc + "?view=setup"); code != 200 || !strings.Contains(body, "Clear all results") {
		t.Fatalf("back to setup: %d", code)
	}
	if code, body := get("/projects/" + id + "/downloads"); code != 200 || !strings.Contains(body, "university proxy") {
		t.Fatalf("downloads partial: %d", code)
	}
	if code, body := get("/projects/" + id + "/export.bib"); code != 200 || !strings.Contains(body, "@article{one2024genai") {
		t.Fatalf("bib export: %d\n%s", code, body)
	}
	if code, _ := get("/projects/" + id + "/export.json"); code != 200 {
		t.Fatalf("json export: %d", code)
	}
	// Search without API key must redirect with an error, not crash.
	for _, mode := range []string{"generate", "run", "rescreen"} {
		resp, _ = client.PostForm(ts.URL+"/projects/"+id+"/search", url.Values{"mode": {mode}})
		resp.Body.Close()
		if resp.StatusCode != 303 || !strings.Contains(resp.Header.Get("Location"), "error=") {
			t.Fatalf("search %s without key: %d %s", mode, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
	// Reset clears results but keeps settings.
	resp, _ = client.PostForm(ts.URL+"/projects/"+id+"/reset", nil)
	resp.Body.Close()
	if p0, _ := s.store.Get(id); len(p0.Papers) != 0 || p0.Question != "Changed question" || p0.Stage != model.StageSetup {
		t.Fatalf("reset: %+v", p0)
	}
}

func TestRenderMarkdown(t *testing.T) {
	out := string(renderMarkdown("## Themes\n\nPara with *em*.\n\n- a\n- b <x>"))
	for _, want := range []string{"<h3>Themes</h3>", "<em>em</em>", "<li>b &lt;x&gt;</li>", "<ul>"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %s", want, out)
		}
	}
}

func TestMasterTable(t *testing.T) {
	s, ts := newTestServer(t)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	p := &model.Project{ID: "tbl1", Title: "T", Question: "Q", Stage: model.StageScreening, CreatedAt: time.Now(), Papers: []*model.Paper{
		{ID: "a", Title: "Kept & extracted <b>", Authors: []string{"Ann Author"}, Year: 2024, DOI: "10.1/a", Decision: model.DecisionKeep, AIScore: 80, AIVerdict: model.VerdictKeep,
			Extract: &model.Extraction{Objective: "obj", Method: "survey n=100", Findings: "finding", Keywords: []string{"k1", "k2"}}},
		{ID: "b", Title: "Kept not extracted", Year: 2023, Decision: model.DecisionKeep},
		{ID: "c", Title: "Dropped", Decision: model.DecisionDrop},
	}}
	if err := s.store.Put(p); err != nil {
		t.Fatal(err)
	}
	// Not available before the user has proceeded past screening.
	resp, _ := client.Get(ts.URL + "/projects/tbl1/table")
	resp.Body.Close()
	if resp.StatusCode != 303 {
		t.Fatalf("table before decision: %d", resp.StatusCode)
	}
	_ = s.store.Update("tbl1", func(p *model.Project) error { p.Stage = model.StageDone; return nil })
	resp, _ = client.Get(ts.URL + "/projects/tbl1/table")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	html := string(body)
	if resp.StatusCode != 200 || !strings.Contains(html, "survey n=100") || !strings.Contains(html, "not extracted yet") || strings.Contains(html, "Dropped") || !strings.Contains(html, "&lt;b&gt;") {
		t.Fatalf("table page: %d\n%s", resp.StatusCode, html)
	}
	resp, _ = client.Get(ts.URL + "/projects/tbl1/table.csv")
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), "Key findings") || !strings.Contains(string(body), "survey n=100") || strings.Contains(string(body), "Dropped") {
		t.Fatalf("csv: %d %s", resp.StatusCode, body)
	}
	resp, _ = client.Get(ts.URL + "/projects/tbl1/table.xlsx")
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("xlsx: %d", resp.StatusCode)
	}
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("xlsx not a zip: %v", err)
	}
	var sheet string
	for _, f := range zr.File {
		if f.Name == "xl/worksheets/sheet1.xml" {
			rc, _ := f.Open()
			b, _ := io.ReadAll(rc)
			rc.Close()
			sheet = string(b)
		}
	}
	if !strings.Contains(sheet, "survey n=100") || !strings.Contains(sheet, "Kept &amp; extracted &lt;b&gt;") || !strings.Contains(sheet, `<pane ySplit="1"`) {
		t.Fatalf("sheet xml wrong: %s", sheet[:300])
	}
	if colName(0) != "A" || colName(25) != "Z" || colName(26) != "AA" {
		t.Fatal("colName")
	}
}

func TestKeywordMapPageAndExports(t *testing.T) {
	s, ts := newTestServer(t)
	p := &model.Project{ID: "kw1", Title: "T", Question: "Q", Stage: model.StageDone, CreatedAt: time.Now(), Papers: []*model.Paper{
		{ID: "a", Title: "A", Decision: model.DecisionKeep, Keywords: []string{"LLMs", "enterprise"}, Extract: &model.Extraction{Keywords: []string{"governance"}}},
		{ID: "b", Title: "B", Decision: model.DecisionKeep, Keywords: []string{"large language model", "governance"}},
		{ID: "c", Title: "C <script>", Decision: model.DecisionDrop, Keywords: []string{"dropped only"}},
	}}
	_ = s.store.Put(p)
	client := &http.Client{}
	get := func(path string) (int, string) {
		resp, err := client.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	code, body := get("/projects/kw1/keywords")
	if code != 200 || !strings.Contains(body, "large language model") || strings.Contains(body, "dropped only") || !strings.Contains(body, "vis-network") {
		t.Fatalf("keywords page: %d\n%s", code, body[:1500])
	}
	if code, body := get("/projects/kw1/keywords?scope=all&min=1"); code != 200 || !strings.Contains(body, "dropped only") {
		t.Fatalf("scope=all: %d", code)
	}
	if code, body := get("/projects/kw1/keywords/vosviewer-map.txt"); code != 200 || !strings.HasPrefix(body, "id\tlabel\tcluster") || !strings.Contains(body, "governance") {
		t.Fatalf("map export: %d %s", code, body)
	}
	if code, body := get("/projects/kw1/keywords/vosviewer-network.txt"); code != 200 || !strings.Contains(body, "\t") {
		t.Fatalf("network export: %d %s", code, body)
	}
	if code, body := get("/projects/kw1/keywords/keywords.csv"); code != 200 || !strings.HasPrefix(body, "keyword,papers") {
		t.Fatalf("csv export: %d %s", code, body)
	}
}

func TestZoteroDesktopSend(t *testing.T) {
	var created []map[string]any
	var collections []string
	var uploaded, registered int
	authorized := false
	fakeZ := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Zotero-Server-ID", "SRV1")
		b, _ := io.ReadAll(r.Body)
		isWrite := r.Method == "POST"
		if isWrite && !strings.HasPrefix(r.URL.Path, "/api/local/") && !strings.HasPrefix(r.URL.Path, "/connector/") {
			if r.Header.Get("Zotero-Server-ID") != "SRV1" {
				w.WriteHeader(428)
				return
			}
			if r.Header.Get("Zotero-API-Key") != "LOCALKEY" {
				w.WriteHeader(401)
				return
			}
		}
		switch {
		case r.URL.Path == "/connector/ping":
			w.WriteHeader(200)
		case r.URL.Path == "/connector/getSelectedCollection":
			w.Write([]byte(`{"id":1,"name":"My Library","targets":[{"id":"L1","name":"My Library","level":0},{"id":"C7","name":"Review X","level":1}]}`))
		case r.URL.Path == "/api/":
			w.WriteHeader(200)
		case r.URL.Path == "/api/local/authorize":
			authorized = true
			w.Write([]byte(`{"key":"LOCALKEY","remember":true}`))
		case r.URL.Path == "/api/users/0/collections" && r.Method == "GET":
			w.Write([]byte(`[{"key":"CX","data":{"name":"Review X"}}]`))
		case strings.HasSuffix(r.URL.Path, "/items/top"):
			w.Write([]byte(`[]`))
		case strings.HasSuffix(r.URL.Path, "/children"):
			w.Write([]byte(`[]`))
		case r.URL.Path == "/api/users/0/collections":
			var cs []map[string]any
			_ = json.Unmarshal(b, &cs)
			collections = append(collections, cs[0]["name"].(string))
			w.Write([]byte(`{"success":{"0":"NEWCOL"},"failed":{}}`))
		case r.URL.Path == "/api/users/0/items":
			var items []map[string]any
			_ = json.Unmarshal(b, &items)
			succ := map[string]string{}
			for i, it := range items {
				created = append(created, it)
				succ[string(rune('0'+i))] = "K" + string(rune('A'+len(created)))
			}
			out, _ := json.Marshal(map[string]any{"success": succ, "failed": map[string]any{}})
			w.Write(out)
		case strings.HasSuffix(r.URL.Path, "/file") && strings.HasPrefix(string(b), "md5="):
			w.Write([]byte(`{"url":"http://` + r.Host + `/api/local/uploads/UK1","contentType":"application/octet-stream","prefix":"","suffix":"","uploadKey":"UK1"}`))
		case r.URL.Path == "/api/local/uploads/UK1":
			uploaded++
			w.WriteHeader(201)
		case strings.HasSuffix(r.URL.Path, "/file") && strings.HasPrefix(string(b), "upload="):
			registered++
			w.WriteHeader(204)
		default:
			w.WriteHeader(404)
		}
	}))
	defer fakeZ.Close()
	st, _ := store.Open(t.TempDir())
	cfg := config.FromEnv()
	cfg.AIBackend, cfg.AnthropicAPIKey, cfg.BrowserFetch = "api", "", "off"
	cfg.ZoteroLocal = fakeZ.URL
	s, _ := New(cfg, st)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	pdf := filepath.Join(t.TempDir(), "a.pdf")
	_ = os.WriteFile(pdf, []byte("%PDF-1.4 x"), 0o644)
	_ = s.store.Put(&model.Project{ID: "z1", Title: "Zed", Question: "Q", Stage: model.StageDone, CreatedAt: time.Now(), Papers: []*model.Paper{
		{ID: "a", Title: "Kept with PDF", Authors: []string{"Ann Author"}, Year: 2024, Venue: "MISQ", DOI: "10.1/a", Decision: model.DecisionKeep, Download: model.DownloadResult{Status: model.DownloadOK, Path: pdf}, Extract: &model.Extraction{Objective: "obj"}},
		{ID: "b", Title: "Kept no PDF", Decision: model.DecisionKeep},
		{ID: "c", Title: "Dropped", Decision: model.DecisionDrop},
	}})
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, _ := client.Get(ts.URL + "/projects/z1/zotero")
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(b), "Review X") || !strings.Contains(string(b), ">running<") {
		t.Fatalf("zotero page: %d\n%s", resp.StatusCode, b[:1200])
	}
	resp, _ = client.PostForm(ts.URL+"/projects/z1/zotero/send", url.Values{"mode": {"desktop"}, "collection": {"litreview: Zed"}})
	resp.Body.Close()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var running bool
		_ = s.store.View("z1", func(p *model.Project) { running = p.Job != nil && p.Job.Running })
		if !running {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	p, _ := s.store.Get("z1")
	if p.Job.Error != "" {
		t.Fatalf("send: %s\n%s", p.Job.Error, strings.Join(p.Job.Log, "\n"))
	}
	if !authorized || len(collections) != 1 || collections[0] != "litreview: Zed" {
		t.Fatalf("collection creation: authorized=%v cols=%v", authorized, collections)
	}
	// 2 items + 1 note + 1 attachment item
	if len(created) != 4 || created[0]["title"] != "Kept with PDF" || created[2]["itemType"] != "note" || created[3]["itemType"] != "attachment" || uploaded != 1 || registered != 1 {
		t.Fatalf("items: n=%d uploaded=%d registered=%d\n%v", len(created), uploaded, registered, created)
	}
	if cols, _ := created[0]["collections"].([]any); len(cols) != 1 || cols[0] != "NEWCOL" {
		t.Fatalf("item not in new collection: %v", created[0]["collections"])
	}
	if s.loadZotero().LocalKey != "LOCALKEY" {
		t.Fatal("permanent local key not saved")
	}
	if !strings.Contains(strings.Join(p.Job.Log, "\n"), "Always Allow") {
		t.Fatal("log should instruct the user about the dialog")
	}
}

func TestPrismaPage(t *testing.T) {
	s, ts := newTestServer(t)
	_ = s.store.Put(&model.Project{ID: "pr1", Title: "P", Question: "Q", Stage: model.StageDone, CreatedAt: time.Now(),
		Queries: []model.Query{{Source: "openalex", Query: "x", Results: 10}},
		Papers:  []*model.Paper{{Decision: model.DecisionKeep, Download: model.DownloadResult{Status: model.DownloadOK}}, {Decision: model.DecisionDrop}}})
	resp, _ := http.Get(ts.URL + "/projects/pr1/prisma")
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(b), "<svg") || !strings.Contains(string(b), "Studies included in review (n = 1)") || !strings.Contains(string(b), "Methods paragraph") {
		t.Fatalf("prisma page: %d\n%s", resp.StatusCode, b[:1500])
	}
	resp, _ = http.Get(ts.URL + "/projects/pr1/prisma.svg")
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(string(b), "<svg") || resp.Header.Get("Content-Type") != "image/svg+xml" {
		t.Fatalf("svg export: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
}

func TestVenueRating(t *testing.T) {
	fakeJufo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/etsi.php" && r.URL.Query().Get("issn") == "0276-7783":
			w.Write([]byte(`[{"Jufo_ID":"63427","Name":"Mis quarterly"}]`))
		case r.URL.Path == "/etsi.php":
			w.Write([]byte(`[{"Jufo_ID":"77","Name":"Power system technology"}]`))
		case r.URL.Path == "/kanava/63427":
			w.Write([]byte(`[{"Name":"Mis quarterly","Level":"3"}]`))
		case r.URL.Path == "/kanava/77":
			w.Write([]byte(`[{"Name":"Power system technology","Level":"0"}]`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer fakeJufo.Close()
	st, _ := store.Open(t.TempDir())
	cfg := config.FromEnv()
	cfg.AIBackend, cfg.AnthropicAPIKey, cfg.BrowserFetch = "api", "", "off"
	cfg.JufoBase = fakeJufo.URL
	fakeOA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/sources/issn:0276-7783" {
			w.Write([]byte(`{"id":"S1","display_name":"MIS Quarterly","type":"journal","is_core":true,"summary_stats":{"h_index":300}}`))
			return
		}
		w.Write([]byte(`{"results":[]}`))
	}))
	defer fakeOA.Close()
	cfg.OpenAlexBase = fakeOA.URL
	fakeCORE := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("2,International Conference on Information Systems,ICIS,CORE2023,A,Yes,2,,\n"))
	}))
	defer fakeCORE.Close()
	cfg.COREURL = fakeCORE.URL
	fakeCR := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"message":{"items":[]}}`)) }))
	defer fakeCR.Close()
	cfg.CrossRefBase = fakeCR.URL
	s, _ := New(cfg, st)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	_ = st.Put(&model.Project{ID: "v1", Title: "V", Question: "Q", Stage: model.StageScreening, CreatedAt: time.Now(), Papers: []*model.Paper{
		{ID: "a", Title: "Top", Venue: "MIS Quarterly", ISSN: "0276-7783", Decision: model.DecisionKeep},
		{ID: "b", Title: "Weak", Venue: "Power System Technology", Decision: model.DecisionKeep},
		{ID: "c", Title: "Conf", Venue: "Proceedings of the International Conference on Information Systems (ICIS 2023)", Decision: model.DecisionKeep},
		{ID: "d", Title: "Dropped", Venue: "MIS Quarterly", Decision: model.DecisionDrop},
	}})
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	r, _ := client.PostForm(ts.URL+"/projects/v1/venues", url.Values{"scope": {"kept"}})
	r.Body.Close()
	p := waitIdle(t, st, "v1")
	if p.Job.Error != "" {
		t.Fatal(p.Job.Error)
	}
	if a := p.Paper("a").VenueRating; a == nil || a.JufoLevel != 3 || a.Basket != "AIS basket of 8" || !a.OACore || a.HIndex != 300 || a.Tier != "top" {
		t.Fatalf("a: %+v", a)
	}
	if b := p.Paper("b").VenueRating; b == nil || b.JufoLevel != 0 || b.Tier != "below" {
		t.Fatalf("b: %+v", b)
	}
	if c := p.Paper("c").VenueRating; c == nil || c.ISConf != "ICIS" || c.CORERank != "A" || c.Tier != "good" {
		t.Fatalf("c: %+v", c)
	}
	if p.Paper("d").VenueRating != nil {
		t.Fatal("dropped paper should not be rated in kept scope")
	}
	rr, _ := client.Get(ts.URL + "/projects/v1?view=screening")
	b, _ := io.ReadAll(rr.Body)
	rr.Body.Close()
	html := string(b)
	if !strings.Contains(html, "JUFO 3") || !strings.Contains(html, "AIS basket of 8") || !strings.Contains(html, "CORE A") || !strings.Contains(html, "Below bar (1)") || !strings.Contains(html, "Drop below-bar venues") {
		t.Fatalf("screening view lacks venue badges/controls:\n%s", html[:3000])
	}
	rr, _ = client.Get(ts.URL + "/projects/v1?view=screening&filter=quality")
	b, _ = io.ReadAll(rr.Body)
	rr.Body.Close()
	if strings.Contains(string(b), ">Weak<") || !strings.Contains(string(b), ">Top<") {
		t.Fatal("quality filter wrong")
	}
	r, _ = client.PostForm(ts.URL+"/projects/v1/bulk", url.Values{"action": {"drop-jufo0"}})
	r.Body.Close()
	p, _ = st.Get("v1")
	if p.Paper("b").Decision != model.DecisionDrop || p.Paper("a").Decision != model.DecisionKeep {
		t.Fatal("drop-jufo0 wrong")
	}
	_ = st.Update("v1", func(p *model.Project) error { p.Stage = model.StageDone; return nil })
	rr, _ = client.Get(ts.URL + "/projects/v1/table.csv")
	b, _ = io.ReadAll(rr.Body)
	rr.Body.Close()
	if !strings.Contains(string(b), "Venue tier,JUFO level,Norway level,CORE rank,SJR quartile,Venue flags") || !strings.Contains(string(b), "AIS basket of 8") {
		t.Fatalf("csv venue columns: %s", b)
	}
}

func TestUndoRedoButtons(t *testing.T) {
	s, ts := newTestServer(t)
	_ = s.store.Put(&model.Project{ID: "un1", Title: "U", Question: "Q", Stage: model.StageScreening, CreatedAt: time.Now(), Papers: []*model.Paper{
		{ID: "a", Title: "Alpha", AIVerdict: model.VerdictKeep, AIScore: 80},
		{ID: "b", Title: "Beta", AIVerdict: model.VerdictDrop, AIScore: 10},
	}})
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	r, _ := client.PostForm(ts.URL+"/projects/un1/bulk", url.Values{"action": {"accept-ai"}})
	r.Body.Close()
	p, _ := s.store.Get("un1")
	if p.Paper("a").Decision != model.DecisionKeep || p.Paper("b").Decision != model.DecisionDrop {
		t.Fatal("bulk did not apply")
	}
	rr, _ := client.Get(ts.URL + "/projects/un1?view=screening")
	b, _ := io.ReadAll(rr.Body)
	rr.Body.Close()
	if !strings.Contains(string(b), "Undo: Accept AI suggestions") {
		t.Fatalf("undo button missing:\n%s", b[:1500])
	}
	r, _ = client.PostForm(ts.URL+"/projects/un1/undo", url.Values{"view": {"screening"}})
	r.Body.Close()
	if !strings.Contains(r.Header.Get("Location"), "Undone") {
		t.Fatalf("undo redirect: %s", r.Header.Get("Location"))
	}
	p, _ = s.store.Get("un1")
	if p.Paper("a").Decision != model.DecisionUndecided || p.Paper("b").Decision != model.DecisionUndecided {
		t.Fatal("undo did not restore")
	}
	rr, _ = client.Get(ts.URL + "/projects/un1?view=screening")
	b, _ = io.ReadAll(rr.Body)
	rr.Body.Close()
	if !strings.Contains(string(b), "Redo: Accept AI suggestions") {
		t.Fatal("redo button missing")
	}
	r, _ = client.PostForm(ts.URL+"/projects/un1/redo", url.Values{"view": {"screening"}})
	r.Body.Close()
	p, _ = s.store.Get("un1")
	if p.Paper("a").Decision != model.DecisionKeep {
		t.Fatal("redo did not re-apply")
	}
	// A single toggle is undoable too, with the paper named.
	r, _ = client.PostForm(ts.URL+"/projects/un1/papers/b/decision", url.Values{"decision": {"keep"}})
	r.Body.Close()
	if u, _ := s.store.Labels("un1"); !strings.Contains(u, "keep: Beta") {
		t.Fatalf("label %q", u)
	}
}

func TestLaTeXTableExports(t *testing.T) {
	s, ts := newTestServer(t)
	_ = s.store.Put(&model.Project{ID: "lt1", Title: "L & T", Question: "Q", Stage: model.StageDone, CreatedAt: time.Now(),
		Concepts: []model.Concept{{Name: "Governance"}, {Name: "Trust & Ethics"}},
		Papers: []*model.Paper{
			{ID: "a", Title: "Alpha", Authors: []string{"Ann Author"}, Year: 2024, Venue: "MISQ", Decision: model.DecisionKeep, Extract: &model.Extraction{Method: "survey n=100", Context: "Finland", Findings: "50% & more", Theory: "TOE"}, Concepts: []model.ConceptHit{{Name: "Governance"}}},
			{ID: "b", Title: "Beta", Year: 2023, Decision: model.DecisionKeep},
			{ID: "c", Title: "Dropped", Decision: model.DecisionDrop},
		}})
	resp, _ := http.Get(ts.URL + "/projects/lt1/table.tex")
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	tex := string(b)
	if resp.StatusCode != 200 || !strings.Contains(tex, `\begin{longtable}`) || !strings.Contains(tex, `\citet{author2024alpha}`) || !strings.Contains(tex, `50\% \& more`) || !strings.Contains(tex, `\textbf{Key findings}`) || strings.Contains(tex, "Dropped") {
		t.Fatalf("table.tex:\n%s", tex)
	}
	if strings.Count(tex, `\citet{`) != 2 {
		t.Fatal("expected one row per kept paper")
	}
	resp, _ = http.Get(ts.URL + "/projects/lt1/table.tex?cols=study,year,venue,bogus")
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), `\textbf{Year}`) || strings.Contains(string(b), "Key findings") {
		t.Fatalf("custom columns: %s", b)
	}
	resp, _ = http.Get(ts.URL + "/projects/lt1/concepts.tex")
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	ctex := string(b)
	if resp.StatusCode != 200 || !strings.Contains(ctex, `\rotatebox{90}{\textbf{Trust \& Ethics}}`) || !strings.Contains(ctex, `\citet{author2024alpha} & $\bullet$ &  \\`) {
		t.Fatalf("concepts.tex:\n%s", ctex)
	}
}

func TestBibtexWithoutAuthors(t *testing.T) {
	b := bibtex(&model.Paper{Title: "Business Analytics Report", Year: 2025, Venue: "Gartner Research", BibKey: "unknown2025business"})
	if !strings.Contains(b, "key = {unknown2025business}") || !strings.Contains(b, "author = {{Gartner Research}}") {
		t.Fatalf("bibtex without authors:\n%s", b)
	}
}
