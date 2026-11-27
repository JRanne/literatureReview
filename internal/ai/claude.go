// Package ai wraps the Claude Messages API for the review pipeline.
package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"literaturereview/internal/model"
)

const (
	defaultBaseURL = "https://api.anthropic.com"
	apiVersion     = "2023-06-01"
)

// Backend produces one assistant reply for a system + user prompt.
type Backend interface {
	// Name is a short label for the UI, e.g. "api" or "claude-cli".
	Name() string
	// ModelLabel describes which model will answer.
	ModelLabel() string
	Complete(ctx context.Context, system, user string, maxTokens int) (string, error)
	// WithModel returns a copy of the backend answering with another model
	// (empty keeps the current one).
	WithModel(model string) Backend
}

// WithModel returns a client whose backend uses the given model, or the same
// client when model is empty.
func (c *Client) WithModel(model string) *Client {
	if !c.Enabled() || model == "" {
		return c
	}
	return &Client{backend: c.backend.WithModel(c.catalog.Resolve(model)), catalog: c.catalog}
}

// WithCatalog returns a client that offers and resolves models through cat.
func (c *Client) WithCatalog(cat *Catalog) *Client {
	return &Client{backend: c.backend, catalog: cat}
}

// Catalog returns the models this client can be switched to. It is never
// nil-unsafe: a nil catalog offers the built-in list.
func (c *Client) Catalog() *Catalog {
	if c == nil {
		return nil
	}
	return c.catalog
}

// Client runs the review prompts against whichever backend is configured.
type Client struct {
	backend Backend
	catalog *Catalog
}

// New returns a client over the given backend (nil means AI is disabled).
func New(b Backend) *Client { return &Client{backend: b} }

// NewAPI is a convenience for the Messages API backend.
func NewAPI(apiKey, model string) *Client {
	if apiKey == "" {
		return &Client{}
	}
	return &Client{backend: &APIBackend{APIKey: apiKey, Model: model, HTTP: &http.Client{Timeout: 5 * time.Minute}}}
}

// Enabled reports whether a backend is configured.
func (c *Client) Enabled() bool { return c != nil && c.backend != nil }

// BackendName returns the backend label or "none".
func (c *Client) BackendName() string {
	if !c.Enabled() {
		return "none"
	}
	return c.backend.Name()
}

// ModelLabel returns the backend's model description or "".
func (c *Client) ModelLabel() string {
	if !c.Enabled() {
		return ""
	}
	return c.backend.ModelLabel()
}

// Complete sends one user turn and returns the assistant text.
func (c *Client) Complete(ctx context.Context, system, user string, maxTokens int) (string, error) {
	if !c.Enabled() {
		return "", errors.New("no AI backend configured (set ANTHROPIC_API_KEY, or install and log in to the claude CLI)")
	}
	return c.backend.Complete(ctx, system, user, maxTokens)
}

// APIBackend talks to the Claude Messages API with an API key.
type APIBackend struct {
	APIKey string
	Model  string
	HTTP   *http.Client
	// BaseURL overrides https://api.anthropic.com (tests).
	BaseURL string
}

func (b *APIBackend) baseURL() string {
	if b.BaseURL != "" {
		return strings.TrimRight(b.BaseURL, "/")
	}
	return defaultBaseURL
}

func (b *APIBackend) httpClient() *http.Client {
	if b.HTTP != nil {
		return b.HTTP
	}
	return http.DefaultClient
}

