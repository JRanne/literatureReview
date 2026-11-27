package ai

import "testing"

func lookupFound(string) (string, bool)   { return "/usr/bin/claude", true }
func lookupMissing(string) (string, bool) { return "", false }

func TestSelectBackend(t *testing.T) {
	cases := []struct {
		name        string
		opts        BackendOptions
		lookup      func(string) (string, bool)
		wantBackend string
		wantModel   string
	}{
		{"auto prefers the API key", BackendOptions{Mode: "auto", APIKey: "k", APIModel: "claude-sonnet-5-5", CLIModel: "opus"}, lookupFound, "api", "claude-sonnet-5-5"},
		{"auto falls back to the CLI", BackendOptions{Mode: "auto", CLIModel: "opus"}, lookupFound, "claude-cli", "opus"},
		{"CLI model defaults to sonnet", BackendOptions{Mode: "claude-cli"}, lookupFound, "claude-cli", "sonnet"},
		{"auto with nothing disables AI", BackendOptions{Mode: "auto"}, lookupMissing, "none", ""},
		{"forced CLI that is missing disables AI", BackendOptions{Mode: "claude-cli", APIKey: "k"}, lookupMissing, "none", ""},
		{"forced API without a key disables AI", BackendOptions{Mode: "api"}, lookupFound, "none", ""},
		{"empty mode means auto", BackendOptions{APIKey: "k", APIModel: "m"}, lookupMissing, "api", "m"},
	}
	for _, c := range cases {
		cl, _ := SelectBackend(c.opts, c.lookup)
		if cl.BackendName() != c.wantBackend || cl.ModelLabel() != c.wantModel {
			t.Errorf("%s: got %s/%q, want %s/%q", c.name, cl.BackendName(), cl.ModelLabel(), c.wantBackend, c.wantModel)
		}
	}
}

func TestSelectBackendExplainsTheChoice(t *testing.T) {
	_, msg := SelectBackend(BackendOptions{Mode: "claude-cli", CLIPath: "claude"}, lookupMissing)
	if msg == "" {
		t.Fatal("a disabled backend must say why")
	}
}
