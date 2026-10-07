package model

import "testing"

func TestConsensus(t *testing.T) {
	both := &Paper{AIVerdict: VerdictKeep, AIScore: 80, Jev: &JevAssessment{Verdict: VerdictKeep, Score: 90}}
	if both.FinalVerdict() != VerdictKeep || both.Agreement() != "agree" || both.RankScore() != 85 {
		t.Fatalf("agree: %s %s %d", both.FinalVerdict(), both.Agreement(), both.RankScore())
	}
	conflict := &Paper{AIVerdict: VerdictKeep, Jev: &JevAssessment{Verdict: VerdictDrop}}
	conflict.ApplyVerdict()
	if conflict.FinalVerdict() != VerdictUnsure || conflict.Agreement() != "conflict" || conflict.Decision != DecisionUndecided {
		t.Fatalf("conflict: %s %s %s", conflict.FinalVerdict(), conflict.Agreement(), conflict.Decision)
	}
	partial := &Paper{AIVerdict: VerdictKeep, Jev: &JevAssessment{Verdict: VerdictUnsure}}
	if partial.Agreement() != "partial" || partial.FinalVerdict() != VerdictKeep {
		t.Fatal("keep + unsure should be inclusive keep")
	}
	if (&Paper{AIVerdict: VerdictUnsure, Jev: &JevAssessment{Verdict: VerdictKeep}}).FinalVerdict() != VerdictKeep {
		t.Fatal("unsure + keep should be keep")
	}
	if (&Paper{AIVerdict: VerdictUnsure, Jev: &JevAssessment{Verdict: VerdictDrop}}).FinalVerdict() != VerdictUnsure {
		t.Fatal("unsure + drop should stay undecided")
	}
	if (&Paper{AIVerdict: VerdictDrop, Jev: &JevAssessment{Verdict: VerdictDrop}}).FinalVerdict() != VerdictDrop {
		t.Fatal("drop + drop should drop")
	}
	if (&Paper{AIVerdict: VerdictUnsure, Jev: &JevAssessment{Verdict: VerdictUnsure}}).FinalVerdict() != VerdictUnsure {
		t.Fatal("unsure + unsure should stay undecided")
	}
	jevErr := &Paper{AIVerdict: VerdictDrop, Jev: &JevAssessment{Error: "HTTP 500"}}
	if jevErr.FinalVerdict() != VerdictDrop || jevErr.Agreement() != "" || jevErr.HasJev() {
		t.Fatal("jev error must fall back to Claude")
	}
	jevOnly := &Paper{Jev: &JevAssessment{Verdict: VerdictKeep, Score: 75}}
	if jevOnly.FinalVerdict() != VerdictKeep || jevOnly.RankScore() != 75 {
		t.Fatal("jev only")
	}
	if (&JevAssessment{Relevance: 3.73, Include: 0.78, Exclude: 0.1, Confidence: 0.77}).Detail() != "relevance 3.73/4 · include 78% · exclusion 10% · confidence 0.77" {
		t.Fatalf("detail: %q", (&JevAssessment{Relevance: 3.73, Include: 0.78, Exclude: 0.1, Confidence: 0.77}).Detail())
	}
}
