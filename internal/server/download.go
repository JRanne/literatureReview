package server

import (
	"context"
	"errors"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"literaturereview/internal/browserlogin"
	"literaturereview/internal/fetch"
	"literaturereview/internal/fulltext"
	"literaturereview/internal/model"
)

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var kept []model.Paper
	err := s.store.Update(id, func(p *model.Project) error {
		for _, pp := range p.Papers {
			if pp.Decision == model.DecisionKeep && pp.Download.Status != model.DownloadOK {
				pp.Download = model.DownloadResult{Status: model.DownloadPending}
				kept = append(kept, *pp)
			}
		}
		if len(p.Kept()) == 0 {
			return errors.New("no papers are marked keep")
		}
		if len(kept) == 0 {
			return errors.New("every kept paper already has a PDF")
		}
		if model.StageIndex(p.Stage) < 3 {
			p.Stage = model.StageDownloading
		}
		return nil
	})
	if err != nil {
		http.Redirect(w, r, "/projects/"+id+"?view=downloads&error="+urlEsc(err.Error()), http.StatusSeeOther)
		return
	}
	if err := s.runJob(id, "download", func(ctx context.Context, lg jobLogger) error {
		return s.downloadJob(ctx, id, kept, lg)
	}); err != nil {
		http.Redirect(w, r, "/projects/"+id+"?view=downloads&error="+urlEsc(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/projects/"+id+"?view=downloads", http.StatusSeeOther)
}

func (s *Server) downloadJob(ctx context.Context, id string, papers []model.Paper, lg jobLogger) error {
	dir, err := s.store.ProjectDir(id)
	if err != nil {
		return err
	}
	dir = filepath.Join(dir, "pdfs")
	lg.phase(1, 1, "Downloading PDFs", len(papers))
	lg.log("Downloading %d papers via: %s", len(papers), strings.Join(s.chain.Names(), " → "))
	s.ensureSession(ctx, lg)
	const workers = 3
	jobs := make(chan model.Paper)
	var wg sync.WaitGroup
	var mu sync.Mutex
	done, ok := 0, 0
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for pp := range jobs {
				pp := pp
				_ = s.store.Update(id, func(p *model.Project) error {
					if t := p.Paper(pp.ID); t != nil {
						t.Download.Status = model.DownloadRunning
					}
					return nil
				})
				res := s.chain.Run(ctx, &pp, dir)
				var textPath string
				var textChars int
				if res.Status == model.DownloadOK && fulltext.Available() {
					if tp, n, err := fulltext.ExtractToFile(ctx, res.Path, filepath.Join(filepath.Dir(dir), "text")); err == nil {
						textPath, textChars = tp, n
					} else {
						lg.log("text extraction failed for %s: %v", shortTitle(pp.Title), err)
					}
				}
				_ = s.store.Update(id, func(p *model.Project) error {
					if t := p.Paper(pp.ID); t != nil {
						t.Download = res
						if textPath != "" {
							t.TextPath, t.TextChars = textPath, textChars
						}
					}
					return nil
				})
				mu.Lock()
				done++
				if res.Status == model.DownloadOK {
					ok++
					lg.log("✓ %s — via %s", shortTitle(pp.Title), res.Method)
				} else {
					lg.log("✗ %s — all methods failed", shortTitle(pp.Title))
				}
				lg.progress(done, len(papers))
				mu.Unlock()
			}
		}()
	}
	for _, pp := range papers {
		if ctx.Err() != nil {
			break
		}
		jobs <- pp
	}
	close(jobs)
	wg.Wait()
	_ = s.store.Update(id, func(p *model.Project) error {
		for _, pp := range p.Papers {
			if pp.Download.Status == model.DownloadRunning {
				pp.Download.Status = model.DownloadFailed
			}
		}
		if model.StageIndex(p.Stage) < 4 {
			p.Stage = model.StageDone
		}
		return nil
	})
	waiting := 0
	_ = s.store.View(id, func(p *model.Project) {
		for _, pp := range p.Kept() {
			if pp.Download.Status != model.DownloadOK && pp.Download.NeedsLogin {
				waiting++
			}
		}
	})
	if waiting > 0 {
		lg.log("Finished: %d of %d PDFs obtained. %d paper(s) need university access: log in on the Library access page and they are retried automatically.", ok, len(papers), waiting)
	} else {
		lg.log("Finished: %d of %d PDFs obtained.", ok, len(papers))
	}
	return ctx.Err()
}

