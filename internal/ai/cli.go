package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// CLIBackend runs the Claude Code CLI in headless mode (`claude -p`), which
// authenticates with the user's existing login — typically a Claude
// subscription — instead of an API key.
type CLIBackend struct {
	// Path to the claude binary (default "claude" on PATH).
	Path string
	// Model is passed as --model. When empty, "sonnet" is used; the CLI's
	// account default is never relied upon.
	Model string
	// Timeout bounds one invocation.
	Timeout time.Duration
}

// LookupCLI reports whether the claude binary is available.
func LookupCLI(path string) (string, bool) {
	if path == "" {
		path = "claude"
	}
	full, err := exec.LookPath(path)
	return full, err == nil
}

func (b *CLIBackend) Name() string { return "claude-cli" }

func (b *CLIBackend) model() string {
	if b.Model == "" {
		return "sonnet"
	}
	return b.Model
}

func (b *CLIBackend) ModelLabel() string { return b.model() }

// WithModel implements Backend.
func (b *CLIBackend) WithModel(m string) Backend {
	if m == "" {
		return b
	}
	c := *b
	c.Model = m
	return &c
}

// cliResult is the subset of `claude -p --output-format json` we use.
type cliResult struct {
	Type       string                     `json:"type"`
	Subtype    string                     `json:"subtype"`
	IsError    bool                       `json:"is_error"`
	Result     string                     `json:"result"`
	NumTurns   int                        `json:"num_turns"`
	ModelUsage map[string]json.RawMessage `json:"modelUsage"`
}

// Complete implements Backend by invoking `claude -p`.
func (b *CLIBackend) Complete(ctx context.Context, system, user string, maxTokens int) (string, error) {
	path := b.Path
	if path == "" {
		path = "claude"
	}
	timeout := b.Timeout
	if timeout == 0 {
		timeout = 10 * time.Minute
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(time.Duration(attempt) * 10 * time.Second):
			}
		}
		cctx, cancel := context.WithTimeout(ctx, timeout)
		args := []string{
			"-p",
			"--output-format", "json",
			"--no-session-persistence",
			"--tools", "", // pure text completion: no file or shell access
			"--system-prompt", system,
			"--model", b.model(),
		}
		cmd := exec.CommandContext(cctx, path, args...)
		cmd.Stdin = strings.NewReader(user)
		// Running inside another Claude Code session sets CLAUDECODE, which
		// makes the CLI refuse to nest; the headless child must not inherit it.
		var env []string
		for _, kv := range os.Environ() {
			if strings.HasPrefix(kv, "CLAUDECODE=") {
				continue
			}
			env = append(env, kv)
		}
		cmd.Env = env
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		cancel()
		if err != nil && stdout.Len() == 0 {
			lastErr = fmt.Errorf("claude cli: %v: %s", err, truncate(strings.TrimSpace(stderr.String()), 300))
			if errors.Is(ctx.Err(), context.Canceled) {
				return "", ctx.Err()
			}
			continue
		}
		text, perr := parseCLIOutput(stdout.Bytes())
		if perr != nil {
			lastErr = perr
			if isRetryableCLIError(perr) {
				continue
			}
			return "", perr
		}
		return text, nil
	}
	return "", lastErr
}

// parseCLIOutput extracts the assistant text from the CLI's JSON result.
func parseCLIOutput(out []byte) (string, error) {
	out = bytes.TrimSpace(out)
	if len(out) == 0 {
		return "", errors.New("claude cli: empty output")
	}
	var res cliResult
	if err := json.Unmarshal(out, &res); err != nil {
		// Some versions print one JSON object per line; take the last result line.
		lines := bytes.Split(out, []byte("\n"))
		for i := len(lines) - 1; i >= 0; i-- {
			if json.Unmarshal(bytes.TrimSpace(lines[i]), &res) == nil && res.Type == "result" {
				break
			}
		}
		if res.Type != "result" {
			return "", fmt.Errorf("claude cli: unexpected output: %s", truncate(string(out), 300))
		}
	}
	if res.IsError {
		return "", fmt.Errorf("claude cli: %s", truncate(strings.TrimSpace(res.Result), 400))
	}
	if strings.TrimSpace(res.Result) == "" {
		return "", errors.New("claude cli: empty result")
	}
	return res.Result, nil
}

func isRetryableCLIError(err error) bool {
	m := strings.ToLower(err.Error())
	for _, s := range []string{"rate limit", "overloaded", "429", "529", "timeout", "timed out", "temporarily", "econnreset", "network"} {
		if strings.Contains(m, s) {
			return true
		}
	}
	return false
}
