// Package model defines the domain types shared across the application.
package model

import (
	"regexp"
	"strings"
	"time"
)

// Stage is the lifecycle position of a review project.
type Stage string

const (
	StageSetup       Stage = "setup"
	StageSearching   Stage = "searching"
	StageScreening   Stage = "screening"
	StageDownloading Stage = "downloading"
	StageDone        Stage = "done"
)

// Decision is the human keep/drop choice for a paper.
type Decision string

const (
	DecisionUndecided Decision = ""
	DecisionKeep      Decision = "keep"
	DecisionDrop      Decision = "drop"
)

// Verdict is the AI's suggested decision.
type Verdict string

const (
	VerdictKeep   Verdict = "keep"
	VerdictDrop   Verdict = "drop"
	VerdictUnsure Verdict = "unsure"
)

// Query is one search string generated for one database.
type Query struct {
	Source    string `json:"source"`
	Query     string `json:"query"`
	Rationale string `json:"rationale,omitempty"`
	// URL is the exact request sent to the database on the last run.
	URL string `json:"url,omitempty"`
	// Total is the number of hits the database reported for the query;
	// Results is how many were retrieved (bounded by max per query).
	Total   int       `json:"total,omitempty"`
	Results int       `json:"results"`
	Error   string    `json:"error,omitempty"`
	RanAt   time.Time `json:"ran_at,omitempty"`
	// Round is the search round that produced the query (1 = initial).
	Round int `json:"round,omitempty"`
	// Relevant is how many papers surfaced by this query rank >= 50.
	Relevant int `json:"relevant,omitempty"`
}

// Attempt records one fetcher's try at obtaining a PDF.
type Attempt struct {
	Method  string    `json:"method"`
	URL     string    `json:"url,omitempty"`
	OK      bool      `json:"ok"`
	Message string    `json:"message,omitempty"`
	At      time.Time `json:"at"`
}

// DownloadStatus is the overall outcome for a paper's PDF.
type DownloadStatus string

const (
	DownloadPending DownloadStatus = ""
	DownloadRunning DownloadStatus = "running"
	DownloadOK      DownloadStatus = "ok"
	DownloadFailed  DownloadStatus = "failed"
)

// DownloadResult summarises the fetch chain for a paper.
type DownloadResult struct {
	Status   DownloadStatus `json:"status"`
	Method   string         `json:"method,omitempty"`
	Path     string         `json:"path,omitempty"`
	Bytes    int64          `json:"bytes,omitempty"`
	Attempts []Attempt      `json:"attempts,omitempty"`
	// NeedsLogin is set when the paper was not obtained only because the
	// university proxy session had expired; it is retried after a login.
	NeedsLogin bool `json:"needs_login,omitempty"`
}

// Extraction holds the AI-extracted key aspects of a paper, the content of a
// systematic-review data-extraction (master) table.
type Extraction struct {
	Objective    string   `json:"objective,omitempty"`
	Method       string   `json:"method,omitempty"`
	Context      string   `json:"context,omitempty"`
	Findings     string   `json:"findings,omitempty"`
	Theory       string   `json:"theory,omitempty"`
	Contribution string   `json:"contribution,omitempty"`
	Limitations  string   `json:"limitations,omitempty"`
	Relevance    string   `json:"relevance,omitempty"`
	Keywords     []string `json:"keywords,omitempty"`
	Model        string   `json:"model,omitempty"`
}

// VenueRating is the stored subset of venues.Rating.
type VenueRating struct {
	JufoLevel   int     `json:"jufo_level"` // -1 unknown
	JufoID      string  `json:"jufo_id,omitempty"`
	NorwayLevel int     `json:"norway_level"`
	Basket      string  `json:"basket,omitempty"`
	ISConf      string  `json:"is_conf,omitempty"`
	CORERank    string  `json:"core_rank,omitempty"`
	SJRQuartile string  `json:"sjr_quartile,omitempty"`
	OACore      bool    `json:"oa_core"`
	DOAJ        bool    `json:"doaj"`
	HIndex      int     `json:"h_index"`
	Citedness   float64 `json:"citedness"`
	Tier        string  `json:"tier,omitempty"` // top | good | basic | below | unknown
	Label       string  `json:"label,omitempty"`
	Version     int     `json:"version,omitempty"`
}

