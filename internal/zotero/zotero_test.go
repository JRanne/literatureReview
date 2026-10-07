package zotero

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"literaturereview/internal/model"
)

func TestItemFor(t *testing.T) {
	p := &model.Paper{Title: "T", Authors: []string{"Maija Virtanen", "Smith, John", "Collective"}, Year: 2024, Venue: "Proceedings of ECIS", DOI: "10.1/x", Abstract: "A", BibKey: "virtanen2024t", AIScore: 80, AIVerdict: model.VerdictKeep,
		Extract: &model.Extraction{Objective: "obj", Keywords: []string{"genai"}}}
	it := ItemFor(p, "My review", "http://localhost:8080/files/1/a.pdf")
	if it.ItemType != "conferencePaper" || it.ProceedingsTitle != "Proceedings of ECIS" || it.Date != "2024" || len(it.Creators) != 3 || it.Creators[0].LastName != "Virtanen" || it.Creators[1].FirstName != "John" || it.Creators[2].Name != "Collective" {
		t.Fatalf("item: %+v", it)
	}
	if !strings.Contains(it.Extra, "Citation Key: virtanen2024t") || len(it.Tags) != 3 || it.Attachments[0].URL == "" || !strings.Contains(it.Notes[0].Note, "obj") {
		t.Fatalf("item extras: %+v", it)
	}
	if itemType(&model.Paper{DOI: "10.1007/978-3-1_2"}) != "bookSection" || itemType(&model.Paper{Venue: "MIS Quarterly"}) != "journalArticle" {
		t.Fatal("itemType")
	}
}

func TestWebItemNoAuthors(t *testing.T) {
	b, _ := json.Marshal(WebItem(ItemFor(&model.Paper{Title: "No authors"}, "p", ""), "C1"))
	if strings.Contains(string(b), `"creators":null`) || !strings.Contains(string(b), `"creators":[]`) {
		t.Fatalf("creators must be an empty array: %s", b)
	}
}

func TestLocalSave(t *testing.T) {
	var saved struct {
		items   int
		target  string
		session string
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Zotero-Connector-API-Version") == "" {
			t.Errorf("missing connector headers")
		}
		var body map[string]any
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		switch r.URL.Path {
		case "/connector/ping":
			w.WriteHeader(200)
		case "/connector/getSelectedCollection":
			w.Write([]byte(`{"id":16,"name":"Research Methods","targets":[{"id":"L1","name":"My Library","level":0},{"id":"C4","name":"AI","level":1},{"id":"C9","name":"System 1 review","level":1}]}`))
		case "/connector/updateSession":
			saved.target, _ = body["target"].(string)
			saved.session, _ = body["sessionID"].(string)
			w.WriteHeader(200)
		case "/connector/saveItems":
			items, _ := body["items"].([]any)
			saved.items = len(items)
			if body["sessionID"] != saved.session {
				t.Errorf("session mismatch")
			}
			w.WriteHeader(201)
			w.Write([]byte(`{"items":[]}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	l := NewLocal(srv.URL)
	if !l.Ping(context.Background()) {
		t.Fatal("ping")
	}
	targets, cur, err := l.Targets(context.Background())
	if err != nil || cur != "Research Methods" || len(targets) != 3 {
		t.Fatalf("targets: %v %s %v", targets, cur, err)
	}
	id, ok := FindCollection(targets, " system 1 Review ")
	if !ok || id != "C9" {
		t.Fatal("find collection")
	}
	if _, ok := FindCollection(targets, "My Library"); ok {
		t.Fatal("library must not match as collection")
	}
	if err := l.Save(context.Background(), []Item{ItemFor(&model.Paper{Title: "a"}, "p", ""), ItemFor(&model.Paper{Title: "b"}, "p", "")}, "C9", "http://localhost:8080/"); err != nil {
		t.Fatal(err)
	}
	if saved.items != 2 || saved.target != "C9" {
		t.Fatalf("saved %+v", saved)
	}
}

func TestWebFlow(t *testing.T) {
	var uploaded []byte
	var registered bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Zotero-API-Key") != "k" && !strings.HasPrefix(r.URL.Path, "/storage") {
			t.Errorf("missing api key on %s", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		switch {
		case r.URL.Path == "/keys/current":
			w.Write([]byte(`{"username":"juuso","userID":12345}`))
		case r.URL.Path == "/users/12345/collections":
			w.Write([]byte(`{"successful":{"0":{"key":"COLL1"}},"success":{"0":"COLL1"},"failed":{}}`))
		case r.URL.Path == "/users/12345/items":
			if strings.Contains(string(b), `"attachment"`) && strings.Index(string(b), `"linkMode"`) > strings.Index(string(b), `"filename"`) {
				t.Errorf("linkMode must precede filename in attachment JSON: %s", b)
			}
			var items []map[string]any
			_ = json.Unmarshal(b, &items)
			succ := map[string]string{}
			for i := range items {
				succ[string(rune('0'+i))] = "ITEM" + string(rune('A'+i))
			}
			out, _ := json.Marshal(map[string]any{"success": succ, "failed": map[string]any{}})
			w.Write(out)
		case r.URL.Path == "/users/12345/items/ITEMA/file" && strings.HasPrefix(string(b), "md5="):
			w.Write([]byte(`{"url":"` + srvURL(r) + `/storage","contentType":"multipart/form-data; boundary=x","prefix":"PRE","suffix":"SUF","uploadKey":"UK"}`))
		case r.URL.Path == "/storage":
			uploaded = b
			w.WriteHeader(201)
		case r.URL.Path == "/users/12345/items/ITEMA/file" && strings.HasPrefix(string(b), "upload="):
			registered = true
			w.WriteHeader(204)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	wc := NewWeb("", "k")
	wc.Base = srv.URL
	if u, err := wc.Verify(context.Background()); err != nil || u != "juuso" || wc.UserID != "12345" {
		t.Fatalf("verify: %s %v", u, err)
	}
	key, err := wc.CreateCollection(context.Background(), "System 1 review")
	if err != nil || key != "COLL1" {
		t.Fatalf("collection: %s %v", key, err)
	}
	keys, err := wc.CreateItems(context.Background(), []any{WebItem(ItemFor(&model.Paper{Title: "a"}, "p", ""), key), WebItem(ItemFor(&model.Paper{Title: "b", Venue: "Proc. X"}, "p", ""), key)})
	if err != nil || len(keys) != 2 || keys[0] != "ITEMA" {
		t.Fatalf("items: %v %v", keys, err)
	}
	pdf := filepath.Join(t.TempDir(), "a.pdf")
	_ = os.WriteFile(pdf, []byte("%PDF-1.4 hello"), 0o644)
	if err := wc.UploadPDF(context.Background(), "ITEMA", pdf); err != nil {
		t.Fatal(err)
	}
	if string(uploaded) != "PRE%PDF-1.4 helloSUF" || !registered {
		t.Fatalf("upload: %q registered=%v", uploaded, registered)
	}
}

func srvURL(r *http.Request) string { return "http://" + r.Host }
