package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestProgressBar(t *testing.T) {
	got := progressBar(3, 10, 10)
	if got != "[###-------]" {
		t.Fatalf("progressBar = %q, want [###-------]", got)
	}
}

func TestProgressReporterWritesRepoProgress(t *testing.T) {
	var buf bytes.Buffer
	progress := newProgressReporter(&buf, true, 2)
	progress.Begin()
	progress.Start("o/r")
	progress.Done("o/r", 3)

	out := buf.String()
	for _, want := range []string{
		"repositories queued: 2",
		"examining repo 1/2: o/r",
		"done: o/r (3 jobs, 3 total)",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("progress output missing %q:\n%s", want, out)
		}
	}
}
