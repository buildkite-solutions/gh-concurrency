package main

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestParseArgsVerboseAndDebugAliases(t *testing.T) {
	cfg, err := parseArgs([]string{"--repo", "o/r", "--since", "2025-05-01", "-v"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.verbose {
		t.Fatal("-v did not enable verbose logging")
	}

	cfg, err = parseArgs([]string{"--repo", "o/r", "--since", "2025-05-01", "-d"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.debug || !cfg.verbose {
		t.Fatalf("-d debug/verbose = %v/%v, want true/true", cfg.debug, cfg.verbose)
	}
}

func TestParseArgsIncludeArchived(t *testing.T) {
	cfg, err := parseArgs([]string{"--repo", "o/r", "--since", "2025-05-01", "--include-archived"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.includeArchived {
		t.Fatal("--include-archived did not enable archived repositories")
	}
}

func TestParseArgsPerformanceAndFilterFlags(t *testing.T) {
	cfg, err := parseArgs([]string{
		"--repo", "o/r",
		"--since", "2025-05-01",
		"--api-workers", "8",
		"--include-in-progress",
		"--job-filter", "latest",
		"--branch", "main",
		"--event", "push",
		"--exclude-pull-requests",
		"--top", "3",
	}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.apiWorkers != 8 || !cfg.includeInProgress || cfg.jobFilter != "latest" || cfg.branch != "main" || cfg.event != "push" || !cfg.excludePullRequests || cfg.top != 3 {
		t.Fatalf("parsed cfg = %#v", cfg)
	}
}

func TestParseArgsEstimateFlags(t *testing.T) {
	cfg, err := parseArgs([]string{
		"--repo", "o/r",
		"--since", "2025-05-01",
		"--estimate",
		"--estimate-max-requests", "123",
		"--estimate-min-remaining", "45",
		"--estimate-sample-runs", "67",
		"--estimate-iterations", "89",
		"--estimate-confidence", "80",
		"--estimate-seed", "42",
		"--estimate-repo-limit", "50",
	}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.estimate || cfg.estimateMaxRequests != 123 || cfg.estimateMinRemaining != 45 || cfg.estimateSampleRuns != 67 || cfg.estimateIterations != 89 || cfg.estimateConfidence != 80 || cfg.estimateSeed != 42 || cfg.estimateRepoLimit != 50 {
		t.Fatalf("parsed estimate cfg = %#v", cfg)
	}
}

func TestParseArgsClampsEstimateFlags(t *testing.T) {
	cfg, err := parseArgs([]string{
		"--repo", "o/r",
		"--since", "2025-05-01",
		"--estimate-max-requests", "0",
		"--estimate-min-remaining", "-1",
		"--estimate-sample-runs", "0",
		"--estimate-iterations", "0",
		"--estimate-confidence", "120",
		"--estimate-repo-limit", "-1",
	}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.estimateMaxRequests != 1 || cfg.estimateMinRemaining != 0 || cfg.estimateSampleRuns != 1 || cfg.estimateIterations != 1 || cfg.estimateConfidence != 99 || cfg.estimateRepoLimit != 0 {
		t.Fatalf("clamped estimate cfg = %#v", cfg)
	}
}

func TestParseArgsClampsWorkersAndTop(t *testing.T) {
	cfg, err := parseArgs([]string{"--repo", "o/r", "--since", "2025-05-01", "--api-workers", "99", "--top", "-1"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.apiWorkers != 32 {
		t.Fatalf("apiWorkers = %d, want 32", cfg.apiWorkers)
	}
	if cfg.top != 0 {
		t.Fatalf("top = %d, want 0", cfg.top)
	}
}

func TestValidateConfigRejectsInvalidJobFilter(t *testing.T) {
	cfg, err := parseArgs([]string{"--repo", "o/r", "--since", "2025-05-01", "--job-filter", "bogus"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateConfig(cfg); err == nil || !strings.Contains(err.Error(), "--job-filter") {
		t.Fatalf("validateConfig err = %v, want job-filter error", err)
	}
}

func TestValidateConfigRejectsEstimateKnobsWithoutEstimate(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{name: "max requests", args: []string{"--estimate-max-requests", "10"}},
		{name: "min remaining", args: []string{"--estimate-min-remaining", "10"}},
		{name: "sample runs", args: []string{"--estimate-sample-runs", "10"}},
		{name: "iterations", args: []string{"--estimate-iterations", "10"}},
		{name: "confidence", args: []string{"--estimate-confidence", "80"}},
		{name: "seed", args: []string{"--estimate-seed", "42"}},
		{name: "repo limit", args: []string{"--estimate-repo-limit", "50"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"--repo", "o/r", "--since", "2025-05-01"}, tc.args...)
			cfg, err := parseArgs(args, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			err = validateConfig(cfg)
			if err == nil || !strings.Contains(err.Error(), "requires --estimate") {
				t.Fatalf("validateConfig err = %v, want requires --estimate", err)
			}
		})
	}
}

func TestValidateConfigAcceptsValidProviderModeCombinations(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{
			name: "github exact filters",
			args: []string{
				"--repo", "o/r",
				"--since", "2025-05-01",
				"--until", "2025-05-02",
				"--org", "o",
				"--repo-type", "sources",
				"--include-archived",
				"--include-in-progress",
				"--job-filter", "latest",
				"--branch", "main",
				"--event", "push",
				"--exclude-pull-requests",
			},
		},
		{
			name: "github estimate",
			args: []string{
				"--repo", "o/r",
				"--since", "2025-05-01",
				"--estimate",
				"--estimate-max-requests", "10",
				"--estimate-min-remaining", "0",
				"--estimate-sample-runs", "5",
				"--estimate-iterations", "20",
				"--estimate-confidence", "80",
				"--estimate-seed", "42",
				"--estimate-repo-limit", "50",
			},
		},
		{
			name: "circleci project filters",
			args: []string{
				"--provider", "circleci",
				"--repo", "o/r",
				"--since", "2025-05-01",
				"--until", "2025-05-02",
				"--branch", "main",
				"--circleci-project", "gh/o/extra",
				"--circleci-vcs", "gh",
				"--circleci-job-details=false",
				"--circleci-max-pages", "2",
			},
		},
		{
			name: "github globals",
			args: []string{
				"--repo", "o/r",
				"--since", "2025-05-01",
				"--provider", "github",
				"--base-url", defaultBaseURL,
				"--token", "tok",
				"--format", "json",
				"--max-retries", "2",
				"--request-delay-ms", "0",
				"--api-workers", "2",
				"--top", "0",
				"--verbose",
				"--debug",
			},
		},
		{
			name: "circleci globals",
			args: []string{
				"--provider", "circleci",
				"--repo", "o/r",
				"--since", "2025-05-01",
				"--base-url", defaultCircleCIBaseURL,
				"--token", "tok",
				"--format", "json",
				"--max-retries", "2",
				"--request-delay-ms", "0",
				"--api-workers", "2",
				"--top", "0",
				"--verbose",
				"--debug",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := parseArgs(tc.args, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			if err := validateConfig(cfg); err != nil {
				t.Fatalf("validateConfig err = %v", err)
			}
		})
	}
}

func TestValidateConfigDefaultsDoNotTriggerProviderCompatibilityErrors(t *testing.T) {
	for _, args := range [][]string{
		{"--repo", "o/r", "--since", "2025-05-01"},
		{"--provider", "circleci", "--repo", "o/r", "--since", "2025-05-01"},
	} {
		cfg, err := parseArgs(args, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		if err := validateConfig(cfg); err != nil {
			t.Fatalf("validateConfig(%v) err = %v", args, err)
		}
	}
}

func TestValidateConfigRejectsUntilBeforeSince(t *testing.T) {
	cfg, err := parseArgs([]string{"--repo", "o/r", "--since", "2025-05-02", "--until", "2025-05-01"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateConfig(cfg); err == nil || !strings.Contains(err.Error(), "must be on or after --since") {
		t.Fatalf("validateConfig err = %v, want until before since error", err)
	}
}

func TestParseArgsCircleCIFlags(t *testing.T) {
	t.Setenv("CIRCLECI_TOKEN", "circle-token")
	cfg, err := parseArgs([]string{
		"--provider", "circleci",
		"--repo", "acme/api",
		"--circleci-project", "circleci/org-id/project-id",
		"--circleci-vcs", "bb",
		"--circleci-job-details=false",
		"--circleci-max-pages", "3",
		"--since", "2025-05-01",
	}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.provider != circleCIProvider || cfg.baseURL != defaultCircleCIBaseURL || cfg.token != "circle-token" {
		t.Fatalf("provider/base/token = %q/%q/%q", cfg.provider, cfg.baseURL, cfg.token)
	}
	if cfg.circleCIVCS != "bb" || cfg.circleCIJobDetails || cfg.circleCIMaxPages != 3 {
		t.Fatalf("circleci flags = vcs %q details %v max pages %d", cfg.circleCIVCS, cfg.circleCIJobDetails, cfg.circleCIMaxPages)
	}
	if err := validateConfig(cfg); err != nil {
		t.Fatal(err)
	}
}

func TestValidateConfigRejectsGitHubOnlyCircleCIFlags(t *testing.T) {
	cfg, err := parseArgs([]string{"--provider", "circleci", "--org", "acme", "--repo", "acme/api", "--since", "2025-05-01"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateConfig(cfg); err == nil || !strings.Contains(err.Error(), "--org") {
		t.Fatalf("validateConfig err = %v, want --org error", err)
	}

	cfg, err = parseArgs([]string{"--provider", "circleci", "--repo", "acme/api", "--since", "2025-05-01", "--estimate"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateConfig(cfg); err == nil || !strings.Contains(err.Error(), "--estimate") {
		t.Fatalf("validateConfig err = %v, want --estimate error", err)
	}
}

func TestValidateConfigRejectsCircleCIProviderWithGitHubOnlyFlags(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{name: "org", args: []string{"--org", "acme"}, want: "--org"},
		{name: "org file", args: []string{"--org-file", "orgs.txt"}, want: "--org-file"},
		{name: "repo type explicit default", args: []string{"--repo-type", "all"}, want: "--repo-type"},
		{name: "include archived explicit false", args: []string{"--include-archived=false"}, want: "--include-archived"},
		{name: "include in progress", args: []string{"--include-in-progress=false"}, want: "--include-in-progress is not supported for CircleCI"},
		{name: "job filter explicit default", args: []string{"--job-filter", "all"}, want: "--job-filter"},
		{name: "event", args: []string{"--event", "push"}, want: "--event"},
		{name: "exclude pull requests explicit false", args: []string{"--exclude-pull-requests=false"}, want: "--exclude-pull-requests"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"--provider", "circleci", "--repo", "acme/api", "--since", "2025-05-01"}, tc.args...)
			cfg, err := parseArgs(args, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			err = validateConfig(cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("validateConfig err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestValidateConfigRejectsGitHubProviderWithCircleCIOnlyFlags(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{name: "project", args: []string{"--circleci-project", "gh/acme/api"}, want: "--circleci-project"},
		{name: "project file", args: []string{"--circleci-project-file", "projects.txt"}, want: "--circleci-project-file"},
		{name: "vcs explicit default", args: []string{"--circleci-vcs", "gh"}, want: "--circleci-vcs"},
		{name: "job details explicit true", args: []string{"--circleci-job-details=true"}, want: "--circleci-job-details"},
		{name: "max pages explicit default", args: []string{"--circleci-max-pages", "0"}, want: "--circleci-max-pages"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"--repo", "acme/api", "--since", "2025-05-01"}, tc.args...)
			cfg, err := parseArgs(args, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			err = validateConfig(cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "--provider circleci") {
				t.Fatalf("validateConfig err = %v, want %q and provider hint", err, tc.want)
			}
		})
	}
}

func TestValidateConfigRejectsCircleCIProviderWithEstimateFlags(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{name: "estimate", args: []string{"--estimate"}, want: "--estimate is only supported with --provider github"},
		{name: "max requests", args: []string{"--estimate-max-requests", "10"}, want: "--estimate-max-requests"},
		{name: "min remaining", args: []string{"--estimate-min-remaining", "10"}, want: "--estimate-min-remaining"},
		{name: "sample runs", args: []string{"--estimate-sample-runs", "10"}, want: "--estimate-sample-runs"},
		{name: "iterations", args: []string{"--estimate-iterations", "10"}, want: "--estimate-iterations"},
		{name: "confidence", args: []string{"--estimate-confidence", "80"}, want: "--estimate-confidence"},
		{name: "seed", args: []string{"--estimate-seed", "42"}, want: "--estimate-seed"},
		{name: "repo limit", args: []string{"--estimate-repo-limit", "50"}, want: "--estimate-repo-limit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"--provider", "circleci", "--repo", "acme/api", "--since", "2025-05-01"}, tc.args...)
			cfg, err := parseArgs(args, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			err = validateConfig(cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("validateConfig err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestRunInvalidFlagCombinationFailsBeforeTokenResolution(t *testing.T) {
	t.Setenv("CIRCLECI_TOKEN", "")
	var stdout, stderr bytes.Buffer
	code := run([]string{"--provider", "circleci", "--repo", "acme/api", "--since", "2025-05-01", "--include-in-progress"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("run code = %d, want 2; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "--include-in-progress is not supported for CircleCI") {
		t.Fatalf("stderr missing compatibility error:\n%s", stderr.String())
	}
	if strings.Contains(stderr.String(), "no token") {
		t.Fatalf("validation should run before token resolution:\n%s", stderr.String())
	}
}
