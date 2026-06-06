package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func dt(hms string) time.Time {
	parts := strings.Split(hms, ":")
	hour := atoi(parts[0])
	minute := atoi(parts[1])
	second := atoi(parts[2])
	return time.Date(2025, 5, 1, hour, minute, second, 0, time.UTC)
}

func atoi(value string) int {
	n := 0
	for _, ch := range value {
		n = n*10 + int(ch-'0')
	}
	return n
}

func TestConcurrencyProfileEmpty(t *testing.T) {
	peak, profile := concurrencyProfile(nil)
	if peak != 0 {
		t.Fatalf("peak = %d, want 0", peak)
	}
	if len(profile) != 0 {
		t.Fatalf("profile = %v, want empty", profile)
	}
}

func TestConcurrencyProfileSingleJob(t *testing.T) {
	peak, _ := concurrencyProfile([][2]time.Time{{dt("10:00:00"), dt("10:10:00")}})
	if peak != 1 {
		t.Fatalf("peak = %d, want 1", peak)
	}
}

func TestConcurrencyProfileFullOverlap(t *testing.T) {
	peak, _ := concurrencyProfile([][2]time.Time{
		{dt("10:00:00"), dt("10:00:10")},
		{dt("10:00:05"), dt("10:00:15")},
	})
	if peak != 2 {
		t.Fatalf("peak = %d, want 2", peak)
	}
}

func TestConcurrencyProfileHandoffNotDoubleCounted(t *testing.T) {
	peak, _ := concurrencyProfile([][2]time.Time{
		{dt("10:00:00"), dt("10:00:01")},
		{dt("10:00:01"), dt("10:00:02")},
	})
	if peak != 1 {
		t.Fatalf("peak = %d, want 1", peak)
	}
}

func TestConcurrencyProfileZeroDurationIgnored(t *testing.T) {
	peak, profile := concurrencyProfile([][2]time.Time{{dt("10:00:00"), dt("10:00:00")}})
	if peak != 0 || len(profile) != 0 {
		t.Fatalf("peak/profile = %d/%v, want 0/empty", peak, profile)
	}
}

func TestConcurrencyProfileNestedIntervals(t *testing.T) {
	peak, _ := concurrencyProfile([][2]time.Time{
		{dt("10:00:00"), dt("10:00:30")},
		{dt("10:00:05"), dt("10:00:20")},
		{dt("10:00:10"), dt("10:00:25")},
	})
	if peak != 3 {
		t.Fatalf("peak = %d, want 3", peak)
	}
}

func TestConcurrencyProfileTimeAtLevel(t *testing.T) {
	_, profile := concurrencyProfile([][2]time.Time{
		{dt("10:00:00"), dt("10:00:10")},
		{dt("10:00:05"), dt("10:00:15")},
	})
	if profile[1] != 10 {
		t.Fatalf("profile[1] = %v, want 10", profile[1])
	}
	if profile[2] != 5 {
		t.Fatalf("profile[2] = %v, want 5", profile[2])
	}
}

func TestConcurrencyProfileMatchesBruteforceGrid(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	var intervals [][2]time.Time
	for i := 0; i < 50; i++ {
		start := rng.Intn(600)
		duration := rng.Intn(120) + 1
		intervals = append(intervals, [2]time.Time{
			dt("10:00:00").Add(time.Duration(start) * time.Second),
			dt("10:00:00").Add(time.Duration(start+duration) * time.Second),
		})
	}
	peak, _ := concurrencyProfile(intervals)
	gridPeak := 0
	for sec := 0; sec < 800; sec++ {
		tick := dt("10:00:00").Add(time.Duration(sec) * time.Second)
		count := 0
		for _, interval := range intervals {
			if (tick.Equal(interval[0]) || tick.After(interval[0])) && tick.Before(interval[1]) {
				count++
			}
		}
		if count > gridPeak {
			gridPeak = count
		}
	}
	if peak != gridPeak {
		t.Fatalf("peak = %d, want %d", peak, gridPeak)
	}
}

func TestPercentilesEmpty(t *testing.T) {
	got := percentiles(nil, []int{50, 90, 95, 99})
	for _, p := range []int{50, 90, 95, 99} {
		if got[p] != 0 {
			t.Fatalf("p%d = %d, want 0", p, got[p])
		}
	}
}

func TestPercentilesWeighted(t *testing.T) {
	got := percentiles(map[int]float64{1: 90, 5: 10}, []int{50, 95})
	if got[50] != 1 {
		t.Fatalf("p50 = %d, want 1", got[50])
	}
	if got[95] != 5 {
		t.Fatalf("p95 = %d, want 5", got[95])
	}
}

func rec(seconds int, osName string, selfHosted bool) record {
	return record{
		Repo:       "x/y",
		Start:      dt("10:00:00"),
		End:        dt("10:00:00").Add(time.Duration(seconds) * time.Second),
		OS:         osName,
		SelfHosted: selfHosted,
	}
}

func TestBillableMinutesRoundsUp(t *testing.T) {
	got := billableMinutes([]record{rec(61, "linux", false)})
	if got["linux"].BillableMinutes != 2 {
		t.Fatalf("billable minutes = %d, want 2", got["linux"].BillableMinutes)
	}
}

func TestBillableMinutesMacOSMultiplier(t *testing.T) {
	got := billableMinutes([]record{rec(60, "macos", false)})
	if got["macos"].BillableMinutes != 10 {
		t.Fatalf("billable minutes = %d, want 10", got["macos"].BillableMinutes)
	}
}

func TestBillableMinutesSelfHostedIsFree(t *testing.T) {
	got := billableMinutes([]record{rec(600, "linux", true)})
	if len(got) != 0 {
		t.Fatalf("billable = %v, want empty", got)
	}
}

func TestBuildReportIncludesRuntimeSeconds(t *testing.T) {
	got := buildReport([]record{rec(60, "linux", false)}, config{
		repos:      []string{"o/r"},
		since:      "2025-05-01",
		baseURL:    defaultBaseURL,
		apiWorkers: 4,
		jobFilter:  "all",
		top:        10,
	}, 1500*time.Millisecond, scanSummary{}, requestStats{})
	if got.RuntimeSeconds != 1.5 {
		t.Fatalf("runtime_seconds = %v, want 1.5", got.RuntimeSeconds)
	}
}

func TestPrintTextIncludesRunTime(t *testing.T) {
	rep := buildReport([]record{rec(60, "linux", false)}, config{
		repos:      []string{"o/r"},
		since:      "2025-05-01",
		baseURL:    defaultBaseURL,
		apiWorkers: 4,
		jobFilter:  "all",
		top:        10,
	}, 2300*time.Millisecond, scanSummary{}, requestStats{})

	var out bytes.Buffer
	printText(&out, rep)
	if !strings.Contains(out.String(), "Run time:             2.3s") {
		t.Fatalf("output missing run time:\n%s", out.String())
	}
}

func TestPrintTextDoesNotDoublePrefixTaggedVersion(t *testing.T) {
	rep := report{
		Version:               "v0.0.4",
		Parameters:            parameters{Repos: []string{"o/r"}, RepositoryCount: 1, Since: "2025-05-01", BaseURL: defaultBaseURL},
		PercentileConcurrency: map[string]int{"p50": 1, "p90": 1, "p95": 1, "p99": 1},
	}

	var out bytes.Buffer
	printText(&out, rep)
	if !strings.Contains(out.String(), "gh-concurrency v0.0.4\n") {
		t.Fatalf("output missing single-prefixed version:\n%s", out.String())
	}
	if strings.Contains(out.String(), "gh-concurrency vv0.0.4") {
		t.Fatalf("output double-prefixed version:\n%s", out.String())
	}
}

