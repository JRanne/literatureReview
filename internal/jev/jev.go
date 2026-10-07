// Package jev calls TypeSafe's Jev "System One" decision model to give an
// independent, calibrated relevance assessment for each candidate paper.
// Jev returns scores and probabilities rather than prose, so it serves as the
// second screener next to Claude, mirroring two-reviewer screening.
package jev

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"literaturereview/internal/model"
)

// DefaultURL is the official TypeSafe endpoint.
const DefaultURL = "https://api.typesafe.ai/v1/systemone"

// Client talks to the Jev API.
type Client struct {
	APIKey      string
	Model       string
	URL         string
	HTTP        *http.Client
	Concurrency int
}

// New builds a client; model defaults to jev-latest.
func New(apiKey, modelID, url string) *Client {
	if modelID == "" {
		modelID = "jev-latest"
	}
	if url == "" {
		url = DefaultURL
	}
	return &Client{APIKey: apiKey, Model: modelID, URL: url, HTTP: &http.Client{Timeout: 60 * time.Second}, Concurrency: 8}
}

// Enabled reports whether a key is configured.
func (c *Client) Enabled() bool { return c != nil && c.APIKey != "" }

// Relevance rubric: ordered levels, index 0..4.
var relevanceLevels = []string{
	"Not relevant: the paper is about something else and does not address the research question",
	"Tangential: only loosely related; mentions the topic in passing or in a different setting",
	"Partially relevant: addresses one aspect of the research question or a closely adjacent question",
	"Relevant: directly addresses the research question in the intended context",
	"Highly relevant: central to the research question, in the intended context, and clearly meets the inclusion criteria",
}

type question struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions"`
	Criteria     any    `json:"criteria,omitempty"`
}

type request struct {
	Model     string              `json:"model"`
	State     map[string]any      `json:"state"`
	Questions map[string]question `json:"questions"`
}

type answer struct {
	Type          string             `json:"type"`
	Score         float64            `json:"score"`
	Noul          float64            `json:"noul"`
	Confidence    float64            `json:"confidence"`
	Probabilities map[string]float64 `json:"probabilities"`
}

type response struct {
	Model   string            `json:"model"`
	Answers map[string]answer `json:"answers"`
	Error   any               `json:"error,omitempty"`
	Detail  any               `json:"detail,omitempty"`
	Message string            `json:"message,omitempty"`
}

// Assess scores one paper. It fills and returns a JevAssessment; on API
// failure the assessment carries the error and a zero verdict.
func (c *Client) Assess(ctx context.Context, p *model.Project, pp *model.Paper) *model.JevAssessment {
	a := &model.JevAssessment{Model: c.Model}
	abs := pp.Abstract
	if abs == "" {
		abs = "(no abstract available; only title and venue are known)"
	}
	if len(abs) > 6000 {
		abs = abs[:6000] + "…"
	}
	state := map[string]any{
		"research_question":  p.Question,
		"inclusion_criteria": orNone(p.Inclusion),
		"exclusion_criteria": orNone(p.Exclusion),
		"paper": map[string]any{
			"title":    pp.Title,
			"authors":  strings.Join(pp.Authors, ", "),
			"year":     pp.Year,
			"venue":    pp.Venue,
			"abstract": abs,
		},
	}
	qs := map[string]question{
		"relevance": {Type: "score", Instructions: "How relevant is the paper to the research question, judged from its title, venue and abstract?", Criteria: relevanceLevels},
		"include": {Type: "noul", Instructions: "Should this paper be included for full-text review in a systematic literature review on the research question, taking the inclusion criteria into account?",
			Criteria: map[string]string{"true": "The paper is on topic and plausibly meets the inclusion criteria.", "false": "The paper is off topic or clearly fails the inclusion criteria."}},
	}
	if p.Exclusion != "" {
		qs["exclude"] = question{Type: "noul", Instructions: "Does the paper match any of the exclusion criteria?",
			Criteria: map[string]string{"true": "At least one exclusion criterion applies to this paper.", "false": "No exclusion criterion applies."}}
	}
	resp, err := c.call(ctx, request{Model: c.Model, State: state, Questions: qs})
	if err != nil {
		a.Error = err.Error()
		return a
	}
	if resp.Model != "" {
		a.Model = resp.Model
	}
	rel, ok1 := resp.Answers["relevance"]
	inc, ok2 := resp.Answers["include"]
	if !ok1 || !ok2 {
		a.Error = "response missing answers"
		return a
	}
	a.Relevance = rel.Score
	a.Confidence = rel.Confidence
	a.Include = inc.Noul
	if ex, ok := resp.Answers["exclude"]; ok {
		a.Exclude = ex.Noul
	}
	a.Score, a.Verdict = Combine(a.Relevance, a.Include, a.Exclude)
	return a
}