func (b *APIBackend) Name() string       { return "api" }
func (b *APIBackend) ModelLabel() string { return b.Model }
func (b *APIBackend) WithModel(m string) Backend {
	if m == "" {
		return b
	}
	c := *b
	c.Model = m
	return &c
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type request struct {
	Model     string    `json:"model"`
	MaxTokens int       `json:"max_tokens"`
	System    string    `json:"system,omitempty"`
	Messages  []message `json:"messages"`
}

type response struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
	Error      *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// Complete implements Backend.
func (c *APIBackend) Complete(ctx context.Context, system, user string, maxTokens int) (string, error) {
	body, _ := json.Marshal(request{
		Model:     c.Model,
		MaxTokens: maxTokens,
		System:    system,
		Messages:  []message{{Role: "user", Content: user}},
	})
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(time.Duration(attempt*attempt) * 2 * time.Second):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL()+"/v1/messages", bytes.NewReader(body))
		if err != nil {
			return "", err
		}
		req.Header.Set("content-type", "application/json")
		req.Header.Set("x-api-key", c.APIKey)
		req.Header.Set("anthropic-version", apiVersion)
		resp, err := c.httpClient().Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, 20<<20))
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode == 429 || resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("claude api: HTTP %d: %s", resp.StatusCode, truncate(string(data), 300))
			continue
		}
		var out response
		if err := json.Unmarshal(data, &out); err != nil {
			return "", fmt.Errorf("claude api: decode: %w", err)
		}
		if resp.StatusCode != 200 {
			msg := truncate(string(data), 300)
			if out.Error != nil {
				msg = out.Error.Type + ": " + out.Error.Message
			}
			return "", fmt.Errorf("claude api: HTTP %d: %s", resp.StatusCode, msg)
		}
		var sb strings.Builder
		for _, blk := range out.Content {
			if blk.Type == "text" {
				sb.WriteString(blk.Text)
			}
		}
		return sb.String(), nil
	}
	return "", lastErr
}

// ExtractJSON pulls the first JSON array or object out of free text,
// tolerating markdown code fences and surrounding prose.
func ExtractJSON(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "```"); i >= 0 {
		rest := s[i+3:]
		if nl := strings.Index(rest, "\n"); nl >= 0 {
			rest = rest[nl+1:]
		}
		if j := strings.LastIndex(rest, "```"); j >= 0 {
			rest = rest[:j]
		}
		s = strings.TrimSpace(rest)
	}
	startObj, startArr := strings.Index(s, "{"), strings.Index(s, "[")
	start := -1
	var closer byte
	switch {
	case startArr >= 0 && (startObj < 0 || startArr < startObj):
		start, closer = startArr, ']'
	case startObj >= 0:
		start, closer = startObj, '}'
	default:
		return s
	}
	end := strings.LastIndexByte(s, closer)
	if end < start {
		return s[start:]
	}
	return s[start : end+1]
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// SourceInfo describes a database to the query generator.
type SourceInfo struct {
	Name   string
	Label  string
	Syntax string
}

func projectContext(p *model.Project) string {
	var sb strings.Builder
	if p.Discipline != "" {
		fmt.Fprintf(&sb, "Discipline and audience: %s. Judge relevance, choose vocabulary and write as a researcher in this field would (its constructs, theories, venues and conventions).\n", p.Discipline)
	}
	fmt.Fprintf(&sb, "Research question: %s\n", p.Question)
	if p.Inclusion != "" {
		fmt.Fprintf(&sb, "Inclusion criteria: %s\n", p.Inclusion)
	}
	if p.Exclusion != "" {
		fmt.Fprintf(&sb, "Exclusion criteria: %s\n", p.Exclusion)
	}
	if p.YearFrom > 0 || p.YearTo > 0 {
		fmt.Fprintf(&sb, "Publication years: %d–%d\n", p.YearFrom, p.YearTo)
	}
	return sb.String()
}

// GenerateQueries asks Claude for 2–3 search strings per database.
func (c *Client) GenerateQueries(ctx context.Context, p *model.Project, sources []SourceInfo) ([]model.Query, error) {
	var sb strings.Builder
	sb.WriteString(projectContext(p))
	sb.WriteString("\nDatabases and their query syntax:\n")
	for _, s := range sources {
		fmt.Fprintf(&sb, "- %s (%s): %s\n", s.Name, s.Label, s.Syntax)
	}
	sb.WriteString(`
Produce 2 to 3 complementary search queries for EACH database listed above, written in that database's syntax.
Cover synonyms and adjacent terminology; vary breadth (one precise, one broader).
Respond with ONLY a JSON array of objects: {"source": "<database name exactly as given>", "query": "<query string>", "rationale": "<short reason>"}.`)
	system := "You are an expert research librarian designing systematic literature search strategies" + disciplineClause(p) + ". You output strictly valid JSON when asked."
	text, err := c.Complete(ctx, system, sb.String(), 4000)
	if err != nil {
		return nil, err
	}
	var out []model.Query
	if err := json.Unmarshal([]byte(ExtractJSON(text)), &out); err != nil {
		return nil, fmt.Errorf("parse query JSON: %w (raw: %s)", err, truncate(text, 400))
	}
	valid := map[string]bool{}
	for _, s := range sources {
		valid[s.Name] = true
	}
	var filtered []model.Query
	for _, q := range out {
		q.Source = strings.ToLower(strings.TrimSpace(q.Source))
		q.Query = strings.TrimSpace(q.Query)
		if valid[q.Source] && q.Query != "" {
			filtered = append(filtered, q)
		}
	}
	if len(filtered) == 0 {
		return nil, fmt.Errorf("no usable queries returned (raw: %s)", truncate(text, 400))
	}
	return filtered, nil
}