func TestDisplayVersionPrefixesPlainSemver(t *testing.T) {
	if got := displayVersion("0.0.4"); got != "v0.0.4" {
		t.Fatalf("displayVersion = %q, want v0.0.4", got)
	}
}

func TestRunnerPools(t *testing.T) {
	records := []record{
		{
			Repo:       "o/api",
			Start:      dt("10:00:00"),
			End:        dt("10:10:00"),
			OS:         "linux",
			SelfHosted: false,
		},
		{
			Repo:       "o/web",
			Start:      dt("10:05:00"),
			End:        dt("10:15:00"),
			OS:         "linux",
			SelfHosted: false,
		},
		{
			Repo:            "o/api",
			Start:           dt("10:00:00"),
			End:             dt("10:08:00"),
			OS:              "linux",
			SelfHosted:      true,
			Labels:          []string{"self-hosted", "linux", "x64", "blacksmith-2vcpu-ubuntu-2404"},
			RunnerGroupName: "Default",
		},
		{
			Repo:            "o/web",
			Start:           dt("10:01:00"),
			End:             dt("10:09:00"),
			OS:              "linux",
			SelfHosted:      true,
			Labels:          []string{"self-hosted", "linux", "x64", "blacksmith-2vcpu-ubuntu-2404"},
			RunnerGroupName: "Default",
		},
		{
			Repo:            "o/mobile",
			Start:           dt("10:02:00"),
			End:             dt("10:10:00"),
			OS:              "windows",
			SelfHosted:      true,
			Labels:          []string{"self-hosted", "windows", "x64", "blacksmith-2vcpu-windows-2022"},
			RunnerGroupName: "Default",
		},
	}

	got := runnerPools(records)
	if len(got) != 2 {
		t.Fatalf("runnerPools returned %d pools, want 2: %#v", len(got), got)
	}
	if got[0].Name != "self-hosted/blacksmith" || got[0].PeakConcurrency != 3 || got[0].Jobs != 3 {
		t.Fatalf("top pool = %#v, want blacksmith peak 3 jobs 3", got[0])
	}
	if got[0].PercentileConcurrency["p95"] != 3 {
		t.Fatalf("blacksmith p95 = %d, want 3", got[0].PercentileConcurrency["p95"])
	}
	if got[1].Name != "GitHub-hosted/linux" || got[1].PeakConcurrency != 2 || got[1].Jobs != 2 {
		t.Fatalf("second pool = %#v, want GitHub-hosted/linux peak 2 jobs 2", got[1])
	}
}

func TestPrintTextIncludesRunnerPools(t *testing.T) {
	rep := report{
		Version:               "test",
		Parameters:            parameters{Repos: []string{"o/r"}, RepositoryCount: 1, Since: "2025-05-01", BaseURL: defaultBaseURL},
		PercentileConcurrency: map[string]int{"p50": 1, "p90": 1, "p95": 1, "p99": 1},
		RunnerPools: []runnerPool{
			{Name: "self-hosted/blacksmith", Jobs: 4120, PeakConcurrency: 48, PercentileConcurrency: map[string]int{"p95": 30}},
		},
	}

	var out bytes.Buffer
	printText(&out, rep)
	text := out.String()
	if !strings.Contains(text, "Runner pools:") {
		t.Fatalf("output missing Runner pools section:\n%s", text)
	}
	if !strings.Contains(text, "self-hosted/blacksmith") || !strings.Contains(text, "4,120 jobs") {
		t.Fatalf("output missing runner pool details:\n%s", text)
	}
}

func TestBuildReportIncludesScanSummaryAndTopSummaries(t *testing.T) {
	records := []record{
		{
			Repo:         "o/api",
			WorkflowName: "CI",
			JobName:      "test",
			Conclusion:   "success",
			Start:        dt("10:00:00"),
			End:          dt("10:20:00"),
			OS:           "linux",
		},
		{
			Repo:         "o/web",
			WorkflowName: "Deploy",
			JobName:      "ship",
			Conclusion:   "failure",
			Start:        dt("10:05:00"),
			End:          dt("10:10:00"),
			OS:           "linux",
		},
	}
	rep := buildReport(records, config{
		repos:               []string{"o/api", "o/web"},
		since:               "2025-05-01",
		baseURL:             defaultBaseURL,
		apiWorkers:          4,
		jobFilter:           "all",
		branch:              "main",
		event:               "push",
		excludePullRequests: true,
		top:                 1,
	}, time.Second, scanSummary{
		RepositoriesQueued:  2,
		RepositoriesScanned: 2,
		WorkflowRuns:        2,
		WorkflowJobs:        2,
		JobsUsed:            2,
		Conclusions:         map[string]int{"success": 1, "failure": 1},
	}, requestStats{Requests: 5, Retries: 1, RateLimitSleeps: 1, RateLimitSleepSeconds: 3})

	if rep.Parameters.APIWorkers != 4 || rep.Parameters.RunStatus != "completed" || rep.Parameters.JobFilter != "all" || rep.Parameters.Branch != "main" || rep.Parameters.Event != "push" || !rep.Parameters.ExcludePullRequests {
		t.Fatalf("parameters = %#v", rep.Parameters)
	}
	if rep.Scan.APIRequests != 5 || rep.Scan.Retries != 1 || rep.Scan.RateLimitSleeps != 1 || rep.Scan.RateLimitSleepSeconds != 3 {
		t.Fatalf("scan API stats = %#v", rep.Scan)
	}
	if len(rep.TopRepositories) != 1 || rep.TopRepositories[0].Name != "o/api" {
		t.Fatalf("top repositories = %#v, want o/api", rep.TopRepositories)
	}
	if len(rep.TopWorkflows) != 1 || rep.TopWorkflows[0].Name != "CI" {
		t.Fatalf("top workflows = %#v, want CI", rep.TopWorkflows)
	}
	if len(rep.TopJobs) != 1 || rep.TopJobs[0].Name != "CI / test" {
		t.Fatalf("top jobs = %#v, want CI / test", rep.TopJobs)
	}
}

func TestPrintTextIncludesScanAndTopSummaries(t *testing.T) {
	rep := report{
		Version: "test",
		Parameters: parameters{
			Repos:           []string{"o/r"},
			RepositoryCount: 1,
			Since:           "2025-05-01",
			BaseURL:         defaultBaseURL,
			APIWorkers:      4,
			RunStatus:       "completed",
			JobFilter:       "all",
			Top:             1,
		},
		Scan: scanSummary{
			RepositoriesQueued:  1,
			RepositoriesScanned: 1,
			WorkflowRuns:        2,
			WorkflowJobs:        3,
			JobsUsed:            2,
			APIRequests:         4,
			Conclusions:         map[string]int{"success": 2},
		},
		PercentileConcurrency: map[string]int{"p50": 1, "p90": 1, "p95": 1, "p99": 1},
		TopRepositories:       []usageSummary{{Name: "o/r", Jobs: 2, BusyHours: 0.5, PeakConcurrency: 1, PercentileConcurrency: map[string]int{"p95": 1}}},
	}

	var out bytes.Buffer
	printText(&out, rep)
	text := out.String()
	for _, want := range []string{"Scan summary:", "API: 4 requests", "Top repositories by busy time:", "o/r"} {
		if !strings.Contains(text, want) {
			t.Fatalf("output missing %q:\n%s", want, text)
		}
	}
}

func TestClassifyRunnerPoolFallbacks(t *testing.T) {
	unknownSelfHosted := classifyRunnerPool(record{SelfHosted: true, OS: "linux"})
	if unknownSelfHosted.name != "self-hosted/unknown" {
		t.Fatalf("self-hosted fallback = %q, want self-hosted/unknown", unknownSelfHosted.name)
	}

	unknownGitHubHosted := classifyRunnerPool(record{})
	if unknownGitHubHosted.name != "GitHub-hosted/unknown" {
		t.Fatalf("github-hosted fallback = %q, want GitHub-hosted/unknown", unknownGitHubHosted.name)
	}
}

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

