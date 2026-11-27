package config

import (
	"strings"
	"testing"
)

func TestStepModelsFromEnv(t *testing.T) {
	t.Setenv("MODEL_SCREENING", "claude-haiku-5-5")
	t.Setenv("MODEL_SYNTHESIS", "claude-opus-5-5")
	t.Setenv("MODEL_QUERIES", "")
	c := FromEnv()
	if got := c.StepModels["screening"]; got != "claude-haiku-5-5" {
		t.Errorf("screening = %q", got)
	}
	if got := c.StepModels["synthesis"]; got != "claude-opus-5-5" {
		t.Errorf("synthesis = %q", got)
	}
	if _, ok := c.StepModels["queries"]; ok {
		t.Error("an empty MODEL_QUERIES must not be recorded")
	}
}

func TestSynthesisModelIsAnAlias(t *testing.T) {
	t.Setenv("SYNTHESIS_MODEL", "opus")
	t.Setenv("MODEL_SYNTHESIS", "")
	if got := FromEnv().StepModels["synthesis"]; got != "opus" {
		t.Fatalf("SYNTHESIS_MODEL alias: synthesis = %q, want opus", got)
	}
	t.Setenv("MODEL_SYNTHESIS", "claude-fable-5-2")
	if got := FromEnv().StepModels["synthesis"]; got != "claude-fable-5-2" {
		t.Fatalf("MODEL_SYNTHESIS must win over the alias, got %q", got)
	}
}

func TestValidateRejectsUnknownModelStep(t *testing.T) {
	t.Setenv("CONTACT_EMAIL", "x@example.org")
	t.Setenv("MODEL_SCREENNIG", "haiku") // typo must not be silently ignored
	err := FromEnv().Validate()
	if err == nil || !strings.Contains(err.Error(), "MODEL_SCREENNIG") {
		t.Fatalf("Validate() = %v, want it to name MODEL_SCREENNIG", err)
	}
}
