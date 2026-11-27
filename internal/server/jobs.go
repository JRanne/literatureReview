package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"literaturereview/internal/model"
)

type jobLogger struct {
	s  *Server
	id string
}

func (l jobLogger) log(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	log.Printf("[%s] %s", l.id, msg)
	_ = l.s.store.Update(l.id, func(p *model.Project) error {
		if p.Job != nil {
			p.Job.Message = msg
			p.Job.Log = append(p.Job.Log, time.Now().Format("15:04:05")+"  "+msg)
			if len(p.Job.Log) > 400 {
				p.Job.Log = p.Job.Log[len(p.Job.Log)-400:]
			}
		}
		return nil
	})
}

// phase sets the current phase label and resets the bar. total==0 shows an
// indeterminate bar.
func (l jobLogger) phase(num, of int, label string, total int) {
	_ = l.s.store.Update(l.id, func(p *model.Project) error {
		if p.Job != nil {
			p.Job.Phase, p.Job.PhaseNum, p.Job.PhaseTotal = label, num, of
			p.Job.Progress, p.Job.Total = 0, total
		}
		return nil
	})
}

func (l jobLogger) progress(done, total int) {
	_ = l.s.store.Update(l.id, func(p *model.Project) error {
		if p.Job != nil {
			p.Job.Progress, p.Job.Total = done, total
		}
		return nil
	})
}

// runJob starts fn in the background as the project's single active job.
func (s *Server) runJob(id, name string, fn func(ctx context.Context, lg jobLogger) error) error {
	s.jobMu.Lock()
	defer s.jobMu.Unlock()
	if _, busy := s.jobs[id]; busy {
		return errors.New("a job is already running for this project")
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.jobs[id] = cancel
	if err := s.store.Update(id, func(p *model.Project) error {
		p.Job = &model.JobStatus{Name: name, Running: true, StartedAt: time.Now()}
		return nil
	}); err != nil {
		cancel()
		delete(s.jobs, id)
		return err
	}
	go func() {
		defer func() {
			cancel()
			s.jobMu.Lock()
			delete(s.jobs, id)
			s.jobMu.Unlock()
		}()
		err := fn(ctx, jobLogger{s: s, id: id})
		_ = s.store.Update(id, func(p *model.Project) error {
			if p.Job != nil {
				p.Job.Running = false
				p.Job.FinishedAt = time.Now()
				if err != nil {
					p.Job.Error = err.Error()
					p.Job.Message = "Failed: " + err.Error()
				} else {
					p.Job.Message = "Done"
				}
			}
			return nil
		})
		if err != nil {
			log.Printf("[%s] job %s failed: %v", id, name, err)
		}
	}()
	return nil
}

func (s *Server) cancelJob(id string) {
	s.jobMu.Lock()
	cancel, ok := s.jobs[id]
	s.jobMu.Unlock()
	if ok {
		cancel()
	}
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.cancelJob(id)
	http.Redirect(w, r, "/projects/"+id, http.StatusSeeOther)
}

func (s *Server) handleJobPartial(w http.ResponseWriter, r *http.Request) {
	p, err := s.store.Get(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var running bool
	_ = s.store.View(p.ID, func(p *model.Project) { running = p.Job != nil && p.Job.Running })
	if !running {
		w.Header().Set("HX-Refresh", "true")
	}
	_ = s.store.View(p.ID, func(p *model.Project) { s.render(w, "job", map[string]any{"P": p}) })
}

func (s *Server) handleDownloadsPartial(w http.ResponseWriter, r *http.Request) {
	p, err := s.store.Get(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var running bool
	_ = s.store.View(p.ID, func(p *model.Project) { running = p.Job != nil && p.Job.Running })
	if !running {
		w.Header().Set("HX-Refresh", "true")
	}
	_ = s.store.View(p.ID, func(p *model.Project) { s.render(w, "downloads", s.projectData(p, "keep")) })
}
