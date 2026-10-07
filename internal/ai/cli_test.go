package ai

import (
	"strings"
	"testing"
)

func TestParseCLIOutput(t *testing.T) {
	ok := []byte(`{"type":"result","subtype":"success","is_error":false,"result":"[{\"id\":\"1\"}]","num_turns":1}`)
	got, err := parseCLIOutput(ok)
	if err != nil || got != `[{"id":"1"}]` {
		t.Fatalf("got %q err %v", got, err)
	}
	bad := []byte(`{"type":"result","subtype":"success","is_error":true,"result":"Not logged in · Please run /login"}`)
	if _, err := parseCLIOutput(bad); err == nil || !strings.Contains(err.Error(), "Not logged in") {
		t.Fatalf("expected login error, got %v", err)
	}
	multi := []byte("{\"type\":\"system\"}\n{\"type\":\"result\",\"is_error\":false,\"result\":\"hi\"}\n")
	if got, err := parseCLIOutput(multi); err != nil || got != "hi" {
		t.Fatalf("multi-line: %q %v", got, err)
	}
	if _, err := parseCLIOutput([]byte("garbage")); err == nil {
		t.Fatal("expected error for garbage")
	}
}

func TestCLIDefaultsToSonnet(t *testing.T) {
	if (&CLIBackend{}).ModelLabel() != "sonnet" {
		t.Fatal("empty model must default to sonnet")
	}
	if (&CLIBackend{Model: "haiku"}).ModelLabel() != "haiku" {
		t.Fatal("explicit model must be kept")
	}
}
