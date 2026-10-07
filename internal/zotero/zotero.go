// Package zotero sends included papers (metadata, notes and PDFs) to Zotero,
// either to the running Zotero desktop through its connector API or to a
// Zotero library through the Web API.
package zotero

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"literaturereview/internal/model"
)

var debug = os.Getenv("ZOTERO_DEBUG") != ""

// Item is a Zotero item in the translator/Web-API JSON shape.
type Item struct {
	ItemType         string       `json:"itemType"`
	Title            string       `json:"title"`
	Creators         []Creator    `json:"creators,omitempty"`
	AbstractNote     string       `json:"abstractNote,omitempty"`
	PublicationTitle string       `json:"publicationTitle,omitempty"`
	ProceedingsTitle string       `json:"proceedingsTitle,omitempty"`
	BookTitle        string       `json:"bookTitle,omitempty"`
	Date             string       `json:"date,omitempty"`
	DOI              string       `json:"DOI,omitempty"`
	URL              string       `json:"url,omitempty"`
	Language         string       `json:"language,omitempty"`
	Extra            string       `json:"extra,omitempty"`
	Tags             []Tag        `json:"tags,omitempty"`
	Notes            []Note       `json:"notes,omitempty"`       // connector only
	Attachments      []Attachment `json:"attachments,omitempty"` // connector only
	Collections      []string     `json:"collections,omitempty"` // web API only
}