func TestParseListFile(t *testing.T) {
	got := parseListFile(`
# comments are ignored
buildkite-solutions/gh-concurrency, buildkite-solutions/another
other-org/repo # inline comment

`)
	want := []string{
		"buildkite-solutions/gh-concurrency",
		"buildkite-solutions/another",
		"other-org/repo",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("parseListFile = %v, want %v", got, want)
	}
}

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

func TestResolveTargetReposExpandsOrgsAndFiles(t *testing.T) {
	dir := t.TempDir()
	repoFile := filepath.Join(dir, "repos.txt")
	orgFile := filepath.Join(dir, "orgs.txt")
	if err := os.WriteFile(repoFile, []byte("file-org/file-repo\nacme/api\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(orgFile, []byte("other\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	responses := map[string]fakeResponse{
		"/repos/explicit/repo": {
			body: map[string]any{"full_name": "explicit/repo"},
		},
		"/repos/file-org/file-repo": {
			body: map[string]any{"full_name": "file-org/file-repo"},
		},
		"/repos/acme/api": {
			body: map[string]any{"full_name": "acme/api"},
		},
		"/orgs/acme/repos": {
			body: []map[string]any{
				{"full_name": "acme/api"},
				{"full_name": "acme/web"},
				{"full_name": "acme/archived", "archived": true},
				{"full_name": "acme/disabled", "disabled": true},
			},
		},
		"/orgs/other/repos": {
			body: []map[string]any{
				{"full_name": "other/cli"},
			},
		},
	}
	client := newGitHubClient("https://api.github.com", "tok", 1, false)
	client.httpClient = &http.Client{Transport: fakeTransport{responses: responses}}
	client.sleep = func(time.Duration) {}

	got, skipped, err := resolveTargetRepos(client, config{
		repos:     []string{"explicit/repo"},
		orgs:      []string{"acme"},
		repoFiles: []string{repoFile},
		orgFiles:  []string{orgFile},
		repoType:  "all",
	}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"acme/api", "acme/web", "explicit/repo", "file-org/file-repo", "other/cli"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("resolveTargetRepos = %v, want %v", got, want)
	}
	if len(skipped) != 2 {
		t.Fatalf("skipped = %v, want archived and disabled repos", skipped)
	}
}

func TestResolveTargetReposIncludesArchivedWhenRequested(t *testing.T) {
	responses := map[string]fakeResponse{
		"/orgs/acme/repos": {
			body: []map[string]any{
				{"full_name": "acme/archived", "archived": true},
				{"full_name": "acme/web"},
			},
		},
	}
	client := newGitHubClient("https://api.github.com", "tok", 1, false)
	client.httpClient = &http.Client{Transport: fakeTransport{responses: responses}}
	client.sleep = func(time.Duration) {}

	got, skipped, err := resolveTargetRepos(client, config{
		orgs:            []string{"acme"},
		repoType:        "all",
		includeArchived: true,
	}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"acme/archived", "acme/web"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("resolveTargetRepos = %v, want %v", got, want)
	}
	if len(skipped) != 0 {
		t.Fatalf("skipped = %v, want empty", skipped)
	}
}

func TestResolveTargetReposSkipsArchivedDirectReposByDefault(t *testing.T) {
	responses := map[string]fakeResponse{
		"/repos/acme/live": {
			body: map[string]any{"full_name": "acme/live"},
		},
		"/repos/acme/old": {
			body: map[string]any{"full_name": "acme/old", "archived": true},
		},
	}
	client := newGitHubClient("https://api.github.com", "tok", 1, false)
	client.httpClient = &http.Client{Transport: fakeTransport{responses: responses}}
	client.sleep = func(time.Duration) {}

	got, skipped, err := resolveTargetRepos(client, config{
		repos:    []string{"acme/live", "acme/old"},
		repoType: "all",
	}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"acme/live"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("resolveTargetRepos = %v, want %v", got, want)
	}
	if len(skipped) != 1 || skipped[0].Repo != "acme/old" || skipped[0].Reason != "archived" {
		t.Fatalf("skipped = %v, want acme/old archived", skipped)
	}
}

func TestPaginationAndCollectionOfflineReplay(t *testing.T) {
	responses := map[string]fakeResponse{
		"/repos/o/r/actions/runs": {
			body: map[string]any{"workflow_runs": []map[string]any{{"id": 1}, {"id": 2}}},
		},
		"/repos/o/r/actions/runs/1/jobs": {
			body: map[string]any{"jobs": []map[string]any{
				{
					"started_at":        "2025-05-01T10:00:00Z",
					"completed_at":      "2025-05-01T10:05:00Z",
					"created_at":        "2025-05-01T09:59:00Z",
					"name":              "test linux",
					"workflow_name":     "CI",
					"conclusion":        "success",
					"labels":            []string{"ubuntu-latest"},
					"runner_name":       "GitHub Actions 1",
					"runner_group_name": "GitHub Actions",
				},
			}},
		},
		"/repos/o/r/actions/runs/2/jobs": {
			body: map[string]any{"jobs": []map[string]any{
				{
					"started_at":        "2025-05-01T10:02:00Z",
					"completed_at":      "2025-05-01T10:08:00Z",
					"created_at":        "2025-05-01T10:02:00Z",
					"name":              "test windows",
					"workflow_name":     "CI",
					"conclusion":        "failure",
					"labels":            []string{"self-hosted", "windows", "x64"},
					"runner_name":       "blacksmith-1",
					"runner_group_name": "blacksmith",
				},
				{
					"started_at":   nil,
					"completed_at": nil,
					"labels":       []string{},
				},
			}},
		},
	}
	client := newGitHubClient("https://api.github.com", "tok", 1, false)
	client.httpClient = &http.Client{Transport: fakeTransport{responses: responses}}
	client.sleep = func(time.Duration) {}

	result, err := collectJobs(client, "o/r", collectOptions{Since: "2025-05-01", JobFilter: "all", APIWorkers: 2})
	if err != nil {
		t.Fatal(err)
	}
	records := result.Records
	if len(records) != 2 {
		t.Fatalf("records = %d, want 2", len(records))
	}
	if result.WorkflowRuns != 2 || result.WorkflowJobs != 3 || result.JobsUsed != 2 {
		t.Fatalf("result stats = %#v, want 2 runs, 3 jobs, 2 used", result)
	}
	oses := map[string]bool{}
	for _, rec := range records {
		oses[rec.OS] = true
	}
	if !oses["linux"] || !oses["windows"] {
		t.Fatalf("OSes = %v, want linux and windows", oses)
	}
	if records[1].RunnerName != "blacksmith-1" || records[1].RunnerGroupName != "blacksmith" || !records[1].SelfHosted {
		t.Fatalf("runner metadata = %#v, want self-hosted blacksmith runner", records[1])
	}
	if records[1].WorkflowName != "CI" || records[1].JobName != "test windows" || records[1].Conclusion != "failure" {
		t.Fatalf("job metadata = %#v, want parsed workflow/job/conclusion", records[1])
	}
	peak, _ := concurrencyProfile([][2]time.Time{
		{records[0].Start, records[0].End},
		{records[1].Start, records[1].End},
	})
	if peak != 2 {
		t.Fatalf("peak = %d, want 2", peak)
	}
}

func TestCollectJobsPassesRunAndJobFilters(t *testing.T) {
	var mu sync.Mutex
	var checkErr error
	setCheckErr := func(format string, args ...interface{}) {
		mu.Lock()
		defer mu.Unlock()
		if checkErr == nil {
			checkErr = fmt.Errorf(format, args...)
		}
	}
	client := newGitHubClient("https://api.github.com", "tok", 1, false)
	client.setAPIWorkers(2)
	client.httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/repos/o/r/actions/runs":
			q := req.URL.Query()
			for key, want := range map[string]string{
				"created":               "2025-05-01..2025-05-02",
				"status":                "completed",
				"branch":                "main",
				"event":                 "push",
				"exclude_pull_requests": "true",
				"per_page":              "100",
			} {
				if got := q.Get(key); got != want {
					setCheckErr("runs query %s = %q, want %q (full query %s)", key, got, want, req.URL.RawQuery)
				}
			}
			return fakeHTTPResponse(http.StatusOK, "200 OK", `{"workflow_runs":[{"id":1}]}`, nil), nil
		case "/repos/o/r/actions/runs/1/jobs":
			q := req.URL.Query()
			if got := q.Get("filter"); got != "latest" {
				setCheckErr("jobs filter = %q, want latest (full query %s)", got, req.URL.RawQuery)
			}
			return fakeHTTPResponse(http.StatusOK, "200 OK", `{"jobs":[]}`, nil), nil
		default:
			setCheckErr("unexpected path %s", req.URL.Path)
			return fakeHTTPResponse(http.StatusNotFound, "404 Not Found", `{"message":"missing"}`, nil), nil
		}
	})}
	client.sleep = func(time.Duration) {}

	_, err := collectJobs(client, "o/r", collectOptions{
		Since:               "2025-05-01",
		Until:               "2025-05-02",
		JobFilter:           "latest",
		Branch:              "main",
		Event:               "push",
		ExcludePullRequests: true,
		APIWorkers:          2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if checkErr != nil {
		t.Fatal(checkErr)
	}
}

func TestCollectJobsOmitsStatusWhenIncludingInProgress(t *testing.T) {
	client := newGitHubClient("https://api.github.com", "tok", 1, false)
	client.httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/repos/o/r/actions/runs" {
			t.Fatalf("unexpected path %s", req.URL.Path)
		}
		if got := req.URL.Query().Get("status"); got != "" {
			t.Fatalf("status = %q, want omitted", got)
		}
		return fakeHTTPResponse(http.StatusOK, "200 OK", `{"workflow_runs":[]}`, nil), nil
	})}
	client.sleep = func(time.Duration) {}

	_, err := collectJobs(client, "o/r", collectOptions{Since: "2025-05-01", IncludeInProgress: true, JobFilter: "all", APIWorkers: 1})
	if err != nil {
		t.Fatal(err)
	}
}

