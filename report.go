package main

import (
	"fmt"
	"math"
	"strings"
	"time"
)

type parameters struct {
	Provider             string   `json:"provider"`
	Repos                []string `json:"repos"`
	Orgs                 []string `json:"orgs,omitempty"`
	RepoFiles            []string `json:"repo_files,omitempty"`
	OrgFiles             []string `json:"org_files,omitempty"`
	CircleCIProjects     []string `json:"circleci_projects,omitempty"`
	CircleCIProjectFiles []string `json:"circleci_project_files,omitempty"`
	CircleCIVCS          string   `json:"circleci_vcs,omitempty"`
	CircleCIJobDetails   bool     `json:"circleci_job_details,omitempty"`
	CircleCIMaxPages     int      `json:"circleci_max_pages,omitempty"`
	RepoType             string   `json:"repo_type,omitempty"`
	IncludeArchived      bool     `json:"include_archived"`
	RepositoryCount      int      `json:"repository_count"`
	Since                string   `json:"since"`
	Until                string   `json:"until,omitempty"`
	BaseURL              string   `json:"base_url"`
	APIWorkers           int      `json:"api_workers"`
	RunStatus            string   `json:"run_status"`
	JobFilter            string   `json:"job_filter"`
	Branch               string   `json:"branch,omitempty"`
	Event                string   `json:"event,omitempty"`
	ExcludePullRequests  bool     `json:"exclude_pull_requests"`
	RunnerInventory      bool     `json:"runner_inventory"`
	Top                  int      `json:"top"`
	ResourceMapFile      string   `json:"resource_map_file,omitempty"`
	DefaultVCPUs         int      `json:"default_vcpus,omitempty"`
	Mode                 string   `json:"mode"`
	EstimateMaxRequests  int      `json:"estimate_max_requests,omitempty"`
	EstimateMinRemaining int      `json:"estimate_min_remaining,omitempty"`
	EstimateSampleRuns   int      `json:"estimate_sample_runs,omitempty"`
	EstimateIterations   int      `json:"estimate_iterations,omitempty"`
	EstimateConfidence   int      `json:"estimate_confidence,omitempty"`
	EstimateSeed         int64    `json:"estimate_seed,omitempty"`
	EstimateRepoLimit    int      `json:"estimate_repo_limit,omitempty"`
}

type report struct {
	Tool                    string                  `json:"tool"`
	Version                 string                  `json:"version"`
	GeneratedAt             string                  `json:"generated_at"`
	RuntimeSeconds          float64                 `json:"runtime_seconds"`
	Parameters              parameters              `json:"parameters"`
	Scan                    scanSummary             `json:"scan"`
	JobsAnalyzed            int                     `json:"jobs_analyzed"`
	BusyHours               float64                 `json:"busy_hours"` // Deprecated: use ActiveWindowHours.
	ActiveWindowHours       float64                 `json:"active_window_hours"`
	JobRuntimeMinutes       float64                 `json:"job_runtime_minutes"`
	PeakConcurrency         int                     `json:"peak_concurrency"`
	PercentileConcurrency   map[string]int          `json:"percentile_concurrency"`
	RunnerPools             []runnerPool            `json:"runner_pools"`
	TopRepositories         []usageSummary          `json:"top_repositories,omitempty"`
	TopWorkflows            []usageSummary          `json:"top_workflows,omitempty"`
	TopJobs                 []usageSummary          `json:"top_jobs,omitempty"`
	BillableMinutesEstimate map[string]billableSlot `json:"billable_minutes_estimate"`
	ComputeProjection       *computeProjection      `json:"compute_projection,omitempty"`
	QueueSeconds            *queueStats             `json:"queue_seconds"`
	Warnings                []string                `json:"warnings"`
	Estimate                *estimateReport         `json:"estimate,omitempty"`
}

