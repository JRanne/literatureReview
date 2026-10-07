package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"literaturereview/internal/model"
	"literaturereview/internal/zotero"
)

// zoteroSettings persists the Web API credentials (0600 file).
type zoteroSettings struct {
	UserID   string    `json:"user_id"`
	APIKey   string    `json:"api_key"`
	Username string    `json:"username,omitempty"`
	Verified time.Time `json:"verified_at,omitempty"`
	// LocalKey is the Zotero desktop local-API key granted via "Always Allow".
	LocalKey string `json:"local_key,omitempty"`
}

func (s *Server) zoteroPath() string { return filepath.Join(s.store.Dir(), "zotero.json") }

func (s *Server) loadZotero() zoteroSettings {
	var z zoteroSettings
	if b, err := os.ReadFile(s.zoteroPath()); err == nil {
		_ = json.Unmarshal(b, &z)
	}
	return z
}

func (s *Server) saveZotero(z zoteroSettings) error {
	b, _ := json.MarshalIndent(z, "", "  ")
	return os.WriteFile(s.zoteroPath(), b, 0o600)
}

type zoteroData struct {
	Base        baseData
	P           *model.Project
	Kept        int
	WithPDF     int
	Desktop     bool
	Targets     []zotero.Target
	Current     string
	Web         zoteroSettings
	WebKeySet   bool
	Error       string
	Message     string
	Running     bool
	LastLog     []string
	Collection  string
	DefaultName string
}