func TestListWorkflowRunsForEstimateParsesTotalCount(t *testing.T) {
	client := newGitHubClient("https://api.github.com", "tok", 1, false)
	client.httpClient = &http.Client{Transport: fakeTransport{responses: map[string]fakeResponse{
		"/repos/o/r/actions/runs": {
			body: map[string]any{
				"total_count": 5,
				"workflow_runs": []map[string]any{{
					"id":             1,
					"name":           "CI",
					"workflow_id":    99,
					"event":          "push",
					"head_branch":    "main",
					"status":         "completed",
					"conclusion":     "success",
					"created_at":     "2025-05-01T09:59:00Z",
					"run_started_at": "2025-05-01T10:00:00Z",
					"updated_at":     "2025-05-01T10:10:00Z",
				}},
			},
		},
	}}}
	client.sleep = func(time.Duration) {}

	runs, total, complete, err := listWorkflowRunsForEstimate(client, "o/r", collectOptions{Since: "2025-05-01"})
	if err != nil {
		t.Fatal(err)
	}
	if total != 5 || !complete || len(runs) != 1 {
		t.Fatalf("runs/total/complete = %d/%d/%v, want 1/5/true", len(runs), total, complete)
	}
	if runs[0].Repo != "o/r" || runs[0].WorkflowID != 99 || runs[0].Event != "push" {
		t.Fatalf("run = %#v", runs[0])
	}
}

