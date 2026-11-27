package ai

import "fmt"

// BackendOptions are the settings that decide how Claude is reached.
type BackendOptions struct {
	// Mode is "auto" (API key if present, otherwise the CLI), "api" or
	// "claude-cli". Empty means auto.
	Mode     string
	APIKey   string
	APIModel string
	CLIPath  string
	CLIModel string
}

// SelectBackend builds the client for opts. lookup finds the claude binary
// (LookupCLI in production) and is a parameter so the choice can be tested
// without the CLI installed. The returned message says what was chosen, or
// why AI is disabled, for the start-up log.
func SelectBackend(opts BackendOptions, lookup func(string) (string, bool)) (*Client, string) {
	cli := func() (*Client, string, bool) {
		path, ok := lookup(opts.CLIPath)
		if !ok {
			return nil, "", false
		}
		b := &CLIBackend{Path: path, Model: opts.CLIModel}
		return New(b), fmt.Sprintf("AI backend: claude CLI in headless mode (%s, model: %s)", path, b.model()), true
	}
	api := func() (*Client, string) {
		return NewAPI(opts.APIKey, opts.APIModel), fmt.Sprintf("AI backend: Anthropic API (model: %s)", opts.APIModel)
	}
	switch opts.Mode {
	case "api":
		if opts.APIKey == "" {
			return New(nil), "WARNING: AI_BACKEND=api but ANTHROPIC_API_KEY is not set; AI features disabled"
		}
		return api()
	case "claude-cli":
		if c, msg, ok := cli(); ok {
			return c, msg
		}
		return New(nil), fmt.Sprintf("WARNING: AI_BACKEND=claude-cli but %q was not found on PATH; AI features disabled", opts.CLIPath)
	default:
		if opts.APIKey != "" {
			return api()
		}
		if c, msg, ok := cli(); ok {
			return c, msg
		}
		return New(nil), "WARNING: no AI backend: set ANTHROPIC_API_KEY or install and log in to the claude CLI; AI features disabled"
	}
}
