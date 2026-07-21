package main

import (
	"fmt"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"
)

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
					"runner_id":         101,
					"runner_name":       "GitHub Actions 1",
					"runner_group_id":   1,
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
	if records[0].RunnerID != 101 || records[0].RunnerGroupID != 1 {
		t.Fatalf("GitHub-hosted runner IDs = %d/%d, want 101/1", records[0].RunnerID, records[0].RunnerGroupID)
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

func TestInferOSDoesNotDefaultUnknownLabelsToLinux(t *testing.T) {
	if got := inferOS([]string{"high-memory"}); got != "unknown" {
		t.Fatalf("inferOS = %q, want unknown", got)
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