// Concept is a column of the concept matrix (Webster & Watson).
type Concept struct {
	Name       string `json:"name"`
	Definition string `json:"definition,omitempty"`
}

// ConceptHit marks that a paper addresses a concept, with a short note.
type ConceptHit struct {
	Name string `json:"name"`
	Note string `json:"note,omitempty"`
}

// Adjudication is a decisive third-reviewer judgement on an undecided paper.
type Adjudication struct {
	Decision   Verdict `json:"decision"`
	Confidence float64 `json:"confidence"`
	Reason     string  `json:"reason"`
	Model      string  `json:"model,omitempty"`
}

// JevAssessment is the independent second-screener result from TypeSafe Jev.
type JevAssessment struct {
	Score      int     `json:"score"`
	Verdict    Verdict `json:"verdict,omitempty"`
	Relevance  float64 `json:"relevance"`  // 0–4 rubric level (probability-weighted)
	Include    float64 `json:"include"`    // P(include)
	Exclude    float64 `json:"exclude"`    // P(matches an exclusion criterion)
	Confidence float64 `json:"confidence"` // confidence of the relevance score
	Model      string  `json:"model,omitempty"`
	Error      string  `json:"error,omitempty"`
}

// Detail renders the assessment for display.
func (j *JevAssessment) Detail() string {
	if j == nil {
		return ""
	}
	if j.Error != "" {
		return "Jev error: " + j.Error
	}
	s := "relevance " + ftoa(j.Relevance) + "/4 · include " + itoa(int(j.Include*100+0.5)) + "%"
	if j.Exclude > 0 {
		s += " · exclusion " + itoa(int(j.Exclude*100+0.5)) + "%"
	}
	return s + " · confidence " + ftoa(j.Confidence)
}

func ftoa(f float64) string {
	n := int(f*100 + 0.5)
	frac := n % 100
	fs := itoa(frac)
	if frac < 10 {
		fs = "0" + fs
	}
	return itoa(n/100) + "." + fs
}