// QueryStat summarises how a previous query performed.
type QueryStat struct {
	Source    string
	Query     string
	Total     int
	Retrieved int
	Relevant  int
}

// ExpandQueries designs additional, broader queries informed by what the
// previous rounds found: which queries yielded relevant papers and what
// terminology the best-ranked papers actually use.
func (c *Client) ExpandQueries(ctx context.Context, p *model.Project, sources []SourceInfo, prev []QueryStat, good, bad []*model.Paper, kept, target int) ([]model.Query, error) {
	var sb strings.Builder
	sb.WriteString(projectContext(p))
	fmt.Fprintf(&sb, "\nSo far %d papers are included; the goal is about %d. The previous queries and their yield (hits reported by the database / retrieved / judged relevant):\n", kept, target)
	for _, q := range prev {
		fmt.Fprintf(&sb, "- [%s] %q → %d / %d / %d relevant\n", q.Source, q.Query, q.Total, q.Retrieved, q.Relevant)
	}
	if len(good) > 0 {
		sb.WriteString("\nBest-ranked papers found so far (learn the terminology the relevant literature actually uses):\n")
		for i, pp := range good {
			if i >= 15 {
				break
			}
			fmt.Fprintf(&sb, "- %s (%d, %s)\n", pp.Title, pp.Year, pp.Venue)
		}
	}
	if len(bad) > 0 {
		sb.WriteString("\nExamples of irrelevant papers the previous queries pulled in (avoid this direction):\n")
		for i, pp := range bad {
			if i >= 8 {
				break
			}
			fmt.Fprintf(&sb, "- %s\n", pp.Title)
		}
	}
	sb.WriteString("\nDatabases and their query syntax:\n")
	for _, s := range sources {
		fmt.Fprintf(&sb, "- %s (%s): %s\n", s.Name, s.Label, s.Syntax)
	}
	sb.WriteString(`
Design 2 to 3 NEW queries per database that broaden recall without drifting off topic:
- use the vocabulary of the best-ranked papers and standard academic terms for the concepts in the question (the question may use non-standard or vendor terminology; translate it);
- try synonyms, adjacent concepts, theory names, and different levels of abstraction;
- do not repeat or trivially rephrase previous queries;
- if a database returned few hits, loosen its query (fewer AND terms, more OR synonyms).
Respond with ONLY a JSON array of objects: {"source": "<database name exactly as given>", "query": "<query string>", "rationale": "<short reason>"}.`)
	system := "You are an expert research librarian iteratively refining a systematic literature search to reach adequate recall" + disciplineClause(p) + ". You output strictly valid JSON when asked."
	text, err := c.Complete(ctx, system, sb.String(), 4000)
	if err != nil {
		return nil, err
	}
	var out []model.Query
	if err := json.Unmarshal([]byte(ExtractJSON(text)), &out); err != nil {
		return nil, fmt.Errorf("parse query JSON: %w (raw: %s)", err, truncate(text, 400))
	}
	valid := map[string]bool{}
	for _, s := range sources {
		valid[s.Name] = true
	}
	seen := map[string]bool{}
	for _, q := range prev {
		seen[q.Source+"\x00"+strings.ToLower(strings.TrimSpace(q.Query))] = true
	}
	var filtered []model.Query
	for _, q := range out {
		q.Source = strings.ToLower(strings.TrimSpace(q.Source))
		q.Query = strings.TrimSpace(q.Query)
		key := q.Source + "\x00" + strings.ToLower(q.Query)
		if valid[q.Source] && q.Query != "" && !seen[key] {
			seen[key] = true
			filtered = append(filtered, q)
		}
	}
	if len(filtered) == 0 {
		return nil, fmt.Errorf("no new queries returned (raw: %s)", truncate(text, 400))
	}
	return filtered, nil
}