type Creator struct {
	CreatorType string `json:"creatorType"`
	FirstName   string `json:"firstName,omitempty"`
	LastName    string `json:"lastName,omitempty"`
	Name        string `json:"name,omitempty"`
}
type Tag struct {
	Tag string `json:"tag"`
}
type Note struct {
	Note string `json:"note"`
}
type Attachment struct {
	Title    string `json:"title"`
	URL      string `json:"url,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
	Snapshot bool   `json:"snapshot"`
}

// ItemFor converts a paper. pdfURL (when non-empty) becomes an attachment the
// Zotero desktop downloads; the Web API path uploads the file instead.
func ItemFor(p *model.Paper, projectTitle, pdfURL string) Item {
	it := Item{ItemType: itemType(p), Title: p.Title, AbstractNote: p.Abstract, DOI: p.DOI, URL: p.URL, Language: p.Language}
	if p.Year > 0 {
		it.Date = fmt.Sprint(p.Year)
	}
	switch it.ItemType {
	case "conferencePaper":
		it.ProceedingsTitle = p.Venue
	case "bookSection":
		it.BookTitle = p.Venue
	default:
		it.PublicationTitle = p.Venue
	}
	for _, a := range p.Authors {
		it.Creators = append(it.Creators, creator(a))
	}
	it.Tags = []Tag{{"litreview"}, {"litreview: " + truncate(projectTitle, 60)}}
	if p.Extract != nil {
		for _, k := range p.Extract.Keywords {
			if k = strings.TrimSpace(k); k != "" {
				it.Tags = append(it.Tags, Tag{k})
			}
		}
	}
	var extra []string
	if p.BibKey != "" {
		extra = append(extra, "Citation Key: "+p.BibKey)
	}
	if p.AIScore > 0 || p.HasJev() {
		extra = append(extra, fmt.Sprintf("litreview screening: Claude %s %d/100%s", p.AIVerdict, p.AIScore, jevNote(p)))
	}
	it.Extra = strings.Join(extra, "\n")
	if n := noteFor(p); n != "" {
		it.Notes = []Note{{Note: n}}
	}
	if pdfURL != "" {
		it.Attachments = []Attachment{{Title: "Full Text PDF", URL: pdfURL, MimeType: "application/pdf"}}
	}
	return it
}

func jevNote(p *model.Paper) string {
	if !p.HasJev() {
		return ""
	}
	return fmt.Sprintf(", Jev %s %d/100", p.Jev.Verdict, p.Jev.Score)
}

// noteFor renders the extracted aspects as an HTML note.
func noteFor(p *model.Paper) string {
	e := p.Extract
	if e == nil {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("<h2>litreview extraction</h2>")
	for _, kv := range [][2]string{{"Objective", e.Objective}, {"Method", e.Method}, {"Context & data", e.Context}, {"Key findings", e.Findings}, {"Theory", e.Theory}, {"Contribution", e.Contribution}, {"Limitations", e.Limitations}, {"Relevance to RQ", e.Relevance}} {
		if strings.TrimSpace(kv[1]) != "" {
			fmt.Fprintf(&sb, "<p><b>%s:</b> %s</p>", kv[0], htmlEsc(kv[1]))
		}
	}
	if p.AIRationale != "" {
		fmt.Fprintf(&sb, "<p><i>Screening rationale: %s</i></p>", htmlEsc(p.AIRationale))
	}
	return sb.String()
}

func htmlEsc(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

func itemType(p *model.Paper) string {
	v := strings.ToLower(p.Venue)
	switch {
	case strings.Contains(v, "proceedings") || strings.Contains(v, "conference") || strings.Contains(v, "workshop") || strings.Contains(v, "symposium") || strings.HasPrefix(p.DOI, "10.5220/") || strings.HasPrefix(p.DOI, "10.24251/"):
		return "conferencePaper"
	case strings.HasPrefix(p.DOI, "10.1007/978") || strings.HasPrefix(p.DOI, "10.1007/979") || strings.Contains(v, "chapter") || strings.HasPrefix(p.DOI, "10.1142/978") || strings.Contains(v, "handbook"):
		return "bookSection"
	case p.Preprint:
		return "preprint"
	}
	return "journalArticle"
}

func creator(name string) Creator {
	name = strings.TrimSpace(name)
	if i := strings.Index(name, ","); i > 0 {
		return Creator{CreatorType: "author", LastName: strings.TrimSpace(name[:i]), FirstName: strings.TrimSpace(name[i+1:])}
	}
	parts := strings.Fields(name)
	if len(parts) < 2 {
		return Creator{CreatorType: "author", Name: name}
	}
	return Creator{CreatorType: "author", FirstName: strings.Join(parts[:len(parts)-1], " "), LastName: parts[len(parts)-1]}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ---------------------------------------------------------------------------
// Local Zotero desktop (connector API)

// Local talks to the connector server of a running Zotero desktop.
type Local struct {
	Base string // default http://127.0.0.1:23119
	HTTP *http.Client
}

func NewLocal(base string) *Local {
	if base == "" {
		base = "http://127.0.0.1:23119"
	}
	return &Local{Base: base, HTTP: &http.Client{Timeout: 10 * time.Minute}}
}

// Target is a library or collection Zotero can save into.
type Target struct {
	ID    string `json:"id"` // "L1" or "C12"
	Name  string `json:"name"`
	Level int    `json:"level"`
}

func (l *Local) post(ctx context.Context, path string, body any, out any) (int, error) {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.Base+path, bytes.NewReader(b))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Zotero-Connector-API-Version", "3")
	req.Header.Set("X-Zotero-Version", "5.0.0")
	resp, err := l.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if out != nil && len(data) > 0 {
		_ = json.Unmarshal(data, out)
	}
	if resp.StatusCode >= 400 {
		return resp.StatusCode, fmt.Errorf("zotero %s: HTTP %d %s", path, resp.StatusCode, truncate(strings.TrimSpace(string(data)), 200))
	}
	return resp.StatusCode, nil
}

// Ping reports whether Zotero desktop is running.
func (l *Local) Ping(ctx context.Context) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.Base+"/connector/ping", nil)
	if err != nil {
		return false
	}
	req.Header.Set("X-Zotero-Connector-API-Version", "3")
	req.Header.Set("X-Zotero-Version", "5.0.0")
	resp, err := l.HTTP.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == 200
}

// Targets lists the libraries and collections of the running Zotero, plus
// the currently selected target.
func (l *Local) Targets(ctx context.Context) ([]Target, string, error) {
	var out struct {
		ID      int      `json:"id"`
		Name    string   `json:"name"`
		Targets []Target `json:"targets"`
	}
	if _, err := l.post(ctx, "/connector/getSelectedCollection", map[string]any{}, &out); err != nil {
		return nil, "", err
	}
	return out.Targets, out.Name, nil
}

// FindCollection returns the id of a collection with the given name.
func FindCollection(targets []Target, name string) (string, bool) {
	n := strings.ToLower(strings.TrimSpace(name))
	for _, t := range targets {
		if strings.HasPrefix(t.ID, "C") && strings.ToLower(strings.TrimSpace(t.Name)) == n {
			return t.ID, true
		}
	}
	return "", false
}

// Save sends items to Zotero desktop into the target ("C12" or "L1").
// Zotero downloads the attachment URLs itself.
func (l *Local) Save(ctx context.Context, items []Item, target, uri string) error {
	var sid [8]byte
	_, _ = rand.Read(sid[:])
	session := hex.EncodeToString(sid[:])
	// Set the target first so the items land in the right collection.
	if _, err := l.post(ctx, "/connector/updateSession", map[string]any{"sessionID": session, "target": target, "tags": ""}, nil); err != nil && !strings.Contains(err.Error(), "400") {
		return err
	}
	code, err := l.post(ctx, "/connector/saveItems", map[string]any{"sessionID": session, "items": items, "uri": uri}, nil)
	if err != nil {
		return err
	}
	if code != 200 && code != 201 {
		return fmt.Errorf("zotero saveItems: HTTP %d", code)
	}
	// Re-apply the target: Zotero resolves the session's target after saving.
	_, _ = l.post(ctx, "/connector/updateSession", map[string]any{"sessionID": session, "target": target, "tags": ""}, nil)
	return nil
}

// ---------------------------------------------------------------------------
// Zotero Web API

// Web talks to api.zotero.org, or to the Zotero desktop's local API
// (http://127.0.0.1:23119/api), which implements the same protocol with two
// additions: a Zotero-Server-ID header echoed on writes and a local API key
// granted through an "Allow" dialog in Zotero.
type Web struct {
	Base     string
	UserID   string
	APIKey   string
	ServerID string // local API only
	HTTP     *http.Client

	lastFailure string // first per-object failure message of the last write
}

// NewLocalAPI returns a client for the running Zotero desktop's local API.
func NewLocalAPI(connectorBase, key string) *Web {
	if connectorBase == "" {
		connectorBase = "http://127.0.0.1:23119"
	}
	return &Web{Base: strings.TrimSuffix(connectorBase, "/") + "/api", UserID: "0", APIKey: key, HTTP: &http.Client{Timeout: 15 * time.Minute}}
}

// IsLocal reports whether this client targets the desktop's local API.
func (w *Web) IsLocal() bool {
	return strings.Contains(w.Base, "127.0.0.1") || strings.Contains(w.Base, "localhost")
}

// FetchServerID reads the desktop's server id (required on local writes).
func (w *Web) FetchServerID(ctx context.Context) error {
	code, _, hdr, err := w.do(ctx, http.MethodGet, "/", nil, nil)
	if err != nil {
		return err
	}
	if code != 200 {
		return fmt.Errorf("zotero local api: HTTP %d (is the local API enabled in Zotero settings → Advanced?)", code)
	}
	w.ServerID = hdr.Get("Zotero-Server-ID")
	if w.ServerID == "" {
		return errors.New("zotero local api: no Zotero-Server-ID header; this Zotero version does not support local writes")
	}
	return nil
}

// ErrUnauthorized is returned by writes when the local API key is missing,
// consumed or unknown; call AuthorizeLocal and retry.
var ErrUnauthorized = errors.New("zotero local api: authorisation required")

// AuthorizeLocal asks Zotero desktop for a local API key. Zotero shows a
// dialog (Allow / Always Allow / Deny) to the user; this call blocks until
// the user answers. remember is true for "Always Allow".
func (w *Web) AuthorizeLocal(ctx context.Context, appName string) (key string, remember bool, err error) {
	body, _ := json.Marshal(map[string]string{"appName": appName})
	code, data, _, err := w.do(ctx, http.MethodPost, "/local/authorize", body, map[string]string{"Content-Type": "application/json"})
	if err != nil {
		return "", false, err
	}
	switch code {
	case 200:
		var out struct {
			Key      string `json:"key"`
			Remember bool   `json:"remember"`
		}
		if err := json.Unmarshal(data, &out); err != nil || out.Key == "" {
			return "", false, fmt.Errorf("zotero local api: unexpected authorize response: %s", truncate(string(data), 200))
		}
		w.APIKey = out.Key
		return out.Key, out.Remember, nil
	case 403:
		return "", false, errors.New("access was denied in the Zotero dialog")
	case 429:
		return "", false, errors.New("too many authorisation requests; wait a minute and try again")
	default:
		return "", false, fmt.Errorf("zotero local api authorize: HTTP %d %s", code, truncate(string(data), 200))
	}
}

// ListCollections returns top-level collections (key → name).
func (w *Web) ListCollections(ctx context.Context) (map[string]string, error) {
	code, data, _, err := w.do(ctx, http.MethodGet, "/users/"+w.UserID+"/collections?limit=100", nil, nil)
	if err != nil {
		return nil, err
	}
	if code != 200 {
		return nil, fmt.Errorf("zotero api list collections: HTTP %d", code)
	}
	var cs []struct {
		Key  string `json:"key"`
		Data struct {
			Name string `json:"name"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &cs); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, c := range cs {
		out[c.Key] = c.Data.Name
	}
	return out, nil
}

