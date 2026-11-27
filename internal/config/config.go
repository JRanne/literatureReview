// Package config loads runtime configuration from environment variables.
package config

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"literaturereview/internal/ai"
)

// Config holds every tunable the application needs.
type Config struct {
	Addr    string
	DataDir string

	// AIBackend selects how Claude is called: "auto" (API key if present,
	// otherwise the claude CLI), "api", or "claude-cli".
	AIBackend       string
	AnthropicAPIKey string
	AnthropicModel  string
	// ClaudeCLIPath is the claude binary used by the claude-cli backend.
	ClaudeCLIPath string
	// StepModels is the default model per review step, from MODEL_<STEP>
	// (e.g. MODEL_SCREENING=claude-haiku-5-5). SYNTHESIS_MODEL is still read
	// as an alias for MODEL_SYNTHESIS. Steps left out use the backend model.
	StepModels ai.Models
	// unknownModelVars lists MODEL_* variables that name no step, so a typo
	// fails Validate instead of being ignored.
	unknownModelVars []string
	// ClaudeCLIModel is passed as --model to the CLI. Defaults to "sonnet"
	// so the account's default (often the most expensive model) is never
	// used implicitly.
	ClaudeCLIModel string

	// JevAPIKey enables TypeSafe Jev as an independent second screener
	// (JEV_API_KEY or TYPESAFE_API_KEY).
	JevAPIKey string
	JevModel  string
	JevURL    string
	// Screener selects who screens: "both" (Claude + Jev, default when a Jev
	// key is set), "claude", or "jev".
	Screener string

	// ContactEmail is sent to OpenAlex, CrossRef and Unpaywall so requests land
	// in their "polite pools" and so operators can reach you.
	ContactEmail string

	SemanticScholarAPIKey string
	CoreAPIKey            string
	// OpenAlexAPIKey lifts OpenAlex's free daily credit budget (optional).
	OpenAlexAPIKey string

	// ProxyPrefix is an EZproxy-style login prefix, e.g.
	// https://ezproxy.example.fi/login?url=
	// The target URL is appended verbatim.
	ProxyPrefix string
	// ProxyCookie is the raw Cookie header value for an authenticated proxy
	// session, e.g. "ezproxy=AbCdEf123".
	ProxyCookie string

	// Browser optionally names the Chromium-based binary used for the
	// interactive proxy login (default: first of chromium, google-chrome, brave).
	Browser string
	// BrowserFetch enables the headless-browser download stage ("auto" or "off").
	BrowserFetch string

	// ZoteroLocal is the Zotero desktop connector base URL.
	ZoteroLocal string
	// JufoBase, OpenAlexBase and COREURL override registry endpoints (tests).
	JufoBase     string
	OpenAlexBase string
	COREURL      string
	CrossRefBase string

	UserAgent string
}

// FromEnv builds a Config from environment variables, applying defaults.
func FromEnv() Config {
	c := Config{
		Addr:                  envOr("ADDR", ":8080"),
		DataDir:               envOr("DATA_DIR", "data"),
		AIBackend:             strings.ToLower(envOr("AI_BACKEND", "auto")),
		AnthropicAPIKey:       os.Getenv("ANTHROPIC_API_KEY"),
		AnthropicModel:        envOr("ANTHROPIC_MODEL", "claude-sonnet-5-5"),
		ClaudeCLIPath:         envOr("CLAUDE_CLI", "claude"),
		ClaudeCLIModel:        envOr("CLAUDE_CLI_MODEL", "sonnet"),
		JevAPIKey:             firstEnv("JEV_API_KEY", "TYPESAFE_API_KEY"),
		JevModel:              envOr("JEV_MODEL", "jev-latest"),
		JevURL:                envOr("JEV_URL", "https://api.typesafe.ai/v1/systemone"),
		Screener:              strings.ToLower(envOr("SCREENER", "")),
		ContactEmail:          os.Getenv("CONTACT_EMAIL"),
		SemanticScholarAPIKey: os.Getenv("S2_API_KEY"),
		OpenAlexAPIKey:        os.Getenv("OPENALEX_API_KEY"),
		CoreAPIKey:            os.Getenv("CORE_API_KEY"),
		ProxyPrefix:           os.Getenv("PROXY_PREFIX"),
		ProxyCookie:           os.Getenv("PROXY_COOKIE"),
		Browser:               os.Getenv("BROWSER_BIN"),
		ZoteroLocal:           envOr("ZOTERO_LOCAL", "http://127.0.0.1:23119"),
		BrowserFetch:          strings.ToLower(envOr("BROWSER_FETCH", "auto")),
	}
	c.StepModels, c.unknownModelVars = stepModelsFromEnv(os.Environ())
	if c.Screener == "" {
		if c.JevAPIKey != "" {
			c.Screener = "both"
		} else {
			c.Screener = "claude"
		}
	}
	ua := "litreview/0.1 (academic literature review tool"
	if c.ContactEmail != "" {
		ua += "; mailto:" + c.ContactEmail
	}
	c.UserAgent = ua + ")"
	return c
}

// Validate reports configuration problems that prevent the app from working.
func (c Config) Validate() error {
	var problems []string
	switch c.AIBackend {
	case "auto", "api", "claude-cli":
	default:
		problems = append(problems, fmt.Sprintf("AI_BACKEND=%q is not one of auto, api, claude-cli", c.AIBackend))
	}
	for _, v := range c.unknownModelVars {
		problems = append(problems, fmt.Sprintf("%s names no review step (known: %s)", v, stepVarList()))
	}
	switch c.Screener {
	case "both", "claude", "jev":
	default:
		problems = append(problems, fmt.Sprintf("SCREENER=%q is not one of both, claude, jev", c.Screener))
	}
	if c.Screener != "claude" && c.JevAPIKey == "" {
		problems = append(problems, "SCREENER="+c.Screener+" needs JEV_API_KEY")
	}
	if c.AIBackend == "api" && c.AnthropicAPIKey == "" {
		problems = append(problems, "AI_BACKEND=api but ANTHROPIC_API_KEY is not set")
	}
	if c.ContactEmail == "" {
		problems = append(problems, "CONTACT_EMAIL is not set (required by Unpaywall; recommended for OpenAlex/CrossRef polite pools)")
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("%s", strings.Join(problems, "; "))
}

// ProxyConfigured reports whether university proxy access can be attempted.
func (c Config) ProxyConfigured() bool {
	return c.ProxyPrefix != ""
}

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

const modelVarPrefix = "MODEL_"

// stepModelsFromEnv reads MODEL_<STEP> from environ ("KEY=value" pairs).
func stepModelsFromEnv(environ []string) (ai.Models, []string) {
	m := ai.Models{}
	if v := os.Getenv("SYNTHESIS_MODEL"); v != "" {
		m[ai.StepSynthesis] = v
	}
	var unknown []string
	for _, kv := range environ {
		k, v, _ := strings.Cut(kv, "=")
		if !strings.HasPrefix(k, modelVarPrefix) || v == "" {
			continue
		}
		step, ok := ai.ParseStep(strings.ToLower(strings.TrimPrefix(k, modelVarPrefix)))
		if !ok {
			unknown = append(unknown, k)
			continue
		}
		m[step] = v
	}
	sort.Strings(unknown)
	return m, unknown
}

func stepVarList() string {
	var names []string
	for _, s := range ai.AllSteps {
		names = append(names, modelVarPrefix+strings.ToUpper(string(s.Step)))
	}
	return strings.Join(names, ", ")
}
