package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"literaturereview/internal/model"
)

// ProposeConcepts derives 8–14 concept-matrix columns from the included
// papers' extracted aspects and keywords (Webster & Watson style).
func (c *Client) ProposeConcepts(ctx context.Context, p *model.Project, papers []*model.Paper, keywordClusters []string) ([]model.Concept, error) {
	system := `You are a scholar building a concept-centric literature matrix (Webster & Watson, 2002)` + disciplineClause(p) + `. Concepts are the analytical categories the review will organise the literature by: theoretical constructs, phenomena, design elements, outcomes, contexts. They must be mutually distinct, each supported by several papers, and named as the field names them. Output strictly valid JSON.`
	var sb strings.Builder
	sb.WriteString(projectContext(p))
	if len(keywordClusters) > 0 {
		sb.WriteString("\nKeyword clusters found across the corpus:\n")
		for _, k := range keywordClusters {
			sb.WriteString("- " + k + "\n")
		}
	}
	fmt.Fprintf(&sb, "\nIncluded papers (%d). For each: title, objective, key findings, theory, keywords.\n", len(papers))
	for i, pp := range papers {
		fmt.Fprintf(&sb, "\n%d. %s (%d)\n", i+1, pp.Title, pp.Year)
		if e := pp.Extract; e != nil {
			fmt.Fprintf(&sb, "   Objective: %s\n   Findings: %s\n   Theory: %s\n   Keywords: %s\n", truncate(e.Objective, 300), truncate(e.Findings, 300), truncate(e.Theory, 120), strings.Join(e.Keywords, ", "))
		} else {
			fmt.Fprintf(&sb, "   Abstract: %s\n", truncate(pp.Abstract, 500))
		}
	}
	sb.WriteString(`
Propose between 8 and 14 concepts that together cover the corpus and map onto the research question(s). Prefer concepts that discriminate between papers (avoid one that every paper matches). Respond with ONLY a JSON array: [{"name": "<2–5 word concept name>", "definition": "<one sentence: what counts as addressing this concept>"}]`)
	text, err := c.Complete(ctx, system, sb.String(), 3000)
	if err != nil {
		return nil, err
	}
	var out []model.Concept
	if err := json.Unmarshal([]byte(ExtractJSON(text)), &out); err != nil {
		return nil, fmt.Errorf("parse concepts JSON: %w (raw: %s)", err, truncate(text, 300))
	}
	var clean []model.Concept
	seen := map[string]bool{}
	for _, cpt := range out {
		n := strings.TrimSpace(cpt.Name)
		if n == "" || seen[strings.ToLower(n)] {
			continue
		}
		seen[strings.ToLower(n)] = true
		clean = append(clean, model.Concept{Name: n, Definition: strings.TrimSpace(cpt.Definition)})
	}
	if len(clean) < 3 {
		return nil, fmt.Errorf("too few concepts returned (%d)", len(clean))
	}
	return clean, nil
}

const assignBatch = 10

// AssignConcepts marks, per paper, which concepts it addresses, with a
// short note each. Runs batches concurrently; sets Paper.Concepts.
func (c *Client) AssignConcepts(ctx context.Context, p *model.Project, concepts []model.Concept, papers []*model.Paper, progress func(done, total int)) error {
	system := `You are filling a concept matrix for a literature review` + disciplineClause(p) + `. For each paper, decide which of the given concepts it substantively addresses (not merely mentions), based on its extracted aspects and abstract, and give a short note (max 15 words) on how. Be selective: most papers address 2–5 concepts. Output strictly valid JSON.`
	var conceptList strings.Builder
	for _, cpt := range concepts {
		fmt.Fprintf(&conceptList, "- %s: %s\n", cpt.Name, cpt.Definition)
	}
	total := len(papers)
	var batches [][]*model.Paper
	for start := 0; start < total; start += assignBatch {
		end := start + assignBatch
		if end > total {
			end = total
		}
		batches = append(batches, papers[start:end])
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sem := make(chan struct{}, 3)
	errs := make(chan error, len(batches))
	var mu sync.Mutex
	done := 0
	var wg sync.WaitGroup
	for _, batch := range batches {
		batch := batch
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			var sb strings.Builder
			sb.WriteString(projectContext(p))
			sb.WriteString("\nConcepts:\n" + conceptList.String() + "\nPapers:\n")
			for _, pp := range batch {
				fmt.Fprintf(&sb, "\n[id=%s] %s (%d)\n", pp.ID, pp.Title, pp.Year)
				if e := pp.Extract; e != nil {
					fmt.Fprintf(&sb, "Objective: %s\nMethod: %s\nFindings: %s\nTheory: %s\nContribution: %s\n", truncate(e.Objective, 400), truncate(e.Method, 200), truncate(e.Findings, 500), truncate(e.Theory, 150), truncate(e.Contribution, 250))
				} else {
					fmt.Fprintf(&sb, "Abstract: %s\n", truncate(pp.Abstract, 1500))
				}
			}
			sb.WriteString(`
Respond with ONLY a JSON array, one object per paper:
{"id": "<id>", "concepts": [{"name": "<exact concept name from the list>", "note": "<max 15 words>"}]}`)
			text, err := c.Complete(ctx, system, sb.String(), 4000)
			if err != nil {
				errs <- err
				cancel()
				return
			}
			var out []struct {
				ID       string `json:"id"`
				Concepts []struct {
					Name string `json:"name"`
					Note string `json:"note"`
				} `json:"concepts"`
			}
			if err := json.Unmarshal([]byte(ExtractJSON(text)), &out); err != nil {
				errs <- fmt.Errorf("parse concept assignment JSON: %w (raw: %s)", err, truncate(text, 300))
				cancel()
				return
			}
			valid := map[string]string{}
			for _, cpt := range concepts {
				valid[strings.ToLower(cpt.Name)] = cpt.Name
			}
			byID := map[string][]model.ConceptHit{}
			for _, o := range out {
				var hits []model.ConceptHit
				seen := map[string]bool{}
				for _, h := range o.Concepts {
					name, ok := valid[strings.ToLower(strings.TrimSpace(h.Name))]
					if !ok || seen[name] {
						continue
					}
					seen[name] = true
					hits = append(hits, model.ConceptHit{Name: name, Note: truncate(h.Note, 120)})
				}
				byID[o.ID] = hits
			}
			for _, pp := range batch {
				if hits, ok := byID[pp.ID]; ok {
					pp.Concepts = hits
				} else {
					pp.Concepts = []model.ConceptHit{}
				}
			}
			mu.Lock()
			done += len(batch)
			d := done
			mu.Unlock()
			if progress != nil {
				progress(d, total)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			return err
		}
	}
	return ctx.Err()
}
