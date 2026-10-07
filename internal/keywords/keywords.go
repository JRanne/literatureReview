// Package keywords builds a keyword co-occurrence network (the kind of map
// VOSviewer draws) from the included papers' keywords.
package keywords

import (
	"fmt"
	"sort"
	"strings"

	"literaturereview/internal/model"
)

// Node is one keyword.
type Node struct {
	ID      int    `json:"id"`
	Label   string `json:"label"`
	Count   int    `json:"count"`   // papers mentioning it
	Cluster int    `json:"cluster"` // 1-based community id
	Links   int    `json:"links"`   // distinct co-occurring keywords
	Weight  int    `json:"weight"`  // total link strength
}

// Edge is a co-occurrence between two keywords.
type Edge struct {
	From   int `json:"from"`
	To     int `json:"to"`
	Weight int `json:"weight"` // papers where both occur
}

// Network is the result.
type Network struct {
	Nodes    []Node `json:"nodes"`
	Edges    []Edge `json:"edges"`
	Papers   int    `json:"papers"`
	Distinct int    `json:"distinct"` // distinct keywords before thresholding
}

// synonyms folds common variants onto one canonical keyword.
var synonyms = map[string]string{
	"llm": "large language model", "llms": "large language model", "large language models": "large language model",
	"genai": "generative ai", "generative artificial intelligence": "generative ai", "gen ai": "generative ai",
	"ai": "artificial intelligence", "a.i.": "artificial intelligence",
	"ml":  "machine learning",
	"slm": "small language model", "slms": "small language model", "small language models": "small language model",
	"erp": "enterprise resource planning", "enterprise resource planning (erp)": "enterprise resource planning",
	"is": "information systems", "information system": "information systems",
	"enterprise information system": "enterprise information systems",
	"decision-making":               "decision making", "decision support system": "decision support systems", "dss": "decision support systems",
	"rag": "retrieval-augmented generation", "retrieval augmented generation": "retrieval-augmented generation",
	"nlp":           "natural language processing",
	"organizations": "organization", "organisation": "organization", "organisations": "organization",
	"enterprises":   "enterprise",
	"ai governance": "ai governance", "governance of ai": "ai governance",
	"human-ai collaboration": "human-ai collaboration", "human ai collaboration": "human-ai collaboration",
}

// Normalize canonicalises a keyword.
func Normalize(k string) string {
	k = strings.ToLower(strings.TrimSpace(k))
	k = strings.Trim(k, ".,;:\"'()[]")
	k = strings.Join(strings.Fields(k), " ")
	if k == "" {
		return ""
	}
	if s, ok := synonyms[k]; ok {
		return s
	}
	// crude singularisation for plain plurals of multi-word terms' last word
	if len(k) > 4 && strings.HasSuffix(k, "s") && !strings.HasSuffix(k, "ss") && !strings.HasSuffix(k, "us") && !strings.HasSuffix(k, "is") && !strings.HasSuffix(k, "ics") && !strings.HasSuffix(k, "systems") {
		k = strings.TrimSuffix(k, "s")
	}
	if s, ok := synonyms[k]; ok {
		return s
	}
	return k
}

// PaperKeywords returns the de-duplicated canonical keywords of a paper from
// database keywords and the AI extraction.
func PaperKeywords(p *model.Paper) []string {
	seen := map[string]bool{}
	var out []string
	add := func(ks []string) {
		for _, k := range ks {
			n := Normalize(k)
			if n == "" || seen[n] {
				continue
			}
			seen[n] = true
			out = append(out, n)
		}
	}
	add(p.Keywords)
	if p.Extract != nil {
		add(p.Extract.Keywords)
	}
	return out
}