func (s *Server) handleZotero(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var d zoteroData
	if err := s.store.View(id, func(p *model.Project) {
		d = zoteroData{Base: s.base(), P: p, Running: p.Job != nil && p.Job.Running}
		for _, pp := range p.Kept() {
			d.Kept++
			if pp.Download.Status == model.DownloadOK {
				d.WithPDF++
			}
		}
		if p.Job != nil && p.Job.Name == "zotero" {
			d.LastLog = p.Job.Log
		}
		d.DefaultName = "litreview: " + p.Title
	}); err != nil {
		http.NotFound(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	loc := zotero.NewLocal(s.cfg.ZoteroLocal)
	if loc.Ping(ctx) {
		d.Desktop = true
		d.Targets, d.Current, _ = loc.Targets(ctx)
		if len(d.Targets) == 0 {
			if cols, err := zotero.NewLocalAPI(s.cfg.ZoteroLocal, "").ListCollections(ctx); err == nil {
				for k, n := range cols {
					d.Targets = append(d.Targets, zotero.Target{ID: k, Name: n, Level: 1})
				}
			}
		}
	}
	z := s.loadZotero()
	d.WebKeySet = z.APIKey != ""
	z.APIKey = ""
	d.Web = z
	d.Error = r.URL.Query().Get("error")
	d.Message = r.URL.Query().Get("msg")
	d.Collection = r.URL.Query().Get("collection")
	s.render(w, "zotero", d)
}

// handleZoteroKey stores/verifies Web API credentials.
func (s *Server) handleZoteroKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	z := s.loadZotero()
	if k := strings.TrimSpace(r.FormValue("api_key")); k != "" {
		z.APIKey = k
	}
	if u := strings.TrimSpace(r.FormValue("user_id")); u != "" {
		z.UserID = u
	}
	if r.FormValue("forget") == "1" {
		_ = os.Remove(s.zoteroPath())
		http.Redirect(w, r, "/projects/"+id+"/zotero?msg="+urlEsc("Zotero API key forgotten"), http.StatusSeeOther)
		return
	}
	if z.APIKey == "" {
		http.Redirect(w, r, "/projects/"+id+"/zotero?error="+urlEsc("paste an API key"), http.StatusSeeOther)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	wc := zotero.NewWeb(z.UserID, z.APIKey)
	name, err := wc.Verify(ctx)
	if err != nil {
		http.Redirect(w, r, "/projects/"+id+"/zotero?error="+urlEsc(err.Error()), http.StatusSeeOther)
		return
	}
	z.UserID, z.Username, z.Verified = wc.UserID, name, time.Now()
	_ = s.saveZotero(z)
	http.Redirect(w, r, "/projects/"+id+"/zotero?msg="+urlEsc("Zotero Web API key verified for user "+name+" (ID "+z.UserID+")"), http.StatusSeeOther)
}

// handleZoteroSend runs the export job.
func (s *Server) handleZoteroSend(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	mode := r.FormValue("mode") // desktop | web
	name := strings.TrimSpace(r.FormValue("collection"))
	if name == "" {
		http.Redirect(w, r, "/projects/"+id+"/zotero?error="+urlEsc("give the collection a name"), http.StatusSeeOther)
		return
	}
	var kept []*model.Paper
	var title string
	_ = s.store.Update(id, func(p *model.Project) error {
		p.EnsureBibKeys()
		title = p.Title
		for _, pp := range p.Kept() {
			c := *pp
			kept = append(kept, &c)
		}
		return nil
	})
	if len(kept) == 0 {
		http.Redirect(w, r, "/projects/"+id+"/zotero?error="+urlEsc("no included papers"), http.StatusSeeOther)
		return
	}
	_ = r.Host
	switch mode {
	case "desktop":
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		running := zotero.NewLocal(s.cfg.ZoteroLocal).Ping(ctx)
		cancel()
		if !running {
			http.Redirect(w, r, "/projects/"+id+"/zotero?error="+urlEsc("Zotero desktop is not running (or port 23119 is not reachable)"), http.StatusSeeOther)
			return
		}
		err := s.runJob(id, "zotero", func(ctx context.Context, lg jobLogger) error {
			return s.zoteroLocalExport(ctx, id, title, name, kept, lg)
		})
		if err != nil {
			http.Redirect(w, r, "/projects/"+id+"/zotero?error="+urlEsc(err.Error()), http.StatusSeeOther)
			return
		}
	case "web":
		z := s.loadZotero()
		if z.APIKey == "" || z.UserID == "" {
			http.Redirect(w, r, "/projects/"+id+"/zotero?error="+urlEsc("verify a Zotero Web API key first"), http.StatusSeeOther)
			return
		}
		wc := zotero.NewWeb(z.UserID, z.APIKey)
		err := s.runJob(id, "zotero", func(ctx context.Context, lg jobLogger) error {
			lg.phase(1, 2, "Creating collection and items", len(kept))
			key, err := wc.CreateCollection(ctx, name)
			if err != nil {
				return err
			}
			lg.log("Created collection %q (%s) in library %s.", name, key, z.UserID)
			var payload []any
			for _, pp := range kept {
				payload = append(payload, zotero.WebItem(zotero.ItemFor(pp, title, ""), key))
			}
			keys, err := wc.CreateItems(ctx, payload)
			if err != nil {
				return err
			}
			created := 0
			for _, k := range keys {
				if k != "" {
					created++
				}
			}
			lg.log("Created %d of %d items.", created, len(kept))
			// Notes as child items.
			var notes []any
			for i, pp := range kept {
				if keys[i] == "" || pp.Extract == nil {
					continue
				}
				it := zotero.ItemFor(pp, title, "")
				if len(it.Notes) > 0 {
					notes = append(notes, map[string]any{"itemType": "note", "parentItem": keys[i], "note": it.Notes[0].Note, "tags": []any{}})
				}
			}
			if len(notes) > 0 {
				if _, err := wc.CreateItems(ctx, notes); err != nil {
					lg.log("Notes failed: %v", err)
				} else {
					lg.log("Added %d extraction notes.", len(notes))
				}
			}
			lg.phase(2, 2, "Uploading PDFs", len(kept))
			up, failed := 0, 0
			for i, pp := range kept {
				lg.progress(i+1, len(kept))
				if keys[i] == "" || pp.Download.Status != model.DownloadOK || pp.Download.Path == "" {
					continue
				}
				if err := wc.UploadPDF(ctx, keys[i], pp.Download.Path); err != nil {
					failed++
					lg.log("PDF upload failed for %s: %v", shortTitle(pp.Title), err)
					continue
				}
				up++
			}
			lg.log("Done: %d items, %d PDFs uploaded%s. Zotero desktop will sync the collection %q.", created, up, func() string {
				if failed > 0 {
					return fmt.Sprintf(", %d uploads failed", failed)
				}
				return ""
			}(), name)
			return nil
		})
		if err != nil {
			http.Redirect(w, r, "/projects/"+id+"/zotero?error="+urlEsc(err.Error()), http.StatusSeeOther)
			return
		}
	default:
		http.Redirect(w, r, "/projects/"+id+"/zotero?error="+urlEsc("choose desktop or web"), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/projects/"+id+"/zotero", http.StatusSeeOther)
}

// zoteroLocalExport creates the collection, items, notes and PDF attachments
// in the running Zotero desktop through its local API.
func (s *Server) zoteroLocalExport(ctx context.Context, id, title, name string, kept []*model.Paper, lg jobLogger) error {
	z := s.loadZotero()
	api := zotero.NewLocalAPI(s.cfg.ZoteroLocal, z.LocalKey)
	if err := api.FetchServerID(ctx); err != nil {
		return err
	}
	lg.phase(1, 3, "Connecting to Zotero desktop", 0)
	// authorise: ask Zotero for a key when we have none; retry a write once
	// after a 401 (single-use "Allow" keys are consumed by one write).
	authorize := func() error {
		lg.log("Zotero is asking for permission: click “Always Allow” in the Zotero window (“Allow” works for one write only).")
		key, remember, err := api.AuthorizeLocal(ctx, "litreview")
		if err != nil {
			return err
		}
		if remember {
			z.LocalKey = key
			_ = s.saveZotero(z)
			lg.log("Permanent local API key saved.")
		} else {
			lg.log("One-time key granted; you will be asked again for the next write.")
		}
		return nil
	}
	withAuth := func(op func() error) error {
		if api.APIKey == "" {
			if err := authorize(); err != nil {
				return err
			}
		}
		err := op()
		if errors.Is(err, zotero.ErrUnauthorized) {
			api.APIKey = ""
			if err := authorize(); err != nil {
				return err
			}
			err = op()
		}
		return err
	}
	cols, err := api.ListCollections(ctx)
	if err != nil {
		return err
	}
	colKey, exists := zotero.FindCollectionKey(cols, name)
	if exists {
		lg.log("Using existing collection %q.", name)
	} else {
		if err := withAuth(func() error {
			k, err := api.CreateCollection(ctx, name)
			colKey = k
			return err
		}); err != nil {
			return fmt.Errorf("create collection: %w", err)
		}
		lg.log("Created collection %q in Zotero desktop.", name)
	}
	lg.phase(2, 3, "Creating items", len(kept))
	// Idempotent: reuse items already in the collection (matched by DOI, then
	// title) so the export can be re-run to add missing notes and PDFs.
	existing, err := api.ListCollectionItems(ctx, colKey)
	if err != nil {
		return err
	}
	byDOI := map[string]string{}
	byTitle := map[string]string{}
	for _, it := range existing {
		if it.DOI != "" {
			byDOI[it.DOI] = it.Key
		}
		byTitle[model.NormalizeTitle(it.Title)] = it.Key
	}
	keys := make([]string, len(kept))
	var payload []any
	var payloadIdx []int
	reused := 0
	for i, pp := range kept {
		if k, ok := byDOI[strings.ToLower(pp.DOI)]; ok && pp.DOI != "" {
			keys[i] = k
			reused++
			continue
		}
		if k, ok := byTitle[model.NormalizeTitle(pp.Title)]; ok {
			keys[i] = k
			reused++
			continue
		}
		payload = append(payload, zotero.WebItem(zotero.ItemFor(pp, title, ""), colKey))
		payloadIdx = append(payloadIdx, i)
	}
	if reused > 0 {
		lg.log("%d items already in the collection; reusing them.", reused)
	}
	created := 0
	if len(payload) > 0 {
		var newKeys []string
		if err := withAuth(func() error {
			k, err := api.CreateItems(ctx, payload)
			newKeys = k
			return err
		}); err != nil {
			return fmt.Errorf("create items: %w", err)
		}
		for j, k := range newKeys {
			if j < len(payloadIdx) && k != "" {
				keys[payloadIdx[j]] = k
				created++
			}
		}
		lg.log("Created %d new items.", created)
		if created < len(payload) && api.LastFailure() != "" {
			lg.log("Some items were rejected: %s", api.LastFailure())
		}
	}
	// Notes and PDFs only where missing.
	var notes []any
	needPDF := map[int]bool{}
	for i, pp := range kept {
		if keys[i] == "" {
			continue
		}
		kinds, err := api.ChildTypes(ctx, keys[i])
		if err != nil {
			return err
		}
		hasNote, hasAtt := false, false
		for _, k := range kinds {
			if k == "note" {
				hasNote = true
			}
			if k == "attachment" {
				hasAtt = true
			}
		}
		if !hasNote && pp.Extract != nil {
			if it := zotero.ItemFor(pp, title, ""); len(it.Notes) > 0 {
				notes = append(notes, map[string]any{"itemType": "note", "parentItem": keys[i], "note": it.Notes[0].Note, "tags": []any{}})
			}
		}
		if !hasAtt && pp.Download.Status == model.DownloadOK && pp.Download.Path != "" {
			needPDF[i] = true
		}
	}
	if len(notes) > 0 {
		if err := withAuth(func() error { _, err := api.CreateItems(ctx, notes); return err }); err != nil {
			lg.log("Notes failed: %v", err)
		} else {
			lg.log("Added %d extraction notes.", len(notes))
		}
	}
	lg.phase(3, 3, "Storing PDFs in Zotero", len(needPDF))
	lg.log("Storing %d PDFs (%d items already have an attachment).", len(needPDF), len(kept)-len(needPDF))
	up, failed, done := 0, 0, 0
	for i, pp := range kept {
		if !needPDF[i] {
			continue
		}
		done++
		lg.progress(done, len(needPDF))
		if err := withAuth(func() error { return api.UploadPDF(ctx, keys[i], pp.Download.Path) }); err != nil {
			failed++
			lg.log("PDF failed for %s: %v", shortTitle(pp.Title), err)
			continue
		}
		up++
	}
	extra := ""
	if failed > 0 {
		extra = fmt.Sprintf(", %d PDFs failed", failed)
	}
	lg.log("Done: collection %q holds %d items (%d new, %d reused); %d PDFs stored in Zotero this run%s.", name, created+reused, created, reused, up, extra)
	return nil
}
