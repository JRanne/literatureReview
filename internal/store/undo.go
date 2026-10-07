package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"literaturereview/internal/model"
)

// Snapshot is one undo/redo entry: a label and the full project JSON.
type Snapshot struct {
	Label string    `json:"label"`
	At    time.Time `json:"at"`
	Data  []byte    `json:"data"`
}

type history struct {
	Undo []Snapshot `json:"undo"`
	Redo []Snapshot `json:"redo"`
}

const maxHistory = 30

func (s *Store) historyPath(id string) string { return filepath.Join(s.dir, id+".history.json") }

func (s *Store) loadHistory(id string) history {
	var h history
	if b, err := os.ReadFile(s.historyPath(id)); err == nil {
		_ = json.Unmarshal(b, &h)
	}
	return h
}

func (s *Store) saveHistory(id string, h history) {
	b, _ := json.Marshal(h)
	_ = os.WriteFile(s.historyPath(id), b, 0o644)
}

// SaveUndo records the project's current state under label, so the next
// change can be undone. It clears the redo stack.
func (s *Store) SaveUndo(id, label string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.projects[id]
	if !ok {
		return ErrNotFound
	}
	data, err := json.Marshal(stripJob(p))
	if err != nil {
		return err
	}
	h := s.loadHistory(id)
	h.Undo = append(h.Undo, Snapshot{Label: label, At: time.Now(), Data: data})
	if len(h.Undo) > maxHistory {
		h.Undo = h.Undo[len(h.Undo)-maxHistory:]
	}
	h.Redo = nil
	s.saveHistory(id, h)
	return nil
}

// Labels returns the labels of the next undo and redo entries ("" if none).
func (s *Store) Labels(id string) (undo, redo string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	h := s.loadHistory(id)
	if n := len(h.Undo); n > 0 {
		undo = h.Undo[n-1].Label
	}
	if n := len(h.Redo); n > 0 {
		redo = h.Redo[n-1].Label
	}
	return
}

// Undo restores the most recent snapshot (pushing the current state onto
// the redo stack). It refuses while a job is running.
func (s *Store) Undo(id string) (string, error) { return s.step(id, true) }

// Redo re-applies the most recently undone change.
func (s *Store) Redo(id string) (string, error) { return s.step(id, false) }

func (s *Store) step(id string, undo bool) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.projects[id]
	if !ok {
		return "", ErrNotFound
	}
	if p.Job != nil && p.Job.Running {
		return "", errors.New("wait for the running job to finish before undoing")
	}
	h := s.loadHistory(id)
	from, to := &h.Undo, &h.Redo
	if !undo {
		from, to = &h.Redo, &h.Undo
	}
	if len(*from) == 0 {
		return "", errors.New("nothing to undo")
	}
	snap := (*from)[len(*from)-1]
	*from = (*from)[:len(*from)-1]
	cur, err := json.Marshal(stripJob(p))
	if err != nil {
		return "", err
	}
	*to = append(*to, Snapshot{Label: snap.Label, At: time.Now(), Data: cur})
	var restored model.Project
	if err := json.Unmarshal(snap.Data, &restored); err != nil {
		return "", err
	}
	restored.Job = p.Job // keep the (finished) job record for the log view
	restored.UpdatedAt = time.Now()
	s.projects[id] = &restored
	s.saveHistory(id, h)
	return snap.Label, s.writeLocked(&restored)
}

// stripJob copies the project without the transient job record.
func stripJob(p *model.Project) *model.Project {
	c := *p
	c.Job = nil
	return &c
}
