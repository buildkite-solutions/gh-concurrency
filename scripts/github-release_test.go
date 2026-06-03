package main

import "testing"

func TestParseRepoURL(t *testing.T) {
	tests := map[string]string{
		"git@github.com:buildkite-solutions/gh-concurrency.git":       "buildkite-solutions/gh-concurrency",
		"https://github.com/buildkite-solutions/gh-concurrency.git":   "buildkite-solutions/gh-concurrency",
		"ssh://git@github.com/buildkite-solutions/gh-concurrency.git": "buildkite-solutions/gh-concurrency",
		"buildkite-solutions/gh-concurrency":                          "buildkite-solutions/gh-concurrency",
		"":                                                            "",
	}
	for raw, want := range tests {
		if got := parseRepoURL(raw); got != want {
			t.Fatalf("parseRepoURL(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestSplitRepo(t *testing.T) {
	owner, name, err := splitRepo("buildkite-solutions/gh-concurrency")
	if err != nil {
		t.Fatal(err)
	}
	if owner != "buildkite-solutions" || name != "gh-concurrency" {
		t.Fatalf("splitRepo returned %q/%q", owner, name)
	}
	if _, _, err := splitRepo("nope"); err == nil {
		t.Fatal("splitRepo(nope) succeeded, want error")
	}
}