// Paper is one bibliographic record, merged across sources.
type Paper struct {
	ID       string   `json:"id"`
	DOI      string   `json:"doi,omitempty"`
	Title    string   `json:"title"`
	Authors  []string `json:"authors,omitempty"`
	Year     int      `json:"year,omitempty"`
	Venue    string   `json:"venue,omitempty"`
	Abstract string   `json:"abstract,omitempty"`
	URL      string   `json:"url,omitempty"`
	// PDFURLs are open-access PDF links reported by the databases.
	PDFURLs    []string `json:"pdf_urls,omitempty"`
	ArxivID    string   `json:"arxiv_id,omitempty"`
	OpenAlexID string   `json:"openalex_id,omitempty"`
	PMID       string   `json:"pmid,omitempty"`
	PMCID      string   `json:"pmcid,omitempty"`
	Citations  int      `json:"citations,omitempty"`
	OpenAccess bool     `json:"open_access"`
	// Preprint marks records from preprint servers / repositories (not peer reviewed).
	Preprint bool `json:"preprint,omitempty"`
	// Language is the ISO code reported by the database ("en", "de", …); NonEnglish marks excluded records.
	Language   string   `json:"language,omitempty"`
	NonEnglish bool     `json:"non_english,omitempty"`
	FoundIn    []string `json:"found_in,omitempty"`
	// FoundBy lists the queries (or snowball seeds) that surfaced the paper.
	FoundBy []string `json:"found_by,omitempty"`
	// Keywords are keyword/concept labels reported by the databases (OpenAlex).
	Keywords []string `json:"keywords,omitempty"`
	// ISSN of the venue when the database reports it (for JUFO lookup).
	ISSN string `json:"issn,omitempty"`
	// VenueRating caches the venue's quality flags (JUFO level, AIS basket, IS conference).
	VenueRating *VenueRating `json:"venue_rating,omitempty"`

	AIScore     int     `json:"ai_score"`
	AIVerdict   Verdict `json:"ai_verdict,omitempty"`
	AIRationale string  `json:"ai_rationale,omitempty"`
	// Jev is the second screener's assessment (nil when Jev is not configured).
	Jev *JevAssessment `json:"jev,omitempty"`
	// Adjudication is the third-reviewer verdict for papers the screeners left undecided.
	Adjudication *Adjudication `json:"adjudication,omitempty"`
	// Concepts are the concept-matrix columns this paper addresses.
	Concepts []ConceptHit `json:"concepts,omitempty"`

	// BibKey is the stable citation key shared by the .bib export and the
	// synthesis text, e.g. "virtanen2024generative".
	BibKey string `json:"bib_key,omitempty"`
	// TextPath points at the extracted full text (when a PDF was obtained
	// and pdftotext is available); TextChars is its length.
	TextPath  string `json:"text_path,omitempty"`
	TextChars int    `json:"text_chars,omitempty"`

	// NotRetrieved marks a paper that passed screening but was removed from
	// the analysis because no full text could be obtained. PRISMA counts it
	// as "report not retrieved", not as a screening exclusion.
	NotRetrieved bool `json:"not_retrieved,omitempty"`

	Decision Decision `json:"decision"`
	// DecisionBy is "user" when a human set the decision, "ai" when it was
	// pre-filled from the AI verdict. User decisions survive re-screening.
	DecisionBy string         `json:"decision_by,omitempty"`
	Download   DownloadResult `json:"download"`
	Extract    *Extraction    `json:"extract,omitempty"`
}

