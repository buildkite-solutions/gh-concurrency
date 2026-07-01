package main

import (
	"testing"
)

func TestNextLinkPresent(t *testing.T) {
	header := `<https://api.github.com/x?page=2>; rel="next", <https://api.github.com/x?page=9>; rel="last"`
	got := nextLink(header)
	if got != "https://api.github.com/x?page=2" {
		t.Fatalf("nextLink = %q", got)
	}
}

func TestNextLinkMissing(t *testing.T) {
	if got := nextLink(`<https://api.github.com/x?page=1>; rel="prev"`); got != "" {
		t.Fatalf("nextLink = %q, want empty", got)
	}
}

func TestNextLinkEmpty(t *testing.T) {
	if got := nextLink(""); got != "" {
		t.Fatalf("nextLink = %q, want empty", got)
	}
}