// Screening is the AI's assessment of one paper.
type Screening struct {
	ID        string `json:"id"`
	Score     int    `json:"score"`
	Verdict   string `json:"verdict"`
	Rationale string `json:"rationale"`
}

const screenBatch = 20

// screenConcurrency bounds how many screening batches run at once.
const screenConcurrency = 3

// ScreenPapers scores every paper against the research question in batches
// of screenBatch, running up to screenConcurrency batches concurrently.
// progress is called after each batch with (done, total).
func (c *Client) ScreenPapers(ctx context.Context, p *model.Project, papers []*model.Paper, progress func(done, total int)) error {
	system := `You are a meticulous systematic-review screener` + disciplineClause(p) + `. Given a research question, criteria and candidate papers (title, venue, year, abstract), you rate each paper's relevance for full-text review. Be calibrated: score 0-100 where >=70 means clearly relevant, 40-69 borderline, <40 not relevant. Verdict is "keep" (>=70), "unsure" (40-69) or "drop" (<40). Output strictly valid JSON.`
	total := len(papers)
	var batches [][]*model.Paper
	for start := 0; start < total; start += screenBatch {
		end := start + screenBatch
		if end > total {
			end = total
		}
		batches = append(batches, papers[start:end])
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sem := make(chan struct{}, screenConcurrency)
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
			if err := c.screenBatch(ctx, p, system, batch); err != nil {
				errs <- err
				cancel()
				return
			}
			// Report under the lock: callers write progress to the store and
			// must see calls one at a time, in increasing order.
			mu.Lock()
			done += len(batch)
			if progress != nil {
				progress(done, total)
			}
			mu.Unlock()
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

func (c *Client) screenBatch(ctx context.Context, p *model.Project, system string, batch []*model.Paper) error {
	var sb strings.Builder
	sb.WriteString(projectContext(p))
	sb.WriteString("\nCandidate papers:\n")
	for _, pp := range batch {
		abs := pp.Abstract
		if abs == "" {
			abs = "(no abstract available; judge from title and venue)"
		}
		fmt.Fprintf(&sb, "\n[id=%s]\nTitle: %s\nYear: %d\nVenue: %s\nAbstract: %s\n", pp.ID, pp.Title, pp.Year, pp.Venue, truncate(abs, 2500))
	}
	sb.WriteString(`
Respond with ONLY a JSON array, one object per paper, in the form:
{"id": "<id>", "score": <0-100>, "verdict": "keep"|"unsure"|"drop", "rationale": "<one sentence>"}`)
	text, err := c.Complete(ctx, system, sb.String(), 4000)
	if err != nil {
		return err
	}
	var out []Screening
	if err := json.Unmarshal([]byte(ExtractJSON(text)), &out); err != nil {
		return fmt.Errorf("parse screening JSON: %w (raw: %s)", err, truncate(text, 400))
	}
	byID := map[string]Screening{}
	for _, s := range out {
		byID[s.ID] = s
	}
	for _, pp := range batch {
		s, ok := byID[pp.ID]
		if !ok {
			pp.AIVerdict = model.VerdictUnsure
			pp.AIRationale = "The screener returned no assessment for this paper."
			pp.AIScore = 50
			continue
		}
		pp.AIScore = clamp(s.Score, 0, 100)
		pp.AIRationale = s.Rationale
		switch strings.ToLower(s.Verdict) {
		case "keep":
			pp.AIVerdict = model.VerdictKeep
		case "drop":
			pp.AIVerdict = model.VerdictDrop
		default:
			pp.AIVerdict = model.VerdictUnsure
		}
	}
	return nil
}

// Evidence is what the model gets to read about a paper.
type Evidence struct {
	Paper *model.Paper
	// Text is the full-text excerpt when available, else the abstract.
	Text     string
	FullText bool
}

// adjudication is one third-reviewer decision.
type adjudication struct {
	ID         string  `json:"id"`
	Decision   string  `json:"decision"`
	Confidence float64 `json:"confidence"`
	Reason     string  `json:"reason"`
}

const adjudicateBatch = 10

// Adjudicate acts as a third reviewer on papers the screeners left
// undecided: it must commit to keep or drop unless the abstract genuinely
// lacks the information, and reports a confidence. Results are stored in
// Paper.Adjudication; decisions are not applied here.
func (c *Client) Adjudicate(ctx context.Context, p *model.Project, papers []*model.Paper, strict bool, progress func(done, total int)) error {
	standard := `At title/abstract stage the standard is inclusive: when a paper plausibly addresses the research question and no exclusion criterion clearly applies, keep it.`
	if strict {
		standard = `The standard is strict: keep only papers whose abstract shows they directly address the research question in the intended context and meet the inclusion criteria; when in doubt, drop.`
	}
	system := `You are the senior third reviewer in a systematic literature review` + disciplineClause(p) + `. Two screeners could not agree, or were unsure, about the papers below. You read each abstract carefully against the research question and the inclusion/exclusion criteria and COMMIT to "keep" or "drop". ` + standard + ` Answer "unsure" only when the available text genuinely does not allow a judgement (e.g. no abstract). Give a confidence between 0 and 1 and a one-sentence reason. Output strictly valid JSON.`
	total := len(papers)
	for start := 0; start < total; start += adjudicateBatch {
		end := start + adjudicateBatch
		if end > total {
			end = total
		}
		batch := papers[start:end]
		var sb strings.Builder
		sb.WriteString(projectContext(p))
		sb.WriteString("\nUndecided papers with the earlier screeners' assessments:\n")
		for _, pp := range batch {
			abs := pp.Abstract
			if abs == "" {
				abs = "(no abstract available)"
			}
			fmt.Fprintf(&sb, "\n[id=%s]\nTitle: %s\nYear: %d\nVenue: %s\nScreener A: %s (%d/100) — %s\n", pp.ID, pp.Title, pp.Year, pp.Venue, pp.AIVerdict, pp.AIScore, pp.AIRationale)
			if pp.HasJev() {
				fmt.Fprintf(&sb, "Screener B: %s (%d/100) — %s\n", pp.Jev.Verdict, pp.Jev.Score, pp.Jev.Detail())
			}
			fmt.Fprintf(&sb, "Abstract: %s\n", truncate(abs, 3000))
		}
		sb.WriteString(`
Respond with ONLY a JSON array, one object per paper:
{"id": "<id>", "decision": "keep"|"drop"|"unsure", "confidence": <0-1>, "reason": "<one sentence>"}`)
		text, err := c.Complete(ctx, system, sb.String(), 4000)
		if err != nil {
			return err
		}
		var out []adjudication
		if err := json.Unmarshal([]byte(ExtractJSON(text)), &out); err != nil {
			return fmt.Errorf("parse adjudication JSON: %w (raw: %s)", err, truncate(text, 400))
		}
		byID := map[string]adjudication{}
		for _, a := range out {
			byID[a.ID] = a
		}
		for _, pp := range batch {
			a, ok := byID[pp.ID]
			if !ok {
				continue
			}
			v := model.VerdictUnsure
			switch strings.ToLower(a.Decision) {
			case "keep":
				v = model.VerdictKeep
			case "drop":
				v = model.VerdictDrop
			}
			if a.Confidence < 0 {
				a.Confidence = 0
			}
			if a.Confidence > 1 {
				a.Confidence = 1
			}
			pp.Adjudication = &model.Adjudication{Decision: v, Confidence: a.Confidence, Reason: a.Reason, Model: c.ModelLabel()}
		}
		if progress != nil {
			progress(end, total)
		}
	}
	return nil
}

// Synthesize drafts an academic background / related-work section from the
// included papers. It cites with the papers' BibKeys in pandoc syntax
// ([@key]; [@key1; @key2]) so the text compiles against the .bib export.
// method is a factual description of how the literature was identified
// (databases, queries, rounds, candidates screened, screeners); it is given
// to the model so the opening paragraph can report it accurately.
func (c *Client) Synthesize(ctx context.Context, p *model.Project, ev []Evidence, method string) (string, error) {
	field := "information-systems"
	if p.Discipline != "" {
		field = p.Discipline
	}
	system := `You are an experienced scholar in ` + field + ` writing the Background (related work) section of a journal article. You write in formal academic English: precise, hedged where evidence is limited, synthesising across studies rather than summarising them one by one, and you never attribute a claim to a source that does not support it. You cite exclusively with the provided citation keys using pandoc syntax: [@key] or [@key1; @key2], or "Author et al. [@key]" in narrative position. You never invent sources, findings, numbers or quotations. Output Markdown only.`
	var sb strings.Builder
	sb.WriteString(projectContext(p))
	full := 0
	for _, e := range ev {
		if e.FullText {
			full++
		}
	}
	if method != "" {
		sb.WriteString("\nHow the literature was identified (report these facts in the opening paragraph; do not invent others):\n" + method + "\n")
	}
	fmt.Fprintf(&sb, "\nIncluded papers: %d (%d with full text, the rest abstract only). For each: citation key, bibliographic data, extracted aspects, and source text.\n", len(ev), full)
	for i, e := range ev {
		pp := e.Paper
		fmt.Fprintf(&sb, "\n=== PAPER %d  [@%s] ===\n%s (%d). %s. %s.", i+1, pp.BibKey, strings.Join(pp.Authors, ", "), pp.Year, pp.Title, pp.Venue)
		if pp.DOI != "" {
			fmt.Fprintf(&sb, " doi:%s", pp.DOI)
		}
		sb.WriteString("\n")
		if x := pp.Extract; x != nil {
			fmt.Fprintf(&sb, "Objective: %s\nMethod: %s\nContext: %s\nFindings: %s\nTheory: %s\nContribution: %s\nLimitations: %s\n", x.Objective, x.Method, x.Context, x.Findings, x.Theory, x.Contribution, x.Limitations)
		}
		if e.FullText {
			fmt.Fprintf(&sb, "Source text (excerpt of full text):\n%s\n", e.Text)
		} else {
			fmt.Fprintf(&sb, "Abstract:\n%s\n", e.Text)
		}
	}
	sb.WriteString(`

Write the Background section (about 1,800–2,800 words) with this structure, using Markdown headings (## for the section, ### for subsections):

## Background
Opening paragraph: scope of the review, how the literature was identified (use the facts given above: databases, number of queries and rounds, candidates screened, screening procedure, number of included studies), and a roadmap of the subsections. Do not leave placeholders for the authors.

### Key concepts and definitions
Define the central constructs of the research question(s) using how the included literature defines them. Where the question uses non-standard or vendor terminology, map it explicitly to the terms the literature uses, with citations.

### Thematic synthesis (3–5 subsections, each titled by theme)
For each theme: a topic sentence stating the state of knowledge; synthesis across studies (agreements, tensions, boundary conditions), with citations grouped where several studies support a point; methodological basis of the evidence (e.g. "two case studies and one survey"); and what remains unresolved.

### Theoretical lenses
Which theories or frameworks the corpus draws on, how they have been applied, and gaps in theorising.

### Methodological observations
Designs, settings, data and maturity of the evidence base; note dominance of conceptual or practitioner-oriented work if that is the case.

### Research gaps and positioning
Derive the gaps that motivate the research question(s). End with one paragraph that states how a study addressing the question(s) would contribute.

Rules: cite every empirical or conceptual claim; do not cite anything outside the provided keys; prefer narrative synthesis over lists (no bullet points in the prose); do not include a reference list (the bibliography is generated from the keys); do not mention these instructions.`)
	return c.Complete(ctx, system, sb.String(), 12000)
}

// disciplineClause phrases the project's discipline for system prompts.
func disciplineClause(p *model.Project) string {
	if p == nil || p.Discipline == "" {
		return ""
	}
	return " working for a researcher in " + p.Discipline
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
