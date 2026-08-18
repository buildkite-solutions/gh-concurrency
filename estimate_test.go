package main

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

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
	metrics, compute, warnings := simulateEstimate([]workflowRun{run1, run2}, sampled, config{
		estimateSeed:       7,
		estimateIterations: 50,
		estimateConfidence: 90,
	})
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want empty", warnings)
	}
	if compute != nil {
		t.Fatalf("compute = %#v, want nil without resource mapping", compute)
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

func TestSimulateEstimateIncludesVCPUIntervals(t *testing.T) {
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
			Labels:       []string{"large"},
		}},
	}}
	metrics, compute, warnings := simulateEstimate([]workflowRun{run1, run2}, sampled, config{
		estimateSeed:       7,
		estimateIterations: 20,
		estimateConfidence: 90,
		resourceMapFile:    "resources.json",
		resourceRules: []resourceRule{{
			Name: "large", Match: resourceMatch{Labels: []string{"large"}}, Target: resourceTarget{Platform: "linux", Shape: "medium", VCPUs: 4},
		}},
	})
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v", warnings)
	}
	if metrics.JobsAnalyzed.Median != 2 {
		t.Fatalf("jobs median = %v, want 2", metrics.JobsAnalyzed.Median)
	}
	if compute == nil {
		t.Fatal("compute is nil")
	}
	if compute.Coverage.RuntimePercent.Median != 100 {
		t.Fatalf("coverage = %#v", compute.Coverage)
	}
	if compute.Overall.VCPUMinutes.Median != 80 || compute.Overall.PeakVCPUs.Median != 8 || compute.Overall.PercentileVCPUs["p95"].Median != 8 {
		t.Fatalf("overall = %#v, want 80 vCPU-min and peak/p95 8", compute.Overall)
	}
	if len(compute.Targets) != 1 || compute.Targets[0].Shape != "medium" || compute.Targets[0].VCPUsPerJob != 4 {
		t.Fatalf("targets = %#v", compute.Targets)
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
		defaultVCPUs:         2,
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
	if rep.Estimate.ComputeProjection == nil || rep.Estimate.ComputeProjection.Overall.VCPUMinutes.Median != 40 || rep.Estimate.ComputeProjection.Overall.PeakVCPUs.Median != 4 {
		t.Fatalf("compute projection = %#v, want 40 vCPU-min and peak 4", rep.Estimate.ComputeProjection)
	}
	warnings := strings.Join(rep.Estimate.Warnings, "\n")
	if !strings.Contains(warnings, "vCPU simulation assumes job durations remain unchanged") || !strings.Contains(warnings, "does not include Buildkite agent boot or dispatch time") {
		t.Fatalf("warnings missing vCPU caveats:\n%s", warnings)
	}
	if strings.Contains(warnings, "Use --resource-map") {
		t.Fatalf("warnings incorrectly claim mapping is absent:\n%s", warnings)
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
