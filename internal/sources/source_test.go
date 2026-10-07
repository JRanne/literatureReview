package sources

import (
	"testing"

	"literaturereview/internal/config"
	"literaturereview/internal/model"
)

func TestReconstructAbstract(t *testing.T) {
	idx := map[string][]int{"world": {1}, "hello": {0}, "again": {2}}
	if got := ReconstructAbstract(idx); got != "hello world again" {
		t.Fatalf("got %q", got)
	}
}

func TestMergeDedupes(t *testing.T) {
	a := &model.Paper{DOI: "10.1/ABC", Title: "A Study of Things in Organisations", FoundIn: []string{"openalex"}, Citations: 3}
	b := &model.Paper{DOI: "https://doi.org/10.1/abc", Title: "A study of things in organisations.", FoundIn: []string{"crossref"}, Citations: 9, Abstract: "longer abstract"}
	c := &model.Paper{Title: "A Study of Things in Organisations", FoundIn: []string{"arxiv"}, PDFURLs: []string{"https://arxiv.org/pdf/1"}}
	d := &model.Paper{Title: "Entirely different paper about something else", FoundIn: []string{"pubmed"}}
	merged, added := Merge(nil, []*model.Paper{a, b, c, d})
	if added != 2 || len(merged) != 2 {
		t.Fatalf("added=%d len=%d", added, len(merged))
	}
	p := merged[0]
	if p.DOI != "10.1/abc" || p.Citations != 9 || p.Abstract != "longer abstract" || len(p.FoundIn) != 3 || len(p.PDFURLs) != 1 {
		t.Fatalf("bad merge: %+v", p)
	}
	if p.ID == "" {
		t.Fatal("expected ID assigned")
	}
}

func TestStripTags(t *testing.T) {
	in := `<jats:p>We study <jats:italic>AI</jats:italic> &amp; policy.</jats:p>`
	if got := stripTags(in); got != "We study AI & policy." {
		t.Fatalf("got %q", got)
	}
}

func TestIsPreprint(t *testing.T) {
	cases := map[*model.Paper]bool{
		{DOI: "10.48550/arxiv.2401.01291"}: true,
		{DOI: "10.2139/ssrn.4544943"}:      true,
		{DOI: "10.1016/j.dss.2019.04.004"}: false,
		{Venue: "arXiv (cs.CL)"}:           true,
		{Venue: "MIS Quarterly"}:           false,
		{DOI: "10.1007/x", Preprint: true}: true,
	}
	for p, want := range cases {
		if got := IsPreprint(p); got != want {
			t.Errorf("%+v → %v", p, got)
		}
	}
	kept, dropped := FilterPreprints([]*model.Paper{{DOI: "10.48550/arxiv.1"}, {DOI: "10.1145/1"}})
	if len(kept) != 1 || len(dropped) != 1 || !dropped[0].Preprint {
		t.Fatal("filter")
	}
}

func TestRegistryHasNoArxiv(t *testing.T) {
	r := NewRegistry(config.Config{})
	if _, ok := r.Get("arxiv"); ok {
		t.Fatal("arXiv must not be a source")
	}
}