func TestGetWorkflowRunCountForEstimatePassesFilters(t *testing.T) {
	var checked bool
	client := newGitHubClient("https://api.github.com", "tok", 1, false)
	client.httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/repos/o/r/actions/runs" {
			t.Fatalf("unexpected path %s", req.URL.Path)
		}
		q := req.URL.Query()
		for key, want := range map[string]string{
			"created":               "2025-05-01..2025-05-02",
			"status":                "completed",
			"branch":                "main",
			"event":                 "push",
			"exclude_pull_requests": "true",
			"per_page":              "1",
		} {
			if got := q.Get(key); got != want {
				t.Fatalf("query %s = %q, want %q (full query %s)", key, got, want, req.URL.RawQuery)
			}
		}
		checked = true
		return fakeHTTPResponse(http.StatusOK, "200 OK", `{"total_count":7,"workflow_runs":[]}`, nil), nil
	})}
	client.sleep = func(time.Duration) {}

	count, err := getWorkflowRunCountForEstimate(client, "o/r", collectOptions{
		Since:               "2025-05-01",
		Until:               "2025-05-02",
		Branch:              "main",
		Event:               "push",
		ExcludePullRequests: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !checked || count != 7 {
		t.Fatalf("checked/count = %v/%d, want true/7", checked, count)
	}
}

func TestEstimateRepositoryLandscapeRanksAndSelectsByActionsActivity(t *testing.T) {
	responses := map[string]fakeResponse{
		"/repos/o/large/actions/runs": {body: map[string]any{"total_count": 40, "workflow_runs": []map[string]any{{"id": 1}}}},
		"/repos/o/small/actions/runs": {body: map[string]any{"total_count": 200, "workflow_runs": []map[string]any{{"id": 2}}}},
		"/repos/o/quiet/actions/runs": {body: map[string]any{"total_count": 1, "workflow_runs": []map[string]any{{"id": 3}}}},
	}
	client := newGitHubClient("https://api.github.com", "tok", 1, false)
	client.httpClient = &http.Client{Transport: fakeTransport{responses: responses}}
	client.sleep = func(time.Duration) {}

	infos := map[string]repositoryInfo{
		repoInfoKey("o/large"): {FullName: "o/large", Size: 500000, PushedAt: "2025-05-01T12:00:00Z", OpenIssuesCount: 10},
		repoInfoKey("o/small"): {FullName: "o/small", Size: 100, PushedAt: "2025-05-01T12:00:00Z"},
		repoInfoKey("o/quiet"): {FullName: "o/quiet", Size: 900000, PushedAt: "2025-05-01T12:00:00Z"},
	}
	landscape, selected, warnings, err := buildEstimateRepositoryLandscape(
		client,
		[]string{"o/quiet", "o/large", "o/small"},
		infos,
		collectOptions{Since: "2025-05-01"},
		config{estimateRepoLimit: 2},
		io.Discard,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want empty", warnings)
	}
	if strings.Join(selected, ",") != "o/small,o/large" {
		t.Fatalf("selected = %v, want small then large", selected)
	}
	if landscape.Repositories[0].Repo != "o/small" || landscape.Repositories[0].WorkflowRunCount != 200 || !landscape.Repositories[0].Selected {
		t.Fatalf("top landscape entry = %#v", landscape.Repositories[0])
	}
	if landscape.Repositories[2].Selected {
		t.Fatalf("third repo should not be selected: %#v", landscape.Repositories[2])
	}
}

func TestEstimateRepositoryLandscapeDefaultKeepsAllReposButRanksThem(t *testing.T) {
	responses := map[string]fakeResponse{
		"/repos/o/busy/actions/runs":  {body: map[string]any{"total_count": 30, "workflow_runs": []map[string]any{{"id": 1}}}},
		"/repos/o/quiet/actions/runs": {body: map[string]any{"total_count": 2, "workflow_runs": []map[string]any{{"id": 2}}}},
	}
	client := newGitHubClient("https://api.github.com", "tok", 1, false)
	client.httpClient = &http.Client{Transport: fakeTransport{responses: responses}}
	client.sleep = func(time.Duration) {}

	landscape, selected, _, err := buildEstimateRepositoryLandscape(
		client,
		[]string{"o/quiet", "o/busy"},
		map[string]repositoryInfo{
			repoInfoKey("o/busy"):  {FullName: "o/busy", Size: 10},
			repoInfoKey("o/quiet"): {FullName: "o/quiet", Size: 1000},
		},
		collectOptions{Since: "2025-05-01"},
		config{},
		io.Discard,
	)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(selected, ",") != "o/busy,o/quiet" {
		t.Fatalf("selected = %v, want all repos in ranked order", selected)
	}
	if !landscape.Repositories[0].Selected || !landscape.Repositories[1].Selected || landscape.SelectedRepos != 2 {
		t.Fatalf("landscape selection = %#v", landscape)
	}
}

func TestEstimateRepositoryLandscapeUsesMetadataWhenActivityProbeStops(t *testing.T) {
	client := newGitHubClient("https://api.github.com", "tok", 1, false)
	client.enableRequestBudget(1, 0)
	defer client.disableRequestBudget()
	client.httpClient = &http.Client{Transport: fakeTransport{responses: map[string]fakeResponse{
		"/repos/o/large/actions/runs": {body: map[string]any{"total_count": 0, "workflow_runs": []map[string]any{}}},
	}}}
	client.sleep = func(time.Duration) {}

	landscape, selected, warnings, err := buildEstimateRepositoryLandscape(
		client,
		[]string{"o/small", "o/large"},
		map[string]repositoryInfo{
			repoInfoKey("o/large"): {FullName: "o/large", Size: 1000000, PushedAt: "2025-05-01T12:00:00Z"},
			repoInfoKey("o/small"): {FullName: "o/small", Size: 1, PushedAt: "2025-05-01T12:00:00Z"},
		},
		collectOptions{Since: "2025-05-01"},
		config{},
		io.Discard,
	)
	if err != nil {
		t.Fatal(err)
	}
	if landscape.ProbeComplete || landscape.StopReason == "" {
		t.Fatalf("landscape probe state = complete %v stop %q, want partial with stop reason", landscape.ProbeComplete, landscape.StopReason)
	}
	if len(warnings) == 0 || !strings.Contains(strings.Join(warnings, "\n"), "landscape probing stopped") {
		t.Fatalf("warnings = %v, want partial landscape warning", warnings)
	}
	if strings.Join(selected, ",") != "o/large,o/small" {
		t.Fatalf("selected = %v, want metadata-ranked repos", selected)
	}
}

func TestEstimateRepositoryLandscapeCapsProbesToPreserveEstimateBudget(t *testing.T) {
	calls := 0
	client := newGitHubClient("https://api.github.com", "tok", 1, false)
	client.httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		return fakeHTTPResponse(http.StatusOK, "200 OK", `{"total_count":99,"workflow_runs":[]}`, nil), nil
	})}
	client.sleep = func(time.Duration) {}

	landscape, selected, warnings, err := buildEstimateRepositoryLandscape(
		client,
		[]string{"o/small", "o/large"},
		map[string]repositoryInfo{
			repoInfoKey("o/large"): {FullName: "o/large", Size: 1000000},
			repoInfoKey("o/small"): {FullName: "o/small", Size: 1},
		},
		collectOptions{Since: "2025-05-01"},
		config{estimateMaxRequests: 2, estimateSampleRuns: 1},
		io.Discard,
	)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("landscape made %d API calls, want none when preserving a tiny estimate budget", calls)
	}
	if landscape.ProbeComplete || !strings.Contains(landscape.StopReason, "preserve estimate request budget") {
		t.Fatalf("landscape probe state = complete %v stop %q", landscape.ProbeComplete, landscape.StopReason)
	}
	if len(warnings) == 0 || !strings.Contains(strings.Join(warnings, "\n"), "landscape probing stopped") {
		t.Fatalf("warnings = %v, want partial landscape warning", warnings)
	}
	if strings.Join(selected, ",") != "o/large,o/small" {
		t.Fatalf("selected = %v, want metadata-ranked repos", selected)
	}
}

func TestSelectSampleRunsDeterministic(t *testing.T) {
	var runs []workflowRun
	for i := 0; i < 10; i++ {
		runs = append(runs, workflowRun{
			ID:           int64(i + 1),
			Repo:         "o/r",
			Name:         "CI",
			WorkflowID:   1,
			Event:        "push",
			RunStartedAt: dt("10:00:00").Add(time.Duration(i) * time.Minute).Format(time.RFC3339),
		})
	}
	a := selectSampleRuns(runs, 4, 42)
	b := selectSampleRuns(runs, 4, 42)
	c := selectSampleRuns(runs, 4, 43)
	if workflowRunIDs(a) != workflowRunIDs(b) {
		t.Fatalf("same seed produced different samples: %s vs %s", workflowRunIDs(a), workflowRunIDs(b))
	}
	if workflowRunIDs(a) == workflowRunIDs(c) {
		t.Fatalf("different seed produced same sample: %s", workflowRunIDs(a))
	}
}

func workflowRunIDs(runs []workflowRun) string {
	var ids []string
	for _, run := range runs {
		ids = append(ids, strconv.FormatInt(run.ID, 10))
	}
	return strings.Join(ids, ",")
}

func TestRequestBudgetStopsBeforeMaxRequests(t *testing.T) {
	client := newGitHubClient("https://api.github.com", "tok", 1, false)
	client.enableRequestBudget(1, 0)
	client.httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return fakeHTTPResponse(http.StatusOK, "200 OK", `{"ok":true}`, nil), nil
	})}
	client.sleep = func(time.Duration) {}

	if _, _, err := client.request("https://api.github.com/one"); err != nil {
		t.Fatal(err)
	}
	_, _, err := client.request("https://api.github.com/two")
	var stopErr requestBudgetStopError
	if !errors.As(err, &stopErr) {
		t.Fatalf("err = %v, want requestBudgetStopError", err)
	}
	if !strings.Contains(stopErr.Reason, "budget") {
		t.Fatalf("stop reason = %q, want budget", stopErr.Reason)
	}
}

func TestSimulateEstimateIntervalsContainExactSyntheticValue(t *testing.T) {
	run1 := workflowRun{ID: 1, Repo: "o/r", Name: "CI", WorkflowID: 1, Event: "push", RunStartedAt: dt("10:00:00").Format(time.RFC3339)}
	run2 := workflowRun{ID: 2, Repo: "o/r", Name: "CI", WorkflowID: 1, Event: "push", RunStartedAt: dt("10:05:00").Format(time.RFC3339)}
	sampled := []sampledRun{{
		Run: run1,
		Records: []record{{
			Repo:         "o/r",
			WorkflowName: "CI",
			JobName:      "test",
			Start:        dt("10:00:00"),
			End:          dt("10:10:00"),
			OS:           "linux",
		}},
	}}
	metrics, warnings := simulateEstimate([]workflowRun{run1, run2}, sampled, config{
		estimateSeed:       7,
		estimateIterations: 50,
		estimateConfidence: 90,
	})
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want empty", warnings)
	}
	if metrics.JobsAnalyzed.Median != 2 {
		t.Fatalf("jobs median = %v, want 2", metrics.JobsAnalyzed.Median)
	}
	if metrics.PeakConcurrency.Lower > 2 || metrics.PeakConcurrency.Upper < 2 {
		t.Fatalf("peak interval = %#v, want to contain 2", metrics.PeakConcurrency)
	}
	if metrics.PercentileConcurrency["p95"].Lower > 2 || metrics.PercentileConcurrency["p95"].Upper < 2 {
		t.Fatalf("p95 interval = %#v, want to contain 2", metrics.PercentileConcurrency["p95"])
	}
}