// Combine maps Jev's answers to the app's 0–100 score and verdict:
// half from the relevance level (0–4 → 0–1), half from the include
// probability; a confident exclusion-criterion match caps the score at 39.
func Combine(relevance, include, exclude float64) (int, model.Verdict) {
	rel := math.Max(0, math.Min(1, relevance/4))
	s := 100 * (0.5*rel + 0.5*include)
	if exclude >= 0.7 && s > 39 {
		s = 39
	}
	score := int(math.Round(s))
	switch {
	case score >= 70:
		return score, model.VerdictKeep
	case score < 40:
		return score, model.VerdictDrop
	default:
		return score, model.VerdictUnsure
	}
}

func (c *Client) call(ctx context.Context, req request) (*response, error) {
	body, _ := json.Marshal(req)
	var lastErr error
	wait := time.Second
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(wait):
			}
			wait *= 3
		}
		hr, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		hr.Header.Set("Authorization", "Bearer "+c.APIKey)
		hr.Header.Set("Content-Type", "application/json")
		res, err := c.HTTP.Do(hr)
		if err != nil {
			lastErr = err
			continue
		}
		data, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
		res.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		if res.StatusCode == 429 || res.StatusCode == 529 || res.StatusCode >= 500 {
			lastErr = fmt.Errorf("jev: HTTP %d: %s", res.StatusCode, truncate(string(data), 200))
			if ra, err := strconv.Atoi(res.Header.Get("Retry-After")); err == nil && ra > 0 && ra <= 60 {
				wait = time.Duration(ra) * time.Second
			}
			continue
		}
		var out response
		if err := json.Unmarshal(data, &out); err != nil {
			return nil, fmt.Errorf("jev: decode: %w (%s)", err, truncate(string(data), 200))
		}
		if res.StatusCode != 200 {
			msg := out.Message
			if msg == "" {
				msg = truncate(string(data), 300)
			}
			if res.StatusCode == 401 {
				return nil, errors.New("jev: HTTP 401 invalid API key")
			}
			return nil, fmt.Errorf("jev: HTTP %d: %s", res.StatusCode, msg)
		}
		return &out, nil
	}
	return nil, lastErr
}

// AssessAll runs Assess over papers concurrently and stores the result in
// each paper's Jev field. progress is called after each paper.
func (c *Client) AssessAll(ctx context.Context, p *model.Project, papers []*model.Paper, progress func(done, total int)) error {
	n := c.Concurrency
	if n <= 0 {
		n = 4
	}
	sem := make(chan struct{}, n)
	var wg sync.WaitGroup
	var mu sync.Mutex
	done := 0
	for _, pp := range papers {
		pp := pp
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return ctx.Err()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			pp.Jev = c.Assess(ctx, p, pp)
			mu.Lock()
			done++
			d := done
			mu.Unlock()
			if progress != nil {
				progress(d, len(papers))
			}
		}()
	}
	wg.Wait()
	return ctx.Err()
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "none specified"
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
