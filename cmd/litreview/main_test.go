package main

import "testing"

func TestListenURL(t *testing.T) {
	for addr, want := range map[string]string{
		":8080":          "http://localhost:8080",
		"127.0.0.1:9000": "http://127.0.0.1:9000",
	} {
		if got := listenURL(addr); got != want {
			t.Errorf("listenURL(%q) = %q, want %q", addr, got, want)
		}
	}
}