// Build computes the co-occurrence network over papers, keeping keywords
// that occur in at least minCount papers and at most maxNodes keywords.
func Build(papers []*model.Paper, minCount, maxNodes int) *Network {
	if minCount < 1 {
		minCount = 1
	}
	if maxNodes <= 0 {
		maxNodes = 80
	}
	count := map[string]int{}
	var sets [][]string
	for _, p := range papers {
		ks := PaperKeywords(p)
		if len(ks) == 0 {
			continue
		}
		sets = append(sets, ks)
		for _, k := range ks {
			count[k]++
		}
	}
	distinct := len(count)
	type kc struct {
		k string
		c int
	}
	var kept []kc
	for k, c := range count {
		if c >= minCount {
			kept = append(kept, kc{k, c})
		}
	}
	sort.Slice(kept, func(i, j int) bool {
		if kept[i].c != kept[j].c {
			return kept[i].c > kept[j].c
		}
		return kept[i].k < kept[j].k
	})
	if len(kept) > maxNodes {
		kept = kept[:maxNodes]
	}
	idx := map[string]int{}
	net := &Network{Papers: len(sets), Distinct: distinct}
	for i, x := range kept {
		idx[x.k] = i + 1
		net.Nodes = append(net.Nodes, Node{ID: i + 1, Label: x.k, Count: x.c})
	}
	co := map[[2]int]int{}
	for _, ks := range sets {
		var ids []int
		for _, k := range ks {
			if id, ok := idx[k]; ok {
				ids = append(ids, id)
			}
		}
		sort.Ints(ids)
		for a := 0; a < len(ids); a++ {
			for b := a + 1; b < len(ids); b++ {
				co[[2]int{ids[a], ids[b]}]++
			}
		}
	}
	for pair, w := range co {
		net.Edges = append(net.Edges, Edge{From: pair[0], To: pair[1], Weight: w})
	}
	sort.Slice(net.Edges, func(i, j int) bool {
		if net.Edges[i].Weight != net.Edges[j].Weight {
			return net.Edges[i].Weight > net.Edges[j].Weight
		}
		if net.Edges[i].From != net.Edges[j].From {
			return net.Edges[i].From < net.Edges[j].From
		}
		return net.Edges[i].To < net.Edges[j].To
	})
	links := map[int]int{}
	weight := map[int]int{}
	for _, e := range net.Edges {
		links[e.From]++
		links[e.To]++
		weight[e.From] += e.Weight
		weight[e.To] += e.Weight
	}
	for i := range net.Nodes {
		net.Nodes[i].Links = links[net.Nodes[i].ID]
		net.Nodes[i].Weight = weight[net.Nodes[i].ID]
	}
	cluster(net)
	return net
}

// cluster assigns communities by label propagation over association-
// strength-normalised links (VOSviewer's normalisation: a link between two
// frequent keywords counts less than one between rare ones), using only
// links whose association is above average, then renumbers clusters by size.
func cluster(net *Network) {
	n := len(net.Nodes)
	if n == 0 {
		return
	}
	strength := map[int]float64{}
	total := 0.0
	for _, e := range net.Edges {
		strength[e.From] += float64(e.Weight)
		strength[e.To] += float64(e.Weight)
		total += float64(e.Weight)
	}
	adj := make(map[int]map[int]float64, n)
	for _, e := range net.Edges {
		// association strength = 2m * c_ij / (s_i * s_j); 1 = expected by chance
		a := 2 * total * float64(e.Weight) / (strength[e.From] * strength[e.To])
		if a < 1 {
			continue
		}
		if adj[e.From] == nil {
			adj[e.From] = map[int]float64{}
		}
		if adj[e.To] == nil {
			adj[e.To] = map[int]float64{}
		}
		adj[e.From][e.To] += a
		adj[e.To][e.From] += a
	}
	label := make(map[int]int, n)
	for _, nd := range net.Nodes {
		label[nd.ID] = nd.ID
	}
	order := make([]int, 0, n)
	for _, nd := range net.Nodes {
		order = append(order, nd.ID)
	}
	for iter := 0; iter < 50; iter++ {
		changed := false
		for _, id := range order {
			score := map[int]float64{}
			for nb, w := range adj[id] {
				score[label[nb]] += w
			}
			if len(score) == 0 {
				continue
			}
			best, bestScore := label[id], -1.0
			keys := make([]int, 0, len(score))
			for l := range score {
				keys = append(keys, l)
			}
			sort.Ints(keys)
			for _, l := range keys {
				if score[l] > bestScore+1e-9 {
					best, bestScore = l, score[l]
				}
			}
			if best != label[id] {
				label[id] = best
				changed = true
			}
		}
		if !changed {
			break
		}
	}
	size := map[int]int{}
	for _, l := range label {
		size[l]++
	}
	type ls struct{ l, s int }
	var ordered []ls
	for l, s := range size {
		ordered = append(ordered, ls{l, s})
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].s != ordered[j].s {
			return ordered[i].s > ordered[j].s
		}
		return ordered[i].l < ordered[j].l
	})
	renum := map[int]int{}
	for i, x := range ordered {
		renum[x.l] = i + 1
	}
	for i := range net.Nodes {
		net.Nodes[i].Cluster = renum[label[net.Nodes[i].ID]]
	}
}

// VOSviewerMap renders the "map" file (tab-separated) VOSviewer imports.
func (net *Network) VOSviewerMap() string {
	var sb strings.Builder
	sb.WriteString("id\tlabel\tcluster\tweight<Occurrences>\tweight<Links>\tweight<Total link strength>\n")
	for _, n := range net.Nodes {
		fmt.Fprintf(&sb, "%d\t%s\t%d\t%d\t%d\t%d\n", n.ID, n.Label, n.Cluster, n.Count, n.Links, n.Weight)
	}
	return sb.String()
}

// VOSviewerNetwork renders the "network" file (tab-separated id1 id2 strength).
func (net *Network) VOSviewerNetwork() string {
	var sb strings.Builder
	for _, e := range net.Edges {
		fmt.Fprintf(&sb, "%d\t%d\t%d\n", e.From, e.To, e.Weight)
	}
	return sb.String()
}
