package ai

import (
	"strings"
	"testing"

	"literaturereview/internal/model"
)

func TestExtractJSON(t *testing.T) {
	cases := map[string]string{
		"```json\n[{\"a\":1}]\n```":             `[{"a":1}]`,
		"Here you go:\n[{\"a\":1}]\nThanks":     `[{"a":1}]`,
		"{\"x\": [1,2]}":                        `{"x": [1,2]}`,
		"Sure! ```\n{\"k\": \"v\"}\n``` done":   `{"k": "v"}`,
		"[{\"id\":\"1\",\"verdict\":\"keep\"}]": `[{"id":"1","verdict":"keep"}]`,
	}
	for in, want := range cases {
		if got := ExtractJSON(in); got != want {
			t.Errorf("ExtractJSON(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDisciplineInContext(t *testing.T) {
	p := &model.Project{Question: "Q", Discipline: "Information Systems research"}
	ctx := projectContext(p)
	if !strings.Contains(ctx, "Discipline and audience: Information Systems research") || !strings.HasSuffix(strings.TrimSpace(ctx), "Research question: Q") {
		t.Fatalf("context: %q", ctx)
	}
	if disciplineClause(&model.Project{}) != "" || disciplineClause(p) != " working for a researcher in Information Systems research" {
		t.Fatal("clause")
	}
}
