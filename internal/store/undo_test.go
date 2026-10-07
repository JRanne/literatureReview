package store

import (
	"testing"

	"literaturereview/internal/model"
)

func TestUndoRedo(t *testing.T) {
	st, _ := Open(t.TempDir())
	p := &model.Project{ID: "u1", Title: "T", Papers: []*model.Paper{{ID: "a", Decision: model.DecisionKeep}, {ID: "b"}}}
	_ = st.Put(p)
	if u, r := st.Labels("u1"); u != "" || r != "" {
		t.Fatal("history should be empty")
	}
	_ = st.SaveUndo("u1", "Drop a")
	_ = st.Update("u1", func(p *model.Project) error { p.Paper("a").Decision = model.DecisionDrop; return nil })
	_ = st.SaveUndo("u1", "Keep b")
	_ = st.Update("u1", func(p *model.Project) error { p.Paper("b").Decision = model.DecisionKeep; return nil })
	if u, _ := st.Labels("u1"); u != "Keep b" {
		t.Fatalf("label %q", u)
	}
	label, err := st.Undo("u1")
	if err != nil || label != "Keep b" {
		t.Fatal(label, err)
	}
	q, _ := st.Get("u1")
	if q.Paper("b").Decision != model.DecisionUndecided || q.Paper("a").Decision != model.DecisionDrop {
		t.Fatalf("after undo: %+v %+v", q.Paper("a"), q.Paper("b"))
	}
	if _, err := st.Undo("u1"); err != nil {
		t.Fatal(err)
	}
	q, _ = st.Get("u1")
	if q.Paper("a").Decision != model.DecisionKeep {
		t.Fatal("second undo")
	}
	if _, err := st.Undo("u1"); err == nil {
		t.Fatal("expected nothing to undo")
	}
	if label, err := st.Redo("u1"); err != nil || label != "Drop a" {
		t.Fatal(label, err)
	}
	q, _ = st.Get("u1")
	if q.Paper("a").Decision != model.DecisionDrop {
		t.Fatal("redo")
	}
	// A new change clears redo.
	_ = st.SaveUndo("u1", "x")
	if _, r := st.Labels("u1"); r != "" {
		t.Fatal("redo should be cleared")
	}
	// Refused while a job runs.
	_ = st.Update("u1", func(p *model.Project) error { p.Job = &model.JobStatus{Running: true}; return nil })
	if _, err := st.Undo("u1"); err == nil {
		t.Fatal("must refuse during a job")
	}
	// Persisted across reopen.
	st2, _ := Open(st.Dir())
	if u, _ := st2.Labels("u1"); u != "x" {
		t.Fatalf("history not persisted: %q", u)
	}
}