// CollectionItem is a top-level item already in a collection.
type CollectionItem struct {
	Key   string
	DOI   string
	Title string
}

// ListCollectionItems returns the top-level items of a collection.
func (w *Web) ListCollectionItems(ctx context.Context, collectionKey string) ([]CollectionItem, error) {
	var out []CollectionItem
	for start := 0; ; start += 100 {
		code, data, _, err := w.do(ctx, http.MethodGet, fmt.Sprintf("/users/%s/collections/%s/items/top?format=json&limit=100&start=%d", w.UserID, collectionKey, start), nil, nil)
		if err != nil {
			return nil, err
		}
		if code != 200 {
			return nil, fmt.Errorf("zotero api list items: HTTP %d", code)
		}
		var items []struct {
			Key  string `json:"key"`
			Data struct {
				DOI   string `json:"DOI"`
				Title string `json:"title"`
			} `json:"data"`
		}
		if err := json.Unmarshal(data, &items); err != nil {
			return nil, err
		}
		for _, it := range items {
			out = append(out, CollectionItem{Key: it.Key, DOI: strings.ToLower(it.Data.DOI), Title: it.Data.Title})
		}
		if len(items) < 100 {
			return out, nil
		}
	}
}

// ChildTypes returns the item types of an item's children (notes, attachments).
func (w *Web) ChildTypes(ctx context.Context, itemKey string) ([]string, error) {
	code, data, _, err := w.do(ctx, http.MethodGet, "/users/"+w.UserID+"/items/"+itemKey+"/children?format=json", nil, nil)
	if err != nil {
		return nil, err
	}
	if code != 200 {
		return nil, fmt.Errorf("zotero api children: HTTP %d", code)
	}
	var items []struct {
		Data struct {
			ItemType string `json:"itemType"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &items); err != nil {
		return nil, err
	}
	var out []string
	for _, it := range items {
		out = append(out, it.Data.ItemType)
	}
	return out, nil
}

// FindCollectionKey returns the key of a collection named name (case-insensitive).
func FindCollectionKey(cols map[string]string, name string) (string, bool) {
	n := strings.ToLower(strings.TrimSpace(name))
	for k, v := range cols {
		if strings.ToLower(strings.TrimSpace(v)) == n {
			return k, true
		}
	}
	return "", false
}

func NewWeb(userID, apiKey string) *Web {
	return &Web{Base: "https://api.zotero.org", UserID: userID, APIKey: apiKey, HTTP: &http.Client{Timeout: 5 * time.Minute}}
}

func (w *Web) do(ctx context.Context, method, path string, body []byte, hdr map[string]string) (int, []byte, http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, method, w.Base+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, nil, err
	}
	req.Header.Set("Zotero-API-Version", "3")
	if w.APIKey != "" {
		req.Header.Set("Zotero-API-Key", w.APIKey)
	}
	if w.ServerID != "" {
		req.Header.Set("Zotero-Server-ID", w.ServerID)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := w.HTTP.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if debug && method != http.MethodGet {
		log.Printf("[zotero] %s %s → %d %s", method, path, resp.StatusCode, truncate(strings.TrimSpace(string(data)), 400))
	}
	if resp.StatusCode == 401 && w.IsLocal() {
		return resp.StatusCode, data, resp.Header, ErrUnauthorized
	}
	return resp.StatusCode, data, resp.Header, nil
}

// Verify checks the key and returns the key's username.
func (w *Web) Verify(ctx context.Context) (string, error) {
	code, data, _, err := w.do(ctx, http.MethodGet, "/keys/current", nil, nil)
	if err != nil {
		return "", err
	}
	if code != 200 {
		return "", fmt.Errorf("zotero api: HTTP %d (check the API key)", code)
	}
	var out struct {
		Username string `json:"username"`
		UserID   int    `json:"userID"`
	}
	_ = json.Unmarshal(data, &out)
	if w.UserID == "" && out.UserID > 0 {
		w.UserID = fmt.Sprint(out.UserID)
	}
	return out.Username, nil
}

type writeResp struct {
	Successful map[string]struct {
		Key string `json:"key"`
	} `json:"successful"`
	Success map[string]string `json:"success"`
	Failed  map[string]struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"failed"`
}

// LastFailure returns the first per-object failure of the last write.
func (w *Web) LastFailure() string { return w.lastFailure }

// FirstFailure formats the first failure message, if any.
func (r writeResp) FirstFailure() string {
	for idx, f := range r.Failed {
		return fmt.Sprintf("index %s: HTTP %d %s", idx, f.Code, f.Message)
	}
	return ""
}

// CreateCollection creates a top-level collection and returns its key.
func (w *Web) CreateCollection(ctx context.Context, name string) (string, error) {
	body, _ := json.Marshal([]map[string]any{{"name": name, "parentCollection": false}})
	code, data, _, err := w.do(ctx, http.MethodPost, "/users/"+w.UserID+"/collections", body, map[string]string{"Content-Type": "application/json"})
	if err != nil {
		return "", err
	}
	if code != 200 {
		return "", fmt.Errorf("zotero api create collection: HTTP %d %s", code, truncate(string(data), 200))
	}
	var out writeResp
	_ = json.Unmarshal(data, &out)
	if k, ok := out.Success["0"]; ok {
		return k, nil
	}
	if s, ok := out.Successful["0"]; ok {
		return s.Key, nil
	}
	return "", fmt.Errorf("zotero api: collection not created: %s", truncate(string(data), 200))
}

// CreateItems creates items (max 50 per call) and returns their keys in order.
func (w *Web) CreateItems(ctx context.Context, items []any) ([]string, error) {
	var keys []string
	for start := 0; start < len(items); start += 50 {
		end := start + 50
		if end > len(items) {
			end = len(items)
		}
		body, _ := json.Marshal(items[start:end])
		code, data, _, err := w.do(ctx, http.MethodPost, "/users/"+w.UserID+"/items", body, map[string]string{"Content-Type": "application/json"})
		if err != nil {
			return keys, err
		}
		if code != 200 {
			return keys, fmt.Errorf("zotero api create items: HTTP %d %s", code, truncate(string(data), 300))
		}
		var out writeResp
		if err := json.Unmarshal(data, &out); err != nil {
			return keys, fmt.Errorf("zotero api create items: unexpected response: %s", truncate(string(data), 200))
		}
		w.lastFailure = out.FirstFailure()
		for i := 0; i < end-start; i++ {
			idx := fmt.Sprint(i)
			switch {
			case out.Success[idx] != "":
				keys = append(keys, out.Success[idx])
			case out.Successful[idx].Key != "":
				keys = append(keys, out.Successful[idx].Key)
			default:
				keys = append(keys, "")
			}
		}
	}
	return keys, nil
}

// UploadPDF attaches a local PDF to a parent item: create the attachment
// item, request upload authorisation, upload, register.
func (w *Web) UploadPDF(ctx context.Context, parentKey, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	filename := filepath.Base(path)
	// Field order matters to Zotero's local API (linkMode must precede the
	// filename/path), so use a struct rather than a map.
	type attachmentItem struct {
		ItemType    string `json:"itemType"`
		LinkMode    string `json:"linkMode"`
		ParentItem  string `json:"parentItem"`
		Title       string `json:"title"`
		ContentType string `json:"contentType"`
		Filename    string `json:"filename"`
		Tags        []Tag  `json:"tags"`
	}
	keys, err := w.CreateItems(ctx, []any{attachmentItem{ItemType: "attachment", LinkMode: "imported_file", ParentItem: parentKey, Title: "Full Text PDF", ContentType: "application/pdf", Filename: filename, Tags: []Tag{}}})
	if err != nil {
		return err
	}
	if len(keys) == 0 || keys[0] == "" {
		return fmt.Errorf("zotero api: attachment item not created (%s)", w.lastFailure)
	}
	key := keys[0]
	sum := md5.Sum(data)
	form := fmt.Sprintf("md5=%s&filename=%s&filesize=%d&mtime=%d", hex.EncodeToString(sum[:]), filename, len(data), time.Now().UnixMilli())
	code, body, _, err := w.do(ctx, http.MethodPost, "/users/"+w.UserID+"/items/"+key+"/file", []byte(form), map[string]string{"Content-Type": "application/x-www-form-urlencoded", "If-None-Match": "*"})
	if err != nil {
		return err
	}
	if code != 200 {
		return fmt.Errorf("zotero api upload auth: HTTP %d %s", code, truncate(string(body), 200))
	}
	var auth struct {
		Exists      int    `json:"exists"`
		URL         string `json:"url"`
		ContentType string `json:"contentType"`
		Prefix      string `json:"prefix"`
		Suffix      string `json:"suffix"`
		UploadKey   string `json:"uploadKey"`
	}
	if err := json.Unmarshal(body, &auth); err != nil {
		return err
	}
	if auth.Exists == 1 {
		return nil
	}
	var payload bytes.Buffer
	payload.WriteString(auth.Prefix)
	payload.Write(data)
	payload.WriteString(auth.Suffix)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, auth.URL, &payload)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", auth.ContentType)
	resp, err := w.HTTP.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("zotero storage upload: HTTP %d", resp.StatusCode)
	}
	code, body, _, err = w.do(ctx, http.MethodPost, "/users/"+w.UserID+"/items/"+key+"/file", []byte("upload="+auth.UploadKey), map[string]string{"Content-Type": "application/x-www-form-urlencoded", "If-None-Match": "*"})
	if err != nil {
		return err
	}
	if code != 204 {
		return fmt.Errorf("zotero api upload register: HTTP %d %s", code, truncate(string(body), 200))
	}
	return nil
}

// WebItem converts an Item to the Web API shape (notes become child items,
// attachments are uploaded separately).
func WebItem(it Item, collectionKey string) map[string]any {
	m := map[string]any{"itemType": it.ItemType, "title": it.Title, "creators": it.Creators, "abstractNote": it.AbstractNote, "date": it.Date, "DOI": it.DOI, "url": it.URL, "language": it.Language, "extra": it.Extra, "tags": it.Tags}
	switch it.ItemType {
	case "conferencePaper":
		m["proceedingsTitle"] = it.ProceedingsTitle
	case "bookSection":
		m["bookTitle"] = it.BookTitle
	case "preprint":
		m["repository"] = it.PublicationTitle
	default:
		m["publicationTitle"] = it.PublicationTitle
	}
	if collectionKey != "" {
		m["collections"] = []string{collectionKey}
	}
	// A nil slice would serialise as null, which Zotero rejects.
	if it.Creators == nil {
		m["creators"] = []Creator{}
	}
	if it.Tags == nil {
		m["tags"] = []Tag{}
	}
	return m
}
