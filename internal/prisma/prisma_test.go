package prisma

import (
	"strings"
	"testing"

	"literaturereview/internal/model"
)

func TestComputeAndRender(t *testing.T) {
	p := &model.Project{
		YearFrom: 2016, YearTo: 2026, Rounds: 2,
		Queries: []model.Query{{Source: "openalex", Query: "a", Results: 50}, {Source: "crossref", Query: "b", Results: 30}, {Source: "openalex", Query: "snowball from 3 kept papers", Results: 20}},
		Stats:   model.SearchStats{Preprints: 4, NonEnglish: 1},
		Papers: []*model.Paper{
			{Decision: model.DecisionKeep, Download: model.DownloadResult{Status: model.DownloadOK}},
			{Decision: model.DecisionKeep},
			{Decision: model.DecisionDrop, DecisionBy: "ai"},
			{Decision: model.DecisionDrop, Preprint: true},
			{Decision: model.DecisionDrop, NonEnglish: true},
			{Decision: model.DecisionDrop, DecisionBy: "user"},
			{Decision: model.DecisionUndecided},
		},
	}
	c := Compute(p)
	if c.Databases != 80 || c.Citation != 20 || c.Automation != 5 || c.Screened != 7 || c.Duplicates != 100-7-5 {
		t.Fatalf("identification: %+v", c)
	}
	if c.Excluded != 4 || c.Undecided != 1 || c.Sought != 2 || c.NotRetrieved != 1 || c.Assessed != 1 || c.Included != 2 {
		t.Fatalf("screening: %+v", c)
	}
	if c.ExcludedReason["Preprint or repository record (not peer reviewed)"] != 1 || c.ExcludedReason["Not in English"] != 1 || c.ExcludedReason["Excluded by the researcher"] != 1 {
		t.Fatalf("reasons: %v", c.ExcludedReason)
	}
	label := func(s string) string { return strings.ToUpper(s) }
	svg := SVG(c, label)
	for _, want := range []string{"<svg", "Records identified from databases (n = 80)", "OPENALEX (n = 50)", "Duplicate records (n = 88)", "Records screened (n = 7)", "Studies included in review (n = 2)", "Citation searching (n = 20)"} {
		if !strings.Contains(svg, want) {
			t.Errorf("svg missing %q", want)
		}
	}
	m := Methods(p, c, label, "Screening was performed by two AI screeners.")
	if !strings.Contains(m, "searching OPENALEX, CROSSREF for publications from 2016 to 2026, using 2 search strings over 2 rounds") || !strings.Contains(m, "2 studies were included") {
		t.Fatalf("methods: %s", m)
	}
}