func TestPrintTextEstimateModeIsProminent(t *testing.T) {
	rep := report{
		Version: "test",
		Parameters: parameters{
			Repos:           []string{"o/r"},
			RepositoryCount: 1,
			Since:           "2025-05-01",
			BaseURL:         defaultBaseURL,
			APIWorkers:      4,
			RunStatus:       "completed",
			JobFilter:       "all",
			Top:             10,
			Mode:            "estimate",
		},
		Scan:                  scanSummary{RepositoriesQueued: 1, RepositoriesScanned: 1, WorkflowRuns: 10, WorkflowJobs: 5, JobsUsed: 5, APIRequests: 7},
		PercentileConcurrency: map[string]int{"p50": 1, "p90": 2, "p95": 3, "p99": 4},
		Estimate: &estimateReport{
			Seed:        42,
			Confidence:  90,
			SampledRuns: 3,
			KnownRuns:   10,
			Warnings:    []string{"Peak concurrency is sensitive to rare unsampled fan-out; run exact mode before final commitments."},
			Metrics: estimateMetrics{
				JobsAnalyzed:    estimateInterval{Median: 12, Lower: 9, Upper: 20},
				BusyHours:       estimateInterval{Median: 1.5, Lower: 1, Upper: 2},
				PeakConcurrency: estimateInterval{Median: 4, Lower: 2, Upper: 8},
				PercentileConcurrency: map[string]estimateInterval{
					"p50": {Median: 1, Lower: 1, Upper: 2},
					"p90": {Median: 2, Lower: 1, Upper: 4},
					"p95": {Median: 3, Lower: 2, Upper: 5},
					"p99": {Median: 4, Lower: 2, Upper: 8},
				},
			},
			RepositoryLandscape: &estimateRepositoryLandscape{
				Strategy:      "actions_runs_then_metadata",
				RankedRepos:   1,
				SelectedRepos: 1,
				ProbeComplete: true,
				Repositories: []estimateRepositorySignal{{
					Rank:                  1,
					Repo:                  "o/r",
					WorkflowRunCount:      10,
					WorkflowRunCountKnown: true,
					Size:                  123,
					PushedAt:              "2025-05-01T12:00:00Z",
					Selected:              true,
					SelectionReason:       "selected; no repository limit",
				}},
			},
		},
	}
	var out bytes.Buffer
	printText(&out, rep)
	text := out.String()
	for _, want := range []string{"ESTIMATE MODE", "sampled 3 of 10", "Repository landscape", "#1 o/r", "Peak concurrency:     median 4 (90% range 2-8)", "not billing-grade exact"} {
		if !strings.Contains(text, want) {
			t.Fatalf("output missing %q:\n%s", want, text)
		}
	}
}

func TestRunEstimateBuildsReportFromFakeAPI(t *testing.T) {
	responses := map[string]fakeResponse{
		"/repos/o/r/actions/runs": {
			body: map[string]any{
				"total_count": 2,
				"workflow_runs": []map[string]any{
					{"id": 1, "name": "CI", "workflow_id": 10, "event": "push", "conclusion": "success", "created_at": "2025-05-01T09:59:00Z", "run_started_at": "2025-05-01T10:00:00Z"},
					{"id": 2, "name": "CI", "workflow_id": 10, "event": "push", "conclusion": "success", "created_at": "2025-05-01T10:04:00Z", "run_started_at": "2025-05-01T10:05:00Z"},
				},
			},
		},
		"/repos/o/r/actions/runs/1/jobs": {
			body: map[string]any{"jobs": []map[string]any{{
				"started_at":    "2025-05-01T10:00:00Z",
				"completed_at":  "2025-05-01T10:10:00Z",
				"created_at":    "2025-05-01T09:59:00Z",
				"name":          "test",
				"workflow_name": "CI",
				"conclusion":    "success",
				"labels":        []string{"ubuntu-latest"},
			}}},
		},
		"/repos/o/r/actions/runs/2/jobs": {
			body: map[string]any{"jobs": []map[string]any{{
				"started_at":    "2025-05-01T10:05:00Z",
				"completed_at":  "2025-05-01T10:15:00Z",
				"created_at":    "2025-05-01T10:04:00Z",
				"name":          "test",
				"workflow_name": "CI",
				"conclusion":    "success",
				"labels":        []string{"ubuntu-latest"},
			}}},
		},
	}
	client := newGitHubClient("https://api.github.com", "tok", 1, false)
	client.httpClient = &http.Client{Transport: fakeTransport{responses: responses}}
	client.sleep = func(time.Duration) {}

	rep, err := runEstimate(client, config{
		repos:                []string{"o/r"},
		since:                "2025-05-01",
		baseURL:              defaultBaseURL,
		apiWorkers:           1,
		jobFilter:            "all",
		estimate:             true,
		estimateMaxRequests:  20,
		estimateMinRemaining: 0,
		estimateSampleRuns:   2,
		estimateIterations:   20,
		estimateConfidence:   90,
		estimateSeed:         123,
	}, nil, nil, time.Now(), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Parameters.Mode != "estimate" || rep.Estimate == nil {
		t.Fatalf("report mode/estimate = %q/%#v", rep.Parameters.Mode, rep.Estimate)
	}
	if rep.Estimate.SampledRuns != 2 || rep.Estimate.KnownRuns != 2 || rep.Estimate.SampleFraction != 1 {
		t.Fatalf("estimate summary = %#v", rep.Estimate)
	}
	if rep.Estimate.Metrics.PeakConcurrency.Median != 2 {
		t.Fatalf("peak median = %v, want 2", rep.Estimate.Metrics.PeakConcurrency.Median)
	}
	if rep.Estimate.RepositoryLandscape == nil || rep.Estimate.RepositoryLandscape.SelectedRepos != 1 {
		t.Fatalf("repository landscape = %#v", rep.Estimate.RepositoryLandscape)
	}
}

func TestEstimateModeDoesNotSleepOnPrimaryRateLimit(t *testing.T) {
	headers := make(http.Header)
	headers.Set("X-RateLimit-Remaining", "0")
	headers.Set("X-RateLimit-Reset", "9999999999")
	client := newGitHubClient("https://api.github.com", "tok", 2, false)
	client.enableRequestBudget(10, 0)
	client.httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return fakeHTTPResponse(http.StatusForbidden, "403 Forbidden", `{"message":"rate limit"}`, headers), nil
	})}
	client.sleep = func(d time.Duration) {
		t.Fatalf("estimate mode should not sleep on rate-limit reset, slept %v", d)
	}

	_, _, err := client.request("https://api.github.com/x")
	var stopErr requestBudgetStopError
	if !errors.As(err, &stopErr) {
		t.Fatalf("err = %v, want requestBudgetStopError", err)
	}
}