// ensureSession checks the proxy session once before a download batch. An
// expired session is first renewed silently through the saved browser
// profile (the university SSO often outlives the EZproxy cookie); if that
// fails, the proxy stages skip their papers quickly and mark them for an
// automatic retry after the user logs in.
func (s *Server) ensureSession(ctx context.Context, lg jobLogger) {
	px := s.chain.Proxy()
	if px == nil || !px.HasCookie() {
		return
	}
	tctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	fin, err := px.Test(tctx)
	cancel()
	a := s.loadAccess()
	a.LastTestAt = time.Now()
	if err == nil {
		a.LastTestOK, a.LastTestMsg = true, "reached "+fin
		_ = s.saveAccess(a)
		return
	}
	a.LastTestOK, a.LastTestMsg = false, err.Error()
	_ = s.saveAccess(a)
	if !errors.Is(err, fetch.ErrSessionExpired) {
		lg.log("University proxy check failed (%v); continuing with the full chain.", err)
		return
	}
	lg.log("University proxy session has expired; trying to renew it through the saved browser profile…")
	if msg, ok := s.renewSession(ctx); ok {
		lg.log("University access renewed (%s).", msg)
		return
	} else {
		lg.log("Could not renew the session automatically: %s", msg)
	}
	lg.log("Papers that need university access are skipped now and retried automatically once you log in (Library access → Open browser login).")
}

// renewSession tries the silent refresh and applies the new cookie.
func (s *Server) renewSession(ctx context.Context) (string, bool) {
	if s.browser == nil {
		return "no browser available for a silent renewal", false
	}
	a := s.loadAccess()
	host, err := browserlogin.ProxyHost(a.ProxyPrefix)
	if err != nil {
		return err.Error(), false
	}
	hdr, cks, err := s.browser.RefreshSession(ctx, a.ProxyPrefix+"https://www.sciencedirect.com/", host)
	if err != nil {
		return err.Error(), false
	}
	a.ProxyCookie, a.ProxyCookies, a.UpdatedAt = hdr, cks, time.Now()
	if err := s.applyAccess(a); err != nil {
		return err.Error(), false
	}
	tctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	fin, terr := s.chain.Proxy().Test(tctx)
	a.LastTestAt = time.Now()
	if terr != nil {
		a.LastTestOK, a.LastTestMsg = false, terr.Error()
		_ = s.saveAccess(a)
		return "new cookie was rejected: " + terr.Error(), false
	}
	a.LastTestOK, a.LastTestMsg = true, "reached "+fin
	_ = s.saveAccess(a)
	s.chain.Session.Reset()
	return "reached " + fin, true
}

// afterSessionRefresh runs when a valid proxy session was (re)established:
// every project with papers that were waiting for a login gets a retry job.
func (s *Server) afterSessionRefresh() {
	s.chain.Session.Reset()
	for _, pr := range s.store.List() {
		id := pr.ID
		var todo []model.Paper
		_ = s.store.Update(id, func(p *model.Project) error {
			if p.Job != nil && p.Job.Running {
				return nil
			}
			for _, pp := range p.Kept() {
				if pp.Download.Status != model.DownloadOK && pp.Download.NeedsLogin {
					pp.Download = model.DownloadResult{Status: model.DownloadPending}
					todo = append(todo, *pp)
				}
			}
			return nil
		})
		if len(todo) == 0 {
			continue
		}
		if err := s.runJob(id, "retry", func(ctx context.Context, lg jobLogger) error {
			lg.log("University access restored: retrying %d paper(s) that were waiting for a login.", len(todo))
			return s.downloadJob(ctx, id, todo, lg)
		}); err != nil {
			log.Printf("[%s] automatic retry after login not started: %v", id, err)
		}
	}
}

func (s *Server) handleRetry(w http.ResponseWriter, r *http.Request) {
	id, pid := r.PathValue("id"), r.PathValue("pid")
	var target *model.Paper
	_ = s.store.View(id, func(p *model.Project) {
		if pp := p.Paper(pid); pp != nil {
			c := *pp
			target = &c
		}
	})
	if target == nil {
		http.NotFound(w, r)
		return
	}
	err := s.runJob(id, "retry", func(ctx context.Context, lg jobLogger) error {
		return s.downloadJob(ctx, id, []model.Paper{*target}, lg)
	})
	if err != nil {
		http.Redirect(w, r, "/projects/"+id+"?view=downloads&error="+urlEsc(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/projects/"+id+"?view=downloads", http.StatusSeeOther)
}
