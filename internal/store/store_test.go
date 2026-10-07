package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenMigratesLegacyDecisions(t *testing.T) {
	dir := t.TempDir()
	legacy := `{"id":"old1","title":"t","question":"q","stage":"screening","papers":[
	  {"id":"a","title":"A","decision":"keep"},
	  {"id":"b","title":"B","decision":"drop","decision_by":"ai"},
	  {"id":"c","title":"C","decision":""}],
	  "job":{"name":"search","running":true}}`
	if err := os.WriteFile(filepath.Join(dir, "old1.json"), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := st.Get("old1")
	if p.Paper("a").DecisionBy != "user" || p.Paper("b").DecisionBy != "ai" || p.Paper("c").DecisionBy != "" {
		t.Fatalf("migration wrong: %+v %+v %+v", p.Paper("a"), p.Paper("b"), p.Paper("c"))
	}
	if p.Job.Running || p.Job.Error == "" {
		t.Fatal("interrupted job not marked")
	}
}