func TestCollectRepositoriesWorkerCountsProduceSameRecords(t *testing.T) {
	responses := map[string]fakeResponse{
		"/repos/o/api/actions/runs": {
			body: map[string]any{"workflow_runs": []map[string]any{{"id": 1}}},
		},
		"/repos/o/api/actions/runs/1/jobs": {
			body: map[string]any{"jobs": []map[string]any{{
				"started_at":    "2025-05-01T10:00:00Z",
				"completed_at":  "2025-05-01T10:10:00Z",
				"created_at":    "2025-05-01T09:59:00Z",
				"name":          "test",
				"workflow_name": "CI",
				"conclusion":    "success",
				"labels":        []string{"ubuntu-latest"},
			}}},
		},
		"/repos/o/web/actions/runs": {
			body: map[string]any{"workflow_runs": []map[string]any{{"id": 2}}},
		},
		"/repos/o/web/actions/runs/2/jobs": {
			body: map[string]any{"jobs": []map[string]any{{
				"started_at":    "2025-05-01T10:05:00Z",
				"completed_at":  "2025-05-01T10:15:00Z",
				"created_at":    "2025-05-01T10:04:00Z",
				"name":          "test",
				"workflow_name": "CI",
				"conclusion":    "success",
				"labels":        []string{"ubuntu-latest"},
			}}},
		},
	}

	collect := func(workers int) ([]record, scanSummary) {
		client := newGitHubClient("https://api.github.com", "tok", 1, false)
		client.setAPIWorkers(workers)
		client.httpClient = &http.Client{Transport: fakeTransport{responses: responses}}
		client.sleep = func(time.Duration) {}
		records, summary, err := collectRepositories(
			client,
			[]string{"o/api", "o/web"},
			collectOptions{Since: "2025-05-01", JobFilter: "all", APIWorkers: workers},
			newProgressReporter(io.Discard, false, 2),
			nil,
			io.Discard,
		)
		if err != nil {
			t.Fatal(err)
		}
		return records, summary
	}

	records1, summary1 := collect(1)
	records4, summary4 := collect(4)
	if recordSignature(records1) != recordSignature(records4) {
		t.Fatalf("records differ:\nworkers=1 %s\nworkers=4 %s", recordSignature(records1), recordSignature(records4))
	}
	if summary1.WorkflowRuns != summary4.WorkflowRuns || summary1.WorkflowJobs != summary4.WorkflowJobs || summary1.JobsUsed != summary4.JobsUsed {
		t.Fatalf("summaries differ: %#v vs %#v", summary1, summary4)
	}
}

func recordSignature(records []record) string {
	var parts []string
	for _, rec := range records {
		parts = append(parts, strings.Join([]string{
			rec.Repo,
			rec.WorkflowName,
			rec.JobName,
			rec.Start.Format(time.RFC3339),
			rec.End.Format(time.RFC3339),
		}, "|"))
	}
	return strings.Join(parts, "\n")
}

func TestSecondaryRateLimitBackoffRetries(t *testing.T) {
	calls := 0
	var sleeps []time.Duration
	client := newGitHubClient("https://api.github.com", "tok", 2, false)
	client.setAPIWorkers(2)
	client.httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return fakeHTTPResponse(http.StatusForbidden, "403 Forbidden", `{"message":"You have exceeded a secondary rate limit"}`, nil), nil
		}
		return fakeHTTPResponse(http.StatusOK, "200 OK", `{"ok":true}`, nil), nil
	})}
	client.sleep = func(d time.Duration) {
		sleeps = append(sleeps, d)
	}

	body, _, err := client.request("https://api.github.com/x")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(body)) != `{"ok":true}` {
		t.Fatalf("body = %s", body)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
	if len(sleeps) != 1 || sleeps[0] < time.Minute {
		t.Fatalf("sleeps = %v, want one secondary backoff >= 1m", sleeps)
	}
	stats := client.statsSnapshot()
	if stats.Requests != 2 || stats.Retries != 1 || stats.RateLimitSleeps != 1 {
		t.Fatalf("stats = %#v, want 2 requests, 1 retry, 1 rate-limit sleep", stats)
	}
}

func TestRetryAfterPausesSharedRequestGate(t *testing.T) {
	headers := make(http.Header)
	headers.Set("Retry-After", "2")

	var mu sync.Mutex
	calls := 0
	client := newGitHubClient("https://api.github.com", "tok", 2, false)
	client.setAPIWorkers(2)
	client.httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		mu.Lock()
		calls++
		call := calls
		mu.Unlock()
		if call == 1 {
			return fakeHTTPResponse(http.StatusForbidden, "403 Forbidden", `{"message":"slow down"}`, headers), nil
		}
		return fakeHTTPResponse(http.StatusOK, "200 OK", `{"ok":true}`, nil), nil
	})}

	sleepStarted := make(chan struct{})
	releaseSleep := make(chan struct{})
	var sleepOnce sync.Once
	client.sleep = func(d time.Duration) {
		if d >= 3*time.Second {
			sleepOnce.Do(func() { close(sleepStarted) })
			<-releaseSleep
		}
	}

	firstDone := make(chan error, 1)
	go func() {
		_, _, err := client.request("https://api.github.com/first")
		firstDone <- err
	}()
	<-sleepStarted

	secondDone := make(chan error, 1)
	go func() {
		_, _, err := client.request("https://api.github.com/second")
		secondDone <- err
	}()

	select {
	case err := <-secondDone:
		t.Fatalf("second request completed during shared cooldown: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(releaseSleep)

	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
	stats := client.statsSnapshot()
	if stats.RateLimitSleeps != 1 || stats.RateLimitSleepSeconds != 3 {
		t.Fatalf("stats = %#v, want one 3s rate-limit sleep", stats)
	}
}

func TestClientLimitsConcurrentRequests(t *testing.T) {
	var mu sync.Mutex
	inFlight := 0
	maxInFlight := 0
	client := newGitHubClient("https://api.github.com", "tok", 1, false)
	client.setAPIWorkers(2)
	client.httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		mu.Lock()
		inFlight++
		if inFlight > maxInFlight {
			maxInFlight = inFlight
		}
		mu.Unlock()

		time.Sleep(20 * time.Millisecond)

		mu.Lock()
		inFlight--
		mu.Unlock()
		return fakeHTTPResponse(http.StatusOK, "200 OK", `{"ok":true}`, nil), nil
	})}

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, err := client.request("https://api.github.com/x/" + strconv.Itoa(i))
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if maxInFlight > 2 {
		t.Fatalf("max in-flight requests = %d, want <= 2", maxInFlight)
	}
}

