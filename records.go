package main

import (
	"sort"
	"strings"
	"time"
)

type workflowRun struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	WorkflowID   int64  `json:"workflow_id"`
	Event        string `json:"event"`
	HeadBranch   string `json:"head_branch"`
	Status       string `json:"status"`
	Conclusion   string `json:"conclusion"`
	CreatedAt    string `json:"created_at"`
	RunStartedAt string `json:"run_started_at"`
	UpdatedAt    string `json:"updated_at"`
	Repo         string `json:"-"`
	Provider     string `json:"-"`
}

type workflowJob struct {
	Name            string   `json:"name"`
	Conclusion      string   `json:"conclusion"`
	WorkflowName    string   `json:"workflow_name"`
	StartedAt       string   `json:"started_at"`
	CompletedAt     string   `json:"completed_at"`
	CreatedAt       string   `json:"created_at"`
	Labels          []string `json:"labels"`
	RunnerName      string   `json:"runner_name"`
	RunnerGroupName string   `json:"runner_group_name"`
}

type record struct {
	Provider        string
	Repo            string
	WorkflowName    string
	JobName         string
	Conclusion      string
	Start           time.Time
	End             time.Time
	QueueSeconds    *float64
	OS              string
	SelfHosted      bool
	Labels          []string
	RunnerName      string
	RunnerGroupName string
	ResourceClass   string
	Executor        string
	Parallelism     int
}

type collectOptions struct {
	Since               string
	Until               string
	IncludeInProgress   bool
	JobFilter           string
	Branch              string
	Event               string
	ExcludePullRequests bool
	APIWorkers          int
	CircleCIJobDetails  bool
	CircleCIMaxPages    int
}

type repoScanResult struct {
	Repo         string
	Records      []record
	Pipelines    int
	WorkflowRuns int
	WorkflowJobs int
	JobsUsed     int
}

type scanSummary struct {
	RepositoriesQueued    int                 `json:"repositories_queued"`
	RepositoriesScanned   int                 `json:"repositories_scanned"`
	RepositoriesSkipped   int                 `json:"repositories_skipped"`
	SkippedRepositories   []skippedRepository `json:"skipped_repositories,omitempty"`
	Pipelines             int                 `json:"pipelines,omitempty"`
	WorkflowRuns          int                 `json:"workflow_runs"`
	WorkflowJobs          int                 `json:"workflow_jobs"`
	JobsUsed              int                 `json:"jobs_used"`
	Conclusions           map[string]int      `json:"conclusions,omitempty"`
	APIRequests           int                 `json:"api_requests"`
	Retries               int                 `json:"retries"`
	RateLimitSleeps       int                 `json:"rate_limit_sleeps"`
	RateLimitSleepSeconds float64             `json:"rate_limit_sleep_seconds"`
	RuntimeSeconds        float64             `json:"runtime_seconds"`
}

func sortRecords(records []record) {
	sort.Slice(records, func(i, j int) bool {
		if strings.ToLower(records[i].Repo) != strings.ToLower(records[j].Repo) {
			return strings.ToLower(records[i].Repo) < strings.ToLower(records[j].Repo)
		}
		if !records[i].Start.Equal(records[j].Start) {
			return records[i].Start.Before(records[j].Start)
		}
		if !records[i].End.Equal(records[j].End) {
			return records[i].End.Before(records[j].End)
		}
		if records[i].WorkflowName != records[j].WorkflowName {
			return records[i].WorkflowName < records[j].WorkflowName
		}
		return records[i].JobName < records[j].JobName
	})
}
