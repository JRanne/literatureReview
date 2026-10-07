package model

import "testing"

func TestApplyVerdictKeepsUserDecision(t *testing.T) {
	p := &Paper{AIVerdict: VerdictDrop, Decision: DecisionKeep, DecisionBy: "user"}
	p.ApplyVerdict()
	if p.Decision != DecisionKeep || p.DecisionBy != "user" {
		t.Fatal("user decision overwritten")
	}
	p = &Paper{AIVerdict: VerdictKeep, Decision: DecisionDrop, DecisionBy: "ai"}
	p.ApplyVerdict()
	if p.Decision != DecisionKeep || p.DecisionBy != "ai" {
		t.Fatal("ai decision not updated")
	}
	p = &Paper{AIVerdict: VerdictUnsure, Decision: DecisionKeep, DecisionBy: "ai"}
	p.ApplyVerdict()
	if p.Decision != DecisionUndecided || p.DecisionBy != "" {
		t.Fatal("unsure should clear ai decision")
	}
}
