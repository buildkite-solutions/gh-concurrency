package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

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