// JobStatus describes a background job attached to a project.
type JobStatus struct {
	Name       string    `json:"name"`
	Running    bool      `json:"running"`
	Phase      string    `json:"phase,omitempty"`
	PhaseNum   int       `json:"phase_num,omitempty"`
	PhaseTotal int       `json:"phase_total,omitempty"`
	Progress   int       `json:"progress"`
	Total      int       `json:"total"`
	Message    string    `json:"message,omitempty"`
	Log        []string  `json:"log,omitempty"`
	Error      string    `json:"error,omitempty"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
}

// Project is one literature review.
type Project struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Question  string `json:"question"`
	Inclusion string `json:"inclusion,omitempty"`
	Exclusion string `json:"exclusion,omitempty"`
	// Discipline describes the researcher's field and audience, e.g.
	// "Information Systems (IS) research; target venues ECIS/ICIS/JAIS". It
	// steers query vocabulary, screening judgement, extraction and the
	// register of the synthesis.
	Discipline string `json:"discipline,omitempty"`
	// PeerReviewedOnly excludes preprint-server and repository records.
	PeerReviewedOnly bool `json:"peer_reviewed_only"`
	// EnglishOnly excludes records not written in English.
	EnglishOnly  bool     `json:"english_only"`
	YearFrom     int      `json:"year_from,omitempty"`
	YearTo       int      `json:"year_to,omitempty"`
	MaxPerSource int      `json:"max_per_source"`
	Sources      []string `json:"sources"`
	// Target is the number of included papers the user aims for; the search
	// expands automatically (up to a few rounds) while below it. 0 disables.
	Target int `json:"target,omitempty"`
	// Rounds counts search rounds run so far (initial + expansions + snowballs).
	Rounds int `json:"rounds,omitempty"`
	// Stats accumulates PRISMA identification counts across runs.
	Stats SearchStats `json:"stats"`
	// Concepts are the concept-matrix columns derived for this review.
	Concepts   []Concept `json:"concepts,omitempty"`
	ConceptsAt time.Time `json:"concepts_at,omitempty"`

	Stage     Stage      `json:"stage"`
	Queries   []Query    `json:"queries,omitempty"`
	Papers    []*Paper   `json:"papers"`
	Job       *JobStatus `json:"job,omitempty"`
	Synthesis string     `json:"synthesis,omitempty"`
	// SynthesisModel records which model wrote the current draft.
	SynthesisModel string    `json:"synthesis_model,omitempty"`
	SynthesisAt    time.Time `json:"synthesis_at,omitempty"`
	// SynthesisChoice is the legacy per-project synthesis model, read only
	// for projects saved before StepModels existed.
	SynthesisChoice string `json:"synthesis_choice,omitempty"`
	// StepModels is the model the user picked per review step (keyed by the
	// step name, e.g. "screening"); missing steps use the configured default.
	StepModels map[string]string `json:"step_models,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// StepModel returns the user's model for a step, or "" for the default.
func (p *Project) StepModel(step string) string {
	if m := p.StepModels[step]; m != "" {
		return m
	}
	if step == "synthesis" {
		return p.SynthesisChoice
	}
	return ""
}

// SetStepModel records the user's model for a step ("" restores the default)
// and retires the legacy synthesis field so it cannot shadow the new choice.
func (p *Project) SetStepModel(step, id string) {
	if step == "synthesis" {
		p.SynthesisChoice = ""
	}
	if id == "" {
		delete(p.StepModels, step)
		return
	}
	if p.StepModels == nil {
		p.StepModels = map[string]string{}
	}
	p.StepModels[step] = id
}

// HasJev reports whether a usable Jev assessment exists.
func (pp *Paper) HasJev() bool { return pp.Jev != nil && pp.Jev.Error == "" && pp.Jev.Verdict != "" }

// Agreement describes how the two screeners relate: "agree", "conflict",
// or "" when only one screener ran.
func (pp *Paper) Agreement() string {
	if !pp.HasJev() || pp.AIVerdict == "" {
		return ""
	}
	if pp.AIVerdict == pp.Jev.Verdict {
		return "agree"
	}
	// keep vs drop is a conflict; anything involving "unsure" is soft.
	if (pp.AIVerdict == VerdictKeep && pp.Jev.Verdict == VerdictDrop) || (pp.AIVerdict == VerdictDrop && pp.Jev.Verdict == VerdictKeep) {
		return "conflict"
	}
	return "partial"
}

// FinalVerdict combines the screeners the way abstract-stage screening is
// done with two human reviewers: a paper is dropped only when both drop it,
// kept when one keeps it and the other does not object (keep or unsure), and
// left for adjudication when they clash (keep vs drop) or both are unsure.
// With a single screener, its verdict is used.
func (pp *Paper) FinalVerdict() Verdict {
	switch {
	case pp.HasJev() && pp.AIVerdict != "":
		a, b := pp.AIVerdict, pp.Jev.Verdict
		switch {
		case a == VerdictDrop && b == VerdictDrop:
			return VerdictDrop
		case (a == VerdictKeep && b == VerdictDrop) || (a == VerdictDrop && b == VerdictKeep):
			return VerdictUnsure // conflict → human adjudicates
		case a == VerdictKeep || b == VerdictKeep:
			return VerdictKeep
		default:
			return VerdictUnsure
		}
	case pp.HasJev():
		return pp.Jev.Verdict
	default:
		return pp.AIVerdict
	}
}

// RankScore orders papers: average of both screeners when both ran.
func (pp *Paper) RankScore() int {
	switch {
	case pp.HasJev() && pp.AIVerdict != "":
		return (pp.AIScore + pp.Jev.Score) / 2
	case pp.HasJev():
		return pp.Jev.Score
	default:
		return pp.AIScore
	}
}

// ApplyVerdict pre-fills Decision from the combined verdict unless a user decided.
func (pp *Paper) ApplyVerdict() {
	if pp.DecisionBy == "user" {
		return
	}
	switch pp.FinalVerdict() {
	case VerdictKeep:
		pp.Decision, pp.DecisionBy = DecisionKeep, "ai"
	case VerdictDrop:
		pp.Decision, pp.DecisionBy = DecisionDrop, "ai"
	default:
		pp.Decision, pp.DecisionBy = DecisionUndecided, ""
	}
}

// StageIndex orders stages for navigation.
func StageIndex(st Stage) int {
	switch st {
	case StageSetup:
		return 0
	case StageSearching:
		return 1
	case StageScreening:
		return 2
	case StageDownloading:
		return 3
	case StageDone:
		return 4
	}
	return 0
}

// SearchStats are PRISMA "identification" numbers accumulated over runs.
type SearchStats struct {
	Retrieved  int `json:"retrieved"`   // records returned by database queries
	Snowballed int `json:"snowballed"`  // records from citation searching
	Preprints  int `json:"preprints"`   // removed before screening by the preprint filter
	NonEnglish int `json:"non_english"` // removed before screening by the language filter
	Duplicates int `json:"duplicates"`  // removed before screening by de-duplication
}

// Paper looks up a paper by ID.
func (p *Project) Paper(id string) *Paper {
	for _, pp := range p.Papers {
		if pp.ID == id {
			return pp
		}
	}
	return nil
}

// Counts returns the number of kept, dropped and undecided papers.
func (p *Project) Counts() (keep, drop, undecided int) {
	for _, pp := range p.Papers {
		switch pp.Decision {
		case DecisionKeep:
			keep++
		case DecisionDrop:
			drop++
		default:
			undecided++
		}
	}
	return
}

// Kept returns the papers marked keep.
func (p *Project) Kept() []*Paper {
	var out []*Paper
	for _, pp := range p.Papers {
		if pp.Decision == DecisionKeep {
			out = append(out, pp)
		}
	}
	return out
}

var nonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

// NormalizeDOI lowercases a DOI and strips URL prefixes.
func NormalizeDOI(doi string) string {
	d := strings.TrimSpace(strings.ToLower(doi))
	for _, pre := range []string{"https://doi.org/", "http://doi.org/", "https://dx.doi.org/", "http://dx.doi.org/", "doi:", "doi.org/"} {
		d = strings.TrimPrefix(d, pre)
	}
	return d
}

// NormalizeTitle collapses a title to lowercase alphanumerics for matching.
func NormalizeTitle(t string) string {
	return strings.Trim(nonAlnum.ReplaceAllString(strings.ToLower(t), ""), " ")
}

// FirstAuthorSurname returns a best-effort surname of the first author.
func (pp *Paper) FirstAuthorSurname() string {
	if len(pp.Authors) == 0 {
		return "unknown"
	}
	a := strings.TrimSpace(pp.Authors[0])
	if i := strings.Index(a, ","); i > 0 {
		return strings.TrimSpace(a[:i])
	}
	parts := strings.Fields(a)
	return parts[len(parts)-1]
}

// EnsureBibKeys assigns unique, stable citation keys to all papers that
// lack one: surname + year + first significant title word, with a/b/c
// suffixes on collision.
func (p *Project) EnsureBibKeys() {
	used := map[string]bool{}
	for _, pp := range p.Papers {
		if pp.BibKey != "" {
			used[pp.BibKey] = true
		}
	}
	for _, pp := range p.Papers {
		if pp.BibKey != "" {
			continue
		}
		base := bibBase(pp)
		key := base
		for i := 0; used[key]; i++ {
			key = base + string(rune('a'+i%26))
			if i >= 26 {
				key = base + itoa(i)
			}
		}
		used[key] = true
		pp.BibKey = key
	}
}

var stopWords = map[string]bool{"a": true, "an": true, "the": true, "of": true, "on": true, "in": true, "for": true, "and": true, "to": true, "with": true, "from": true, "by": true, "is": true, "are": true, "towards": true, "toward": true, "how": true, "why": true, "what": true}

// foldDiacritics maps common accented Latin letters to ASCII so keys like
// "ozden2025generative" keep the author's initial letter.
func foldDiacritics(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case 'à', 'á', 'â', 'ã', 'ä', 'å', 'ā', 'ă', 'ą':
			return 'a'
		case 'À', 'Á', 'Â', 'Ã', 'Ä', 'Å', 'Ā':
			return 'A'
		case 'ç', 'ć', 'č':
			return 'c'
		case 'Ç', 'Ć', 'Č':
			return 'C'
		case 'ď', 'đ':
			return 'd'
		case 'è', 'é', 'ê', 'ë', 'ē', 'ė', 'ę', 'ě':
			return 'e'
		case 'È', 'É', 'Ê', 'Ë', 'Ē', 'Ę':
			return 'E'
		case 'ğ':
			return 'g'
		case 'Ğ':
			return 'G'
		case 'ì', 'í', 'î', 'ï', 'ī', 'ı':
			return 'i'
		case 'Ì', 'Í', 'Î', 'Ï', 'İ':
			return 'I'
		case 'ł', 'ľ':
			return 'l'
		case 'Ł':
			return 'L'
		case 'ñ', 'ń', 'ň':
			return 'n'
		case 'Ñ', 'Ń':
			return 'N'
		case 'ò', 'ó', 'ô', 'õ', 'ö', 'ø', 'ō', 'ő':
			return 'o'
		case 'Ò', 'Ó', 'Ô', 'Õ', 'Ö', 'Ø', 'Ő':
			return 'O'
		case 'ř':
			return 'r'
		case 'ś', 'š', 'ş', 'ș':
			return 's'
		case 'Ś', 'Š', 'Ş', 'Ș':
			return 'S'
		case 'ť', 'ţ', 'ț':
			return 't'
		case 'ù', 'ú', 'û', 'ü', 'ū', 'ů', 'ű':
			return 'u'
		case 'Ù', 'Ú', 'Û', 'Ü', 'Ū', 'Ű':
			return 'U'
		case 'ý', 'ÿ':
			return 'y'
		case 'Ý':
			return 'Y'
		case 'ź', 'ż', 'ž':
			return 'z'
		case 'Ź', 'Ż', 'Ž':
			return 'Z'
		case 'ß':
			return 's'
		case 'æ':
			return 'a'
		case 'œ':
			return 'o'
		}
		return r
	}, s)
}

