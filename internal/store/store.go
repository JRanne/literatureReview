// Package store persists projects as JSON files and serves them from memory.
package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"literaturereview/internal/model"
)

// ErrNotFound is returned when a project ID is unknown.
var ErrNotFound = errors.New("project not found")

// Store is a thread-safe in-memory map of projects backed by JSON files.
type Store struct {
	dir      string
	mu       sync.RWMutex
	projects map[string]*model.Project
}

// Open loads all projects from dir (creating it if needed).
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, projects: map[string]*model.Project{}}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		var p model.Project
		if err := json.Unmarshal(b, &p); err != nil {
			return nil, fmt.Errorf("decode %s: %w", e.Name(), err)
		}
		// Projects written before decision provenance existed: never let a
		// re-screen silently overwrite a decision of unknown origin.
		for _, pp := range p.Papers {
			if pp.Decision != model.DecisionUndecided && pp.DecisionBy == "" {
				pp.DecisionBy = "user"
			}
		}
		// A job can't survive a restart.
		if p.Job != nil && p.Job.Running {
			p.Job.Running = false
			p.Job.Error = "interrupted by server restart"
			p.Job.FinishedAt = time.Now()
			if p.Stage == model.StageSearching {
				p.Stage = model.StageSetup
			}
			if p.Stage == model.StageDownloading {
				p.Stage = model.StageScreening
			}
		}
		s.projects[p.ID] = &p
	}
	return s, nil
}

// Dir returns the data directory.
func (s *Store) Dir() string { return s.dir }

// ProjectDir returns (and creates) the per-project directory.
func (s *Store) ProjectDir(id string) (string, error) {
	d := filepath.Join(s.dir, id)
	return d, os.MkdirAll(d, 0o755)
}

// NewID returns a random 10-hex-char identifier.
func NewID() string {
	var b [5]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// List returns projects newest first.
func (s *Store) List() []*model.Project {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*model.Project, 0, len(s.projects))
	for _, p := range s.projects {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// Get returns a project by ID.
func (s *Store) Get(id string) (*model.Project, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.projects[id]
	if !ok {
		return nil, ErrNotFound
	}
	return p, nil
}

// Put inserts or replaces a project and writes it to disk.
func (s *Store) Put(p *model.Project) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p.UpdatedAt = time.Now()
	s.projects[p.ID] = p
	return s.writeLocked(p)
}

// Update applies fn to the project under the write lock and persists it.
func (s *Store) Update(id string, fn func(p *model.Project) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.projects[id]
	if !ok {
		return ErrNotFound
	}
	if err := fn(p); err != nil {
		return err
	}
	p.UpdatedAt = time.Now()
	return s.writeLocked(p)
}

// View runs fn with the read lock held.
func (s *Store) View(id string, fn func(p *model.Project)) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.projects[id]
	if !ok {
		return ErrNotFound
	}
	fn(p)
	return nil
}

// Delete removes a project and its files.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.projects[id]; !ok {
		return ErrNotFound
	}
	delete(s.projects, id)
	_ = os.Remove(filepath.Join(s.dir, id+".json"))
	return os.RemoveAll(filepath.Join(s.dir, id))
}

func (s *Store) writeLocked(p *model.Project) error {
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(s.dir, p.ID+".json.tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.dir, p.ID+".json"))
}
