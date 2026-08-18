package main

import (
	"net/http"
	"testing"
	"time"
)

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
	projection := buildComputeProjection(result.Records, config{
		resourceMapFile: "resources.json",
		resourceRules: []resourceRule{{
			Name:   "CircleCI large",
			Match:  resourceMatch{Provider: circleCIProvider, ResourceClass: "large"},
			Target: resourceTarget{Platform: "linux", Shape: "medium", VCPUs: 4},
		}},
	})
	if projection == nil || projection.Coverage.RuntimePercent != 100 {
		t.Fatalf("projection = %#v", projection)
	}
	if projection.Overall.VCPUMinutes != 80 || projection.Overall.PeakVCPUs != 8 {
		t.Fatalf("CircleCI compute = %#v, want 80 vCPU-min and peak 8", projection.Overall)
	}
}
