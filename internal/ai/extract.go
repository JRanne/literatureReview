package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"literaturereview/internal/model"
)

const extractBatch = 6
const extractConcurrency = 3

type extraction struct {
	ID           string   `json:"id"`
	Objective    string   `json:"objective"`
	Method       string   `json:"method"`
	Context      string   `json:"context"`
	Findings     string   `json:"findings"`
	Theory       string   `json:"theory"`
	Contribution string   `json:"contribution"`
	Limitations  string   `json:"limitations"`
	Relevance    string   `json:"relevance"`
	Keywords     []string `json:"keywords"`
}

// ExtractPapers fills p.Extract for each paper with the key aspects needed
// for a master literature table. evidence maps paper ID to the text to read
// (full-text excerpt or abstract); papers absent from it use their abstract.
// progress is called after each batch with (done, total).
func (c *Client) ExtractPapers(ctx context.Context, p *model.Project, papers []*model.Paper, evidence map[string]Evidence, progress func(done, total int)) error {
	system := `You are a systematic-review data-extraction assistant` + disciplineClause(p) + `. For each paper you receive (bibliographic data plus either the abstract or an excerpt of the full text) you fill a structured extraction form. Be concrete and faithful to the source: when something is not stated, write "not stated in source" rather than guessing. Keep each field to one or two sentences. Output strictly valid JSON.`
	total := len(papers)
	var batches [][]*model.Paper
	for start := 0; start < total; start += extractBatch {
		end := start + extractBatch
		if end > total {
			end = total
		}
		batches = append(batches, papers[start:end])
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sem := make(chan struct{}, extractConcurrency)
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
			if err := c.extractBatch(ctx, p, system, batch, evidence); err != nil {
				errs <- err
				cancel()
				return
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

func (c *Client) extractBatch(ctx context.Context, p *model.Project, system string, batch []*model.Paper, evidence map[string]Evidence) error {
	var sb strings.Builder
	sb.WriteString(projectContext(p))
	sb.WriteString("\nPapers:\n")
	for _, pp := range batch {
		label, text := "Abstract", pp.Abstract
		if e, ok := evidence[pp.ID]; ok && e.Text != "" {
			text = e.Text
			if e.FullText {
				label = "Full text (excerpt)"
			}
		}
		if text == "" {
			text = "(no abstract available)"
		}
		fmt.Fprintf(&sb, "\n[id=%s]\nTitle: %s\nAuthors: %s\nYear: %d\nVenue: %s\n%s: %s\n", pp.ID, pp.Title, strings.Join(pp.Authors, ", "), pp.Year, pp.Venue, label, truncate(text, 9000))
	}
	sb.WriteString(`
For EACH paper respond with one object in a JSON array:
{"id": "<id>",
 "objective": "research aim / question of the paper",
 "method": "research design and methods (e.g. survey n=..., case study, experiment, design science, conceptual, literature review) and level of analysis (individual, team, organisation, ecosystem)",
 "context": "empirical setting, sample, country/sector, data sources",
 "findings": "the key findings or main arguments",
 "theory": "theoretical lens or framework used, if any (name it as the field does, e.g. TOE, socio-technical systems, IS success, design science)",
 "contribution": "stated contribution to knowledge or practice",
 "limitations": "limitations acknowledged or evident from the abstract",
 "relevance": "how the paper speaks to the research question above, in one sentence",
 "keywords": ["3-6 short topic keywords"]}
Respond with ONLY the JSON array.`)
	text, err := c.Complete(ctx, system, sb.String(), 6000)
	if err != nil {
		return err
	}
	var out []extraction
	if err := json.Unmarshal([]byte(ExtractJSON(text)), &out); err != nil {
		return fmt.Errorf("parse extraction JSON: %w (raw: %s)", err, truncate(text, 400))
	}
	byID := map[string]extraction{}
	for _, e := range out {
		byID[e.ID] = e
	}
	for _, pp := range batch {
		e, ok := byID[pp.ID]
		if !ok {
			pp.Extract = &model.Extraction{Relevance: "The extractor returned no entry for this paper.", Model: c.ModelLabel()}
			continue
		}
		pp.Extract = &model.Extraction{
			Objective: e.Objective, Method: e.Method, Context: e.Context, Findings: e.Findings,
			Theory: e.Theory, Contribution: e.Contribution, Limitations: e.Limitations,
			Relevance: e.Relevance, Keywords: e.Keywords, Model: c.ModelLabel(),
		}
	}
	return nil
}