func bibBase(pp *Paper) string {
	sur := nonAlnum.ReplaceAllString(strings.ToLower(foldDiacritics(pp.FirstAuthorSurname())), "")
	if sur == "" {
		sur = "anon"
	}
	word := ""
	for _, w := range strings.Fields(nonAlnum.ReplaceAllString(strings.ToLower(foldDiacritics(pp.Title)), " ")) {
		if len(w) > 2 && !stopWords[w] {
			word = w
			break
		}
	}
	yr := ""
	if pp.Year > 0 {
		yr = itoa(pp.Year)
	}
	return sur + yr + word
}

// Slug builds a filesystem-safe filename stem for the paper.
func (pp *Paper) Slug() string {
	title := strings.ToLower(pp.Title)
	title = nonAlnum.ReplaceAllString(title, "-")
	title = strings.Trim(title, "-")
	words := strings.Split(title, "-")
	if len(words) > 8 {
		words = words[:8]
	}
	title = strings.Join(words, "-")
	author := strings.Trim(nonAlnum.ReplaceAllString(strings.ToLower(pp.FirstAuthorSurname()), "-"), "-")
	year := "nd"
	if pp.Year > 0 {
		year = itoa(pp.Year)
	}
	return year + "-" + author + "-" + title
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	n := len(b)
	for i > 0 {
		n--
		b[n] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		n--
		b[n] = '-'
	}
	return string(b[n:])
}
