// Package dotenv loads KEY=VALUE files into the process environment.
package dotenv

import (
	"bufio"
	"log"
	"os"
	"strings"
)

// Load reads KEY=VALUE lines from path (typically ".env" in the working
// directory) and sets each variable that is not already present in the
// environment, so the shell always wins. A missing file is not an error.
// Supported: blank lines, # comments, an optional "export " prefix, single or
// double quotes around the value, and inline comments introduced by " #"
// outside quotes.
func Load(path string) int {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	n := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		key, val, ok := parseDotEnvLine(sc.Text())
		if !ok {
			continue
		}
		if _, set := os.LookupEnv(key); set {
			continue
		}
		if os.Setenv(key, val) == nil {
			n++
		}
	}
	if n > 0 {
		log.Printf("loaded %d variable(s) from %s", n, path)
	}
	return n
}

// parseDotEnvLine returns the key and value of one .env line, or ok=false
// for blank lines, comments and lines without '='.
func parseDotEnvLine(line string) (key, val string, ok bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false
	}
	line = strings.TrimPrefix(line, "export ")
	k, v, found := strings.Cut(line, "=")
	if !found {
		return "", "", false
	}
	key = strings.TrimSpace(k)
	if key == "" {
		return "", "", false
	}
	raw := v
	v = strings.TrimSpace(v)
	switch {
	case len(v) >= 2 && (v[0] == '"' || v[0] == '\''):
		q := v[0]
		if end := strings.IndexByte(v[1:], q); end >= 0 {
			v = v[1 : end+1]
		} else {
			v = v[1:]
		}
	default:
		// Search the untrimmed value: in "KEY=   # note" the whitespace
		// before "#" is what makes it a comment, and trimming removes it.
		if i := commentStart(raw); i >= 0 {
			v = strings.TrimSpace(raw[:i])
		}
	}
	return key, v, true
}

// commentStart returns the index of the first "#" that follows a space or tab,
// or -1. As in a shell, "KEY=#x" keeps "#x" as the value.
func commentStart(v string) int {
	for i := 1; i < len(v); i++ {
		if v[i] == '#' && (v[i-1] == ' ' || v[i-1] == '\t') {
			return i
		}
	}
	return -1
}