func TestDebugRequestLogging(t *testing.T) {
	var logs bytes.Buffer
	headers := make(http.Header)
	headers.Set("X-RateLimit-Remaining", "4999")
	headers.Set("X-RateLimit-Reset", "1746122400")

	client := newGitHubClient("https://api.github.com", "tok", 1, false)
	client.debug = true
	client.logWriter = &logs
	client.httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return fakeHTTPResponse(http.StatusOK, "200 OK", `{"ok":true}`, headers), nil
	})}
	client.sleep = func(time.Duration) {}

	if _, _, err := client.request("https://api.github.com/repos/o/r/actions/runs?per_page=100"); err != nil {
		t.Fatal(err)
	}
	out := logs.String()
	for _, want := range []string{
		"GET https://api.github.com/repos/o/r/actions/runs?per_page=100",
		"200 OK",
		"remaining=4999",
		"reset=2025-05-01T18:00:00Z",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("debug log missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "tok") {
		t.Fatalf("debug log leaked token:\n%s", out)
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

func TestResolveCircleCIProjectsExpandsReposAndFiles(t *testing.T) {
	dir := t.TempDir()
	repoFile := filepath.Join(dir, "repos.txt")
	projectFile := filepath.Join(dir, "projects.txt")
	if err := os.WriteFile(repoFile, []byte("acme/web\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(projectFile, []byte("circleci/org-id/project-id\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := resolveCircleCIProjects(config{
		provider:             circleCIProvider,
		repos:                []string{"acme/api"},
		repoFiles:            []string{repoFile},
		circleCIProjects:     []string{"gh/acme/mobile"},
		circleCIProjectFiles: []string{projectFile},
		circleCIVCS:          "gh",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"circleci/org-id/project-id", "gh/acme/api", "gh/acme/mobile", "gh/acme/web"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("resolveCircleCIProjects = %v, want %v", got, want)
	}
}

func TestCircleCIClientRequestUsesBearerToken(t *testing.T) {
	client := newCircleCIClient("https://circleci.com/api/v2", "circle-token", 1, false)
	client.httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if got := req.Header.Get("Authorization"); got != "Bearer circle-token" {
			t.Fatalf("Authorization = %q, want bearer token", got)
		}
		if got := req.Header.Get("Accept"); got != "application/json" {
			t.Fatalf("Accept = %q, want application/json", got)
		}
		return fakeHTTPResponse(http.StatusOK, "200 OK", `{"ok":true}`, nil), nil
	})}
	client.sleep = func(time.Duration) {}

	if _, err := client.request("https://circleci.com/api/v2/me"); err != nil {
		t.Fatal(err)
	}
}

func TestCollectCircleCIProjectJobsUsesDetailsAndParallelism(t *testing.T) {
	var sawBranch bool
	var sawPageToken bool
	client := newCircleCIClient("https://circleci.com/api/v2", "tok", 1, false)
	client.setAPIWorkers(2)
	client.httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/api/v2/project/gh/acme/api/pipeline":
			q := req.URL.Query()
			if got := q.Get("branch"); got != "main" {
				t.Fatalf("branch = %q, want main", got)
			}
			sawBranch = true
			if q.Get("page-token") == "older" {
				sawPageToken = true
				return fakeHTTPResponse(http.StatusOK, "200 OK", `{"items":[{"id":"old","created_at":"2025-04-30T23:59:59Z","project_slug":"gh/acme/api"}]}`, nil), nil
			}
			return fakeHTTPResponse(http.StatusOK, "200 OK", `{"items":[{"id":"p1","created_at":"2025-05-01T09:00:00Z","project_slug":"gh/acme/api"}],"next_page_token":"older"}`, nil), nil
		case "/api/v2/pipeline/p1/workflow":
			return fakeHTTPResponse(http.StatusOK, "200 OK", `{"items":[{"id":"w1","name":"build","status":"success"}]}`, nil), nil
		case "/api/v2/workflow/w1/job":
			return fakeHTTPResponse(http.StatusOK, "200 OK", `{"items":[
				{"job_number":11,"name":"test","status":"success","project_slug":"gh/acme/api"},
				{"job_number":12,"name":"hold","status":"blocked","type":"approval","project_slug":"gh/acme/api"}
			]}`, nil), nil
		case "/api/v2/project/gh/acme/api/job/11":
			return fakeHTTPResponse(http.StatusOK, "200 OK", `{
				"number":11,
				"name":"test",
				"status":"success",
				"started_at":"2025-05-01T10:00:00Z",
				"stopped_at":"2025-05-01T10:10:00Z",
				"queued_at":"2025-05-01T09:58:00Z",
				"parallelism":2,
				"executor":{"resource_class":"large","type":"docker"},
				"latest_workflow":{"name":"build"}
			}`, nil), nil
		case "/api/v2/project/gh/acme/api/job/12":
			return fakeHTTPResponse(http.StatusOK, "200 OK", `{"number":12,"name":"hold","status":"blocked"}`, nil), nil
		default:
			t.Fatalf("unexpected request path %s", req.URL.Path)
			return fakeHTTPResponse(http.StatusNotFound, "404 Not Found", `{"message":"missing"}`, nil), nil
		}
	})}
	client.sleep = func(time.Duration) {}

	result, err := collectCircleCIProjectJobs(client, "gh/acme/api", collectOptions{
		Since:              "2025-05-01",
		Branch:             "main",
		APIWorkers:         2,
		CircleCIJobDetails: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !sawBranch || !sawPageToken {
		t.Fatalf("saw branch/page-token = %v/%v, want true/true", sawBranch, sawPageToken)
	}
	if result.Pipelines != 1 || result.WorkflowRuns != 1 || result.WorkflowJobs != 2 || result.JobsUsed != 2 {
		t.Fatalf("result stats = %#v, want 1 pipeline, 1 workflow, 2 jobs seen, 2 slots used", result)
	}
	if len(result.Records) != 2 {
		t.Fatalf("records = %d, want 2 parallel slots", len(result.Records))
	}
	for _, rec := range result.Records {
		if rec.Provider != circleCIProvider || rec.ResourceClass != "large" || rec.Executor != "docker" || rec.Parallelism != 2 {
			t.Fatalf("record CircleCI metadata = %#v", rec)
		}
		if rec.QueueSeconds == nil || *rec.QueueSeconds != 120 {
			t.Fatalf("queue seconds = %v, want 120", rec.QueueSeconds)
		}
	}
	pools := runnerPools(result.Records)
	if len(pools) != 1 || pools[0].Name != "CircleCI/large" || pools[0].PeakConcurrency != 2 {
		t.Fatalf("runner pools = %#v, want CircleCI/large peak 2", pools)
	}
	peak, _ := concurrencyProfile([][2]time.Time{
		{result.Records[0].Start, result.Records[0].End},
		{result.Records[1].Start, result.Records[1].End},
	})
	if peak != 2 {
		t.Fatalf("peak = %d, want 2", peak)
	}
}

func TestBuildReportCircleCIOmitsGitHubBillableMinutes(t *testing.T) {
	records := []record{{
		Provider:      circleCIProvider,
		Repo:          "gh/acme/api",
		WorkflowName:  "build",
		JobName:       "test",
		Conclusion:    "success",
		Start:         dt("10:00:00"),
		End:           dt("10:10:00"),
		ResourceClass: "medium",
		Executor:      "docker",
	}}
	rep := buildReport(records, config{
		provider:           circleCIProvider,
		repos:              []string{"gh/acme/api"},
		circleCIProjects:   []string{"gh/acme/api"},
		circleCIVCS:        "gh",
		circleCIJobDetails: true,
		since:              "2025-05-01",
		baseURL:            defaultCircleCIBaseURL,
		apiWorkers:         1,
		jobFilter:          "all",
		top:                10,
	}, time.Second, scanSummary{RepositoriesQueued: 1, RepositoriesScanned: 1, Pipelines: 1, WorkflowRuns: 1, WorkflowJobs: 1, JobsUsed: 1}, requestStats{Requests: 4})

	if rep.Parameters.Provider != circleCIProvider || rep.Parameters.RepositoryCount != 1 {
		t.Fatalf("parameters = %#v", rep.Parameters)
	}
	if rep.BillableMinutesEstimate != nil {
		t.Fatalf("billable minutes = %#v, want nil for CircleCI", rep.BillableMinutesEstimate)
	}
	var out bytes.Buffer
	printText(&out, rep)
	text := out.String()
	for _, want := range []string{"projects:", "project count: 1", "pipelines: 1", "CircleCI/medium"} {
		if !strings.Contains(text, want) {
			t.Fatalf("output missing %q:\n%s", want, text)
		}
	}
}

type fakeResponse struct {
	body any
	link string
}

type fakeTransport struct {
	responses map[string]fakeResponse
}

func (t fakeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, ok := t.responses[req.URL.Path]
	if !ok {
		return fakeHTTPResponse(http.StatusNotFound, "404 Not Found", `{"message":"missing"}`, nil), nil
	}
	data, err := json.Marshal(resp.body)
	if err != nil {
		return nil, err
	}
	headers := make(http.Header)
	if resp.link != "" {
		headers.Set("Link", resp.link)
	}
	return fakeHTTPResponse(http.StatusOK, "200 OK", string(data), headers), nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func fakeHTTPResponse(statusCode int, status string, body string, headers http.Header) *http.Response {
	if headers == nil {
		headers = make(http.Header)
	}
	return &http.Response{
		StatusCode: statusCode,
		Status:     status,
		Header:     headers,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}
