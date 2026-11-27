package ai

// Step names one stage of the review that calls a language model. Each step
// can run on its own model, so cheap bulk work (screening) and the one
// argument-driven call (synthesis) need not share a price point.
type Step string

const (
	StepQueries      Step = "queries"      // designing and expanding database queries
	StepScreening    Step = "screening"    // title/abstract screening of every record
	StepAdjudication Step = "adjudication" // third-reviewer pass over undecided papers
	StepExtraction   Step = "extraction"   // literature-table extraction per kept paper
	StepConcepts     Step = "concepts"     // concept-matrix columns and assignment
	StepSynthesis    Step = "synthesis"    // the background-section draft
)

// StepInfo describes a step for the settings page.
type StepInfo struct {
	Step  Step
	Label string
	Help  string
}

// AllSteps lists the steps in pipeline order.
var AllSteps = []StepInfo{
	{StepQueries, "Query design", "writes and expands the per-database search queries"},
	{StepScreening, "Screening", "scores every record against the question; the bulk of the calls"},
	{StepAdjudication, "Adjudication", "third-reviewer pass over papers the screeners left undecided"},
	{StepExtraction, "Table extraction", "fills the literature table for each kept paper"},
	{StepConcepts, "Concept matrix", "proposes concept columns and assigns them to papers"},
	{StepSynthesis, "Synthesis", "drafts the academic background section"},
}

// ParseStep returns the step with the given name.
func ParseStep(name string) (Step, bool) {
	for _, s := range AllSteps {
		if string(s.Step) == name {
			return s.Step, true
		}
	}
	return "", false
}

// Models maps steps to model ids. A missing or empty entry means "inherit".
type Models map[Step]string

// fallbacks keeps the behaviour SYNTHESIS_MODEL had before per-step models:
// it also chose the model for adjudication and the concept matrix.
var fallbacks = map[Step]Step{
	StepAdjudication: StepSynthesis,
	StepConcepts:     StepSynthesis,
}

// ResolveModel picks the model for a step: the project's choice, then the
// configured default for the step, then the configured default of the step it
// falls back to. "" means the backend's base model. The fallback is applied to
// the configured defaults only: a project's synthesis pick is a per-draft
// decision and must not move a bulk step onto a dearer model.
func ResolveModel(step Step, project, defaults Models) string {
	if m := project[step]; m != "" {
		return m
	}
	if m := defaults[step]; m != "" {
		return m
	}
	if fb, ok := fallbacks[step]; ok {
		return defaults[fb]
	}
	return ""
}

// ForStep returns a client answering with the model resolved for step.
func (c *Client) ForStep(step Step, project, defaults Models) *Client {
	return c.WithModel(ResolveModel(step, project, defaults))
}

// ModelOption is one model offered in the per-step selectors.
type ModelOption struct {
	ID, Label, Note string
}

// ModelChoices lists the models offered in the UI. Full ids work on both
// backends (the CLI accepts them as well as its aliases). Any id can still be
// configured through the MODEL_<STEP> environment variables.
var ModelChoices = []ModelOption{
	{ID: "", Label: "Default", Note: "the configured default for this step"},
	{ID: "claude-haiku-5-5", Label: "Haiku 5.5", Note: "cheapest; adequate for bulk screening"},
	{ID: "claude-sonnet-5-5", Label: "Sonnet 5.5", Note: "fast and economical"},
	{ID: "claude-opus-5-5", Label: "Opus 5.5", Note: "recommended for the argument-driven background section"},
	{ID: "claude-fable-5-2", Label: "Fable 5.2", Note: "strongest and most expensive"},
}

// KnownModel reports whether id is one of ModelChoices ("" included).
func KnownModel(id string) bool {
	for _, m := range ModelChoices {
		if m.ID == id {
			return true
		}
	}
	return false
}