func buildReport(records []record, cfg config, runtime time.Duration, summary scanSummary, stats requestStats) report {
	intervals := make([][2]time.Time, 0, len(records))
	for _, rec := range records {
		intervals = append(intervals, [2]time.Time{rec.Start, rec.End})
	}
	peak, profile := concurrencyProfile(intervals)
	pct := percentiles(profile, []int{50, 90, 95, 99})
	qstats := computeQueueStats(records)
	busySeconds := 0.0
	for _, seconds := range profile {
		busySeconds += seconds
	}
	runtimeS := runtimeSeconds(runtime)
	summary.APIRequests = stats.Requests
	summary.Retries = stats.Retries
	summary.RateLimitSleeps = stats.RateLimitSleeps
	summary.RateLimitSleepSeconds = stats.RateLimitSleepSeconds
	summary.RuntimeSeconds = runtimeS
	params := buildParameters(cfg)
	compute := buildComputeProjection(records, cfg)
	billable := billableMinutes(records)
	if params.Provider == circleCIProvider {
		billable = nil
	}
	warnings := detectWarnings(params.Provider, peak, pct, qstats)
	if params.Provider == circleCIProvider && !cfg.circleCIJobDetails {
		warnings = append(warnings, "CircleCI job details were disabled, so resource classes, queue time, and parallelism may be underreported.")
	}
	if params.Provider == circleCIProvider && cfg.circleCIMaxPages > 0 {
		warnings = append(warnings, fmt.Sprintf("CircleCI pipeline scanning was capped at %d pages per project; older matching pipelines may be undercounted.", cfg.circleCIMaxPages))
	}
	if compute == nil {
		warnings = append(warnings, "Concurrency metrics count running job slots, not vCPUs. Use --resource-map or --default-vcpus before using them to estimate Buildkite Hosted Agent vCPU capacity or usage.")
	} else {
		warnings = append(warnings, "The vCPU projection assumes job durations remain unchanged on the target Buildkite agents; validate target shapes with representative workloads.")
		warnings = append(warnings, "The vCPU capacity projection uses observed job execution intervals and does not include Buildkite agent boot or dispatch time.")
		if compute.Coverage.UnmappedJobs > 0 {
			warnings = append(warnings, fmt.Sprintf("The vCPU projection covers %.1f%% of job runtime. Projected vCPU-minutes and peak are mapped-workload minima; percentiles describe mapped-active time only.", compute.Coverage.RuntimePercent))
		}
	}
	if len(billable) > 0 {
		warnings = append(warnings, "The GitHub minute estimate uses standard OS entitlement multipliers only; it does not model larger-runner SKUs or vCPU and is not a Buildkite vCPU-minute estimate.")
	}
	activeWindowHours := roundedHours(busySeconds)
	return report{
		Tool:                    "gh-concurrency",
		Version:                 version,
		GeneratedAt:             time.Now().UTC().Format(time.RFC3339),
		RuntimeSeconds:          runtimeS,
		Parameters:              params,
		Scan:                    summary,
		JobsAnalyzed:            len(records),
		BusyHours:               activeWindowHours,
		ActiveWindowHours:       activeWindowHours,
		JobRuntimeMinutes:       math.Round((totalJobRuntimeSeconds(records)/60.0)*100) / 100,
		PeakConcurrency:         peak,
		PercentileConcurrency:   map[string]int{"p50": pct[50], "p90": pct[90], "p95": pct[95], "p99": pct[99]},
		RunnerPools:             runnerPools(records),
		TopRepositories:         topUsageSummaries(records, cfg.top, func(rec record) string { return rec.Repo }),
		TopWorkflows:            topUsageSummaries(records, cfg.top, workflowSummaryName),
		TopJobs:                 topUsageSummaries(records, cfg.top, jobSummaryName),
		BillableMinutesEstimate: billable,
		ComputeProjection:       compute,
		QueueSeconds:            qstats,
		Warnings:                warnings,
	}
}

func buildParameters(cfg config) parameters {
	provider := cfg.provider
	if provider == "" {
		provider = githubProvider
	}
	repositoryCount := len(cfg.repos)
	if provider == circleCIProvider {
		repositoryCount = len(cfg.circleCIProjects)
	}
	params := parameters{
		Provider:            provider,
		Repos:               cfg.repos,
		Orgs:                cfg.orgs,
		RepoFiles:           cfg.repoFiles,
		OrgFiles:            cfg.orgFiles,
		RepoType:            cfg.repoType,
		IncludeArchived:     cfg.includeArchived,
		RepositoryCount:     repositoryCount,
		Since:               cfg.since,
		Until:               cfg.until,
		BaseURL:             cfg.baseURL,
		APIWorkers:          cfg.apiWorkers,
		RunStatus:           runStatus(cfg),
		JobFilter:           cfg.jobFilter,
		Branch:              cfg.branch,
		Event:               cfg.event,
		ExcludePullRequests: cfg.excludePullRequests,
		RunnerInventory:     cfg.runnerInventory,
		Top:                 cfg.top,
		ResourceMapFile:     cfg.resourceMapFile,
		DefaultVCPUs:        cfg.defaultVCPUs,
		Mode:                modeName(cfg),
	}
	if provider == circleCIProvider {
		params.CircleCIProjects = cfg.circleCIProjects
		params.CircleCIProjectFiles = cfg.circleCIProjectFiles
		params.CircleCIVCS = cfg.circleCIVCS
		params.CircleCIJobDetails = cfg.circleCIJobDetails
		params.CircleCIMaxPages = cfg.circleCIMaxPages
	}
	if cfg.estimate {
		params.EstimateMaxRequests = cfg.estimateMaxRequests
		params.EstimateMinRemaining = cfg.estimateMinRemaining
		params.EstimateSampleRuns = cfg.estimateSampleRuns
		params.EstimateIterations = cfg.estimateIterations
		params.EstimateConfidence = cfg.estimateConfidence
		params.EstimateSeed = cfg.estimateSeed
		params.EstimateRepoLimit = cfg.estimateRepoLimit
	}
	return params
}

func modeName(cfg config) string {
	if cfg.estimate {
		return "estimate"
	}
	return "exact"
}

func runStatus(cfg config) string {
	if cfg.includeInProgress {
		return "all"
	}
	return "completed"
}

func runtimeSeconds(runtime time.Duration) float64 {
	if runtime < 0 {
		runtime = 0
	}
	return math.Round(runtime.Seconds()*1000) / 1000
}

func formatRunDuration(runtimeSeconds float64) string {
	if runtimeSeconds < 0 {
		runtimeSeconds = 0
	}
	duration := time.Duration(runtimeSeconds * float64(time.Second)).Round(100 * time.Millisecond)
	return duration.String()
}

func displayVersion(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "dev"
	}
	if strings.HasPrefix(value, "v") || value == "dev" {
		return value
	}
	return "v" + value
}
