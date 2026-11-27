package ai

import (
	"context"
	"testing"
)

// modelBackend records which model answered, so routing can be asserted
// without a network or the claude binary.
type modelBackend struct {
	model string
	seen  *[]string
}

func (b *modelBackend) Name() string       { return "fake" }
func (b *modelBackend) ModelLabel() string { return b.model }
func (b *modelBackend) WithModel(m string) Backend {
	if m == "" {
		return b
	}
	return &modelBackend{model: m, seen: b.seen}
}
func (b *modelBackend) Complete(context.Context, string, string, int) (string, error) {
	*b.seen = append(*b.seen, b.model)
	return "ok", nil
}

func TestResolveModel(t *testing.T) {
	defaults := Models{StepScreening: "haiku", StepSynthesis: "opus"}
	cases := []struct {
		name    string
		step    Step
		project Models
		want    string
	}{
		{"project choice wins", StepScreening, Models{StepScreening: "sonnet"}, "sonnet"},
		{"configured default", StepScreening, nil, "haiku"},
		{"unset step uses the base model", StepQueries, nil, ""},
		{"adjudication inherits the synthesis default", StepAdjudication, nil, "opus"},
		{"concepts inherit the synthesis default", StepConcepts, nil, "opus"},
		// A project's synthesis pick is a per-draft choice; letting it leak
		// into adjudication would silently move a bulk step to a dearer model.
		{"project synthesis pick does not leak", StepAdjudication, Models{StepSynthesis: "fable"}, "opus"},
		{"project adjudication pick", StepAdjudication, Models{StepAdjudication: "sonnet"}, "sonnet"},
		{"extraction has no fallback", StepExtraction, Models{StepSynthesis: "fable"}, ""},
	}
	for _, c := range cases {
		if got := ResolveModel(c.step, c.project, defaults); got != c.want {
			t.Errorf("%s: ResolveModel(%s) = %q, want %q", c.name, c.step, got, c.want)
		}
	}
}

func TestClientForStepUsesResolvedModel(t *testing.T) {
	var seen []string
	base := New(&modelBackend{model: "base", seen: &seen})
	defaults := Models{StepSynthesis: "opus"}
	project := Models{StepScreening: "haiku"}

	for _, step := range []Step{StepScreening, StepQueries, StepAdjudication} {
		if _, err := base.ForStep(step, project, defaults).Complete(context.Background(), "s", "u", 10); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"haiku", "base", "opus"}
	for i := range want {
		if i >= len(seen) || seen[i] != want[i] {
			t.Fatalf("models used = %v, want %v", seen, want)
		}
	}
}

func TestForStepOnDisabledClient(t *testing.T) {
	c := New(nil).ForStep(StepScreening, Models{StepScreening: "haiku"}, nil)
	if c.Enabled() {
		t.Fatal("a disabled client must stay disabled for every step")
	}
}

func TestStepsAreDescribed(t *testing.T) {
	seen := map[Step]bool{}
	for _, s := range AllSteps {
		if s.Label == "" || s.Help == "" {
			t.Errorf("step %q lacks a label or help text", s.Step)
		}
		seen[s.Step] = true
	}
	for _, s := range []Step{StepQueries, StepScreening, StepAdjudication, StepExtraction, StepConcepts, StepSynthesis} {
		if !seen[s] {
			t.Errorf("step %q missing from AllSteps", s)
		}
	}
}

func TestParseStep(t *testing.T) {
	if s, ok := ParseStep("screening"); !ok || s != StepScreening {
		t.Fatalf("ParseStep(screening) = %q, %v", s, ok)
	}
	if _, ok := ParseStep("bogus"); ok {
		t.Fatal("unknown step accepted")
	}
}

func TestKnownModel(t *testing.T) {
	var offline *Catalog // a disabled backend offers the built-in list
	if !offline.Known("") {
		t.Error("empty (inherit) must be accepted")
	}
	if !offline.Known("claude-opus-5-5") {
		t.Error("listed model rejected")
	}
	if offline.Known("gpt-4") {
		t.Error("unlisted model accepted")
	}
}
