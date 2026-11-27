package dotenv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadDotEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	content := `# comment line
DOTENV_PLAIN=plain
DOTENV_INLINE=value # trailing comment
DOTENV_DQ="quoted # not a comment"
DOTENV_SQ='single quoted'
export DOTENV_EXPORTED=exported
DOTENV_SHELL=from-file

not-a-pair
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"DOTENV_PLAIN", "DOTENV_INLINE", "DOTENV_DQ", "DOTENV_SQ", "DOTENV_EXPORTED"} {
		os.Unsetenv(k)
		t.Cleanup(func() { os.Unsetenv(k) })
	}
	t.Setenv("DOTENV_SHELL", "from-shell")

	if n := Load(path); n != 5 {
		t.Fatalf("loaded %d variables, want 5", n)
	}
	want := map[string]string{
		"DOTENV_PLAIN":    "plain",
		"DOTENV_INLINE":   "value",
		"DOTENV_DQ":       "quoted # not a comment",
		"DOTENV_SQ":       "single quoted",
		"DOTENV_EXPORTED": "exported",
		"DOTENV_SHELL":    "from-shell", // the shell wins over the file
	}
	for k, w := range want {
		if got := os.Getenv(k); got != w {
			t.Errorf("%s = %q, want %q", k, got, w)
		}
	}
}

func TestLoadDotEnvMissingFile(t *testing.T) {
	if n := Load(filepath.Join(t.TempDir(), "absent.env")); n != 0 {
		t.Fatalf("missing file loaded %d variables", n)
	}
}

func TestParseLineEmptyValueWithComment(t *testing.T) {
	// .example.env documents empty settings this way; the comment must not
	// become the value (an "API key" of "# leave empty…" selects the API).
	for _, line := range []string{"KEY=                # leave empty", "KEY= #x", "KEY=\t# tab"} {
		k, v, ok := parseDotEnvLine(line)
		if !ok || k != "KEY" || v != "" {
			t.Errorf("%q → %q=%q ok=%v, want KEY=\"\"", line, k, v, ok)
		}
	}
}

func TestParseLineHashWithoutSpaceIsValue(t *testing.T) {
	// As in a shell, "#" only starts a comment after whitespace.
	if _, v, _ := parseDotEnvLine("KEY=#x"); v != "#x" {
		t.Fatalf("KEY=#x → %q, want #x", v)
	}
}

// The shipped template must parse cleanly: no comment may leak into a value.
func TestExampleEnvParses(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", ".example.env"))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := parseDotEnvLine(line)
		if !ok {
			continue
		}
		n++
		if strings.Contains(v, "#") || strings.Contains(v, " ") {
			t.Errorf("%s parses as %q", k, v)
		}
	}
	if n < 20 {
		t.Fatalf("only %d settings parsed from .example.env", n)
	}
}
