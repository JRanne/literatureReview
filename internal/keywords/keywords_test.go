package keywords

import (
	"strings"
	"testing"

	"literaturereview/internal/model"
)

func TestNormalize(t *testing.T) {
	cases := map[string]string{"LLMs": "large language model", " Generative AI ": "generative ai", "Organisations": "organization", "Decision-Making": "decision making", "enterprise systems": "enterprise systems", "ERP": "enterprise resource planning", "analytics": "analytics"}
	for in, want := range cases {
		if got := Normalize(in); got != want {
			t.Errorf("%q → %q, want %q", in, got, want)
		}
	}
}

func TestBuildAndExport(t *testing.T) {
	ps := []*model.Paper{
		{Keywords: []string{"LLMs", "Enterprise"}, Extract: &model.Extraction{Keywords: []string{"governance", "large language models"}}},
		{Keywords: []string{"generative AI", "enterprise", "governance"}},
		{Keywords: []string{"GenAI", "LLM", "small language models"}},
		{Keywords: []string{"intuition", "decision making"}},
		{Keywords: []string{"intuition", "decision-making", "artificial intuition"}},
	}
	net := Build(ps, 2, 50)
	byLabel := map[string]Node{}
	for _, n := range net.Nodes {
		byLabel[n.Label] = n
	}
	if net.Papers != 5 || byLabel["large language model"].Count != 2 || byLabel["enterprise"].Count != 2 || byLabel["governance"].Count != 2 || byLabel["intuition"].Count != 2 || byLabel["decision making"].Count != 2 {
		t.Fatalf("nodes: %+v", net.Nodes)
	}
	if _, ok := byLabel["small language model"]; ok {
		t.Fatal("below threshold keyword kept")
	}
	// intuition & decision making co-occur twice; they must share a cluster
	// distinct from the enterprise/LLM group.
	if byLabel["intuition"].Cluster != byLabel["decision making"].Cluster || byLabel["intuition"].Cluster == byLabel["enterprise"].Cluster {
		t.Fatalf("clusters: %+v", net.Nodes)
	}
	var found bool
	for _, e := range net.Edges {
		if (e.From == byLabel["intuition"].ID && e.To == byLabel["decision making"].ID) || (e.To == byLabel["intuition"].ID && e.From == byLabel["decision making"].ID) {
			found = e.Weight == 2
		}
	}
	if !found {
		t.Fatalf("edge weight: %+v", net.Edges)
	}
	m := net.VOSviewerMap()
	if !strings.HasPrefix(m, "id\tlabel\tcluster") || !strings.Contains(m, "\tintuition\t") {
		t.Fatalf("map: %s", m)
	}
	if lines := strings.Count(net.VOSviewerNetwork(), "\n"); lines != len(net.Edges) {
		t.Fatal("network export")
	}
}
