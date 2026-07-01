package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

type estimateInterval struct {
	Median float64 `json:"median"`
	Lower  float64 `json:"lower"`
	Upper  float64 `json:"upper"`
}

type estimateMetrics struct {
	JobsAnalyzed          estimateInterval            `json:"jobs_analyzed"`
	BusyHours             estimateInterval            `json:"busy_hours"`
	PeakConcurrency       estimateInterval            `json:"peak_concurrency"`
	PercentileConcurrency map[string]estimateInterval `json:"percentile_concurrency"`
}

type estimateRepositoryLandscape struct {
	Strategy      string                     `json:"strategy"`
	RepoLimit     int                        `json:"repo_limit"`
	RankedRepos   int                        `json:"ranked_repos"`
	SelectedRepos int                        `json:"selected_repos"`
	ProbeComplete bool                       `json:"probe_complete"`
	StopReason    string                     `json:"stop_reason,omitempty"`
	Repositories  []estimateRepositorySignal `json:"repositories"`
}

type estimateRepositorySignal struct {
	Rank                  int     `json:"rank"`
	Repo                  string  `json:"repo"`
	Score                 float64 `json:"score"`
	WorkflowRunCount      int     `json:"workflow_run_count"`
	WorkflowRunCountKnown bool    `json:"workflow_run_count_known"`
	Size                  int     `json:"size"`
	PushedAt              string  `json:"pushed_at,omitempty"`
	UpdatedAt             string  `json:"updated_at,omitempty"`
	OpenIssuesCount       int     `json:"open_issues_count"`
	StargazersCount       int     `json:"stargazers_count"`
	ForksCount            int     `json:"forks_count"`
	DefaultBranch         string  `json:"default_branch,omitempty"`
	Fork                  bool    `json:"fork"`
	Private               bool    `json:"private"`
	Selected              bool    `json:"selected"`
	SelectionReason       string  `json:"selection_reason"`
}

type estimateReport struct {
	Seed                int64                        `json:"seed"`
	Confidence          int                          `json:"confidence"`
	Iterations          int                          `json:"iterations"`
	RequestBudget       int                          `json:"request_budget"`
	MinRemaining        int                          `json:"min_remaining"`
	RepoLimit           int                          `json:"repo_limit"`
	TargetSampleRuns    int                          `json:"target_sample_runs"`
	SampledRuns         int                          `json:"sampled_runs"`
	UnsampledRuns       int                          `json:"unsampled_runs"`
	KnownRuns           int                          `json:"known_runs"`
	EstimatedTotalRuns  int                          `json:"estimated_total_runs"`
	SampleFraction      float64                      `json:"sample_fraction"`
	CensusCompleteness  float64                      `json:"census_completeness"`
	StopReason          string                       `json:"stop_reason,omitempty"`
	Warnings            []string                     `json:"warnings,omitempty"`
	Metrics             estimateMetrics              `json:"metrics"`
	RepositoryLandscape *estimateRepositoryLandscape `json:"repository_landscape,omitempty"`
}

type workflowRunPage struct {
	TotalCount   int           `json:"total_count"`
	WorkflowRuns []workflowRun `json:"workflow_runs"`
}

type sampledRun struct {
	Run     workflowRun
	Records []record
}

type jobShape struct {
	Offset          time.Duration
	Duration        time.Duration
	Provider        string
	WorkflowName    string
	JobName         string
	Conclusion      string
	OS              string
	SelfHosted      bool
	Labels          []string
	RunnerName      string
	RunnerGroupName string
	ResourceClass   string
	Executor        string
	Parallelism     int
}

type runShape struct {
	Key         string
	FallbackKey string
	Jobs        []jobShape
}

type simulationMetric struct {
	JobsAnalyzed int
	BusyHours    float64
	Peak         int
	P50          int
	P90          int
	P95          int
	P99          int
}

func buildEstimateRepositoryLandscape(client *githubClient, repos []string, repoInfos map[string]repositoryInfo, opts collectOptions, cfg config, stderr io.Writer) (*estimateRepositoryLandscape, []string, []string, error) {
	signals := make([]estimateRepositorySignal, 0, len(repos))
	for _, repo := range repos {
		info := repoInfos[repoInfoKey(repo)]
		if info.FullName == "" {
			info.FullName = repo
		}
		signals = append(signals, signalFromRepositoryInfo(info))
	}
	if len(signals) == 0 {
		return &estimateRepositoryLandscape{
			Strategy:      "actions_runs_then_metadata",
			RepoLimit:     cfg.estimateRepoLimit,
			ProbeComplete: true,
		}, nil, nil, nil
	}

	signalIndex := map[string]int{}
	for i, signal := range signals {
		signalIndex[repoInfoKey(signal.Repo)] = i
	}

	probeOrder := append([]estimateRepositorySignal{}, signals...)
	sortRepositorySignalsForProbe(probeOrder)
	probeLimit := estimateLandscapeProbeLimit(len(probeOrder), cfg)

	probeComplete := true
	stopReason := ""
	var warnings []string
	probesAttempted := 0
probeLoop:
	for _, probe := range probeOrder {
		if probesAttempted >= probeLimit {
			probeComplete = false
			stopReason = fmt.Sprintf("repository landscape probe cap reached after %d repositories to preserve estimate request budget", probeLimit)
			break
		}
		probesAttempted++
		idx := signalIndex[repoInfoKey(probe.Repo)]
		if !hasRepositoryMetadata(signals[idx]) {
			info, err := getRepoInfo(client, signals[idx].Repo)
			if err != nil {
				var stopErr requestBudgetStopError
				var nf notFoundError
				var ae authError
				switch {
				case errors.As(err, &stopErr):
					probeComplete = false
					stopReason = stopErr.Reason
					break probeLoop
				case errors.As(err, &nf):
					warning := fmt.Sprintf("%s metadata was unavailable during estimate landscape; ranking uses Actions activity when available.", signals[idx].Repo)
					warnings = append(warnings, warning)
					if stderr != nil {
						fmt.Fprintf(stderr, "warning: %s\n", warning)
					}
				case errors.As(err, &ae):
					return nil, nil, nil, authError{}
				default:
					return nil, nil, nil, err
				}
			} else {
				if info.FullName == "" {
					info.FullName = signals[idx].Repo
				}
				signals[idx] = signalFromRepositoryInfo(info)
			}
		}

		count, err := getWorkflowRunCountForEstimate(client, signals[idx].Repo, opts)
		if err != nil {
			var stopErr requestBudgetStopError
			var nf notFoundError
			var ae authError
			switch {
			case errors.As(err, &stopErr):
				probeComplete = false
				stopReason = stopErr.Reason
				break probeLoop
			case errors.As(err, &nf):
				warning := fmt.Sprintf("%s Actions activity was unavailable during estimate landscape; ranking uses repository metadata only.", signals[idx].Repo)
				warnings = append(warnings, warning)
				if stderr != nil {
					fmt.Fprintf(stderr, "warning: %s\n", warning)
				}
			case errors.As(err, &ae):
				return nil, nil, nil, authError{}
			default:
				return nil, nil, nil, err
			}
			continue
		}
		signals[idx].WorkflowRunCount = count
		signals[idx].WorkflowRunCountKnown = true
	}

	for i := range signals {
		signals[i].Score = roundFloat(repositorySignalScore(signals[i]), 3)
	}
	sortRepositorySignalsByRank(signals)

	limit := cfg.estimateRepoLimit
	selectedRepos := make([]string, 0, len(signals))
	for i := range signals {
		signals[i].Rank = i + 1
		selected := limit == 0 || i < limit
		signals[i].Selected = selected
		switch {
		case selected && limit > 0:
			signals[i].SelectionReason = fmt.Sprintf("selected by --estimate-repo-limit=%d", limit)
		case selected:
			signals[i].SelectionReason = "selected; no repository limit"
		default:
			signals[i].SelectionReason = fmt.Sprintf("outside --estimate-repo-limit=%d", limit)
		}
		if selected {
			selectedRepos = append(selectedRepos, signals[i].Repo)
		}
	}
	if !probeComplete {
		warning := "repository landscape probing stopped before all candidate repositories were probed; ranking uses available Actions counts and repository metadata"
		warnings = append(warnings, warning)
	}

	return &estimateRepositoryLandscape{
		Strategy:      "actions_runs_then_metadata",
		RepoLimit:     cfg.estimateRepoLimit,
		RankedRepos:   len(signals),
		SelectedRepos: len(selectedRepos),
		ProbeComplete: probeComplete,
		StopReason:    stopReason,
		Repositories:  signals,
	}, selectedRepos, warnings, nil
}

func signalFromRepositoryInfo(info repositoryInfo) estimateRepositorySignal {
	return estimateRepositorySignal{
		Repo:            info.FullName,
		Size:            info.Size,
		PushedAt:        info.PushedAt,
		UpdatedAt:       info.UpdatedAt,
		OpenIssuesCount: info.OpenIssuesCount,
		StargazersCount: info.StargazersCount,
		ForksCount:      info.ForksCount,
		DefaultBranch:   info.DefaultBranch,
		Fork:            info.Fork,
		Private:         info.Private,
	}
}

func estimateLandscapeProbeLimit(repoCount int, cfg config) int {
	if repoCount <= 0 {
		return 0
	}
	if cfg.estimateMaxRequests <= 0 {
		return repoCount
	}
	sampleReserve := cfg.estimateSampleRuns
	if sampleReserve < 1 {
		sampleReserve = 1
	}
	censusReserve := sampleReserve
	if cfg.estimateRepoLimit > 0 {
		censusReserve = min(cfg.estimateRepoLimit, repoCount)
	}
	reserve := sampleReserve + max(censusReserve, 1)
	limit := cfg.estimateMaxRequests - reserve
	if limit < 0 {
		limit = 0
	}
	return min(repoCount, limit)
}

func hasRepositoryMetadata(signal estimateRepositorySignal) bool {
	return signal.Size > 0 ||
		signal.PushedAt != "" ||
		signal.UpdatedAt != "" ||
		signal.OpenIssuesCount > 0 ||
		signal.StargazersCount > 0 ||
		signal.ForksCount > 0 ||
		signal.DefaultBranch != ""
}

func getWorkflowRunCountForEstimate(client *githubClient, repo string, opts collectOptions) (int, error) {
	repoPath, err := repoAPIPath(repo)
	if err != nil {
		return 0, err
	}
	params := workflowRunParams(opts)
	params.Set("per_page", "1")
	body, _, err := client.request(client.baseURL + repoPath + "/actions/runs?" + params.Encode())
	if err != nil {
		return 0, err
	}
	var page workflowRunPage
	if err := json.Unmarshal(body, &page); err != nil {
		return 0, err
	}
	if page.TotalCount > 0 {
		return page.TotalCount, nil
	}
	return len(page.WorkflowRuns), nil
}

func sortRepositorySignalsForProbe(signals []estimateRepositorySignal) {
	sort.SliceStable(signals, func(i, j int) bool {
		left := repositoryMetadataScore(signals[i])
		right := repositoryMetadataScore(signals[j])
		if left != right {
			return left > right
		}
		return strings.ToLower(signals[i].Repo) < strings.ToLower(signals[j].Repo)
	})
}

func sortRepositorySignalsByRank(signals []estimateRepositorySignal) {
	sort.SliceStable(signals, func(i, j int) bool {
		if signals[i].Score != signals[j].Score {
			return signals[i].Score > signals[j].Score
		}
		if signals[i].WorkflowRunCountKnown != signals[j].WorkflowRunCountKnown {
			return signals[i].WorkflowRunCountKnown
		}
		if signals[i].WorkflowRunCount != signals[j].WorkflowRunCount {
			return signals[i].WorkflowRunCount > signals[j].WorkflowRunCount
		}
		if signals[i].Size != signals[j].Size {
			return signals[i].Size > signals[j].Size
		}
		return strings.ToLower(signals[i].Repo) < strings.ToLower(signals[j].Repo)
	})
}

func repositorySignalScore(signal estimateRepositorySignal) float64 {
	score := repositoryMetadataScore(signal)
	if signal.WorkflowRunCountKnown {
		score += float64(signal.WorkflowRunCount) * 1_000_000_000
	}
	return score
}

func repositoryMetadataScore(signal estimateRepositorySignal) float64 {
	score := math.Log1p(float64(max(signal.Size, 0))) * 1000
	score += math.Log1p(float64(max(signal.OpenIssuesCount, 0))) * 200
	score += math.Log1p(float64(max(signal.StargazersCount, 0))) * 100
	score += math.Log1p(float64(max(signal.ForksCount, 0))) * 100
	score += repositoryTimestampScore(signal.PushedAt)
	score += repositoryTimestampScore(signal.UpdatedAt) / 2
	return score
}

func repositoryTimestampScore(value string) float64 {
	if value == "" {
		return 0
	}
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return 0
	}
	return float64(t.Unix()) / float64(86400*100)
}

func runEstimate(client *githubClient, cfg config, repoInfos map[string]repositoryInfo, skipped []skippedRepository, started time.Time, stderr io.Writer) (report, error) {
	if cfg.estimateSeed == 0 {
		cfg.estimateSeed = time.Now().UnixNano()
	}
	client.enableRequestBudget(cfg.estimateMaxRequests, cfg.estimateMinRemaining)
	defer client.disableRequestBudget()

	opts := collectOptions{
		Since:               cfg.since,
		Until:               cfg.until,
		IncludeInProgress:   cfg.includeInProgress,
		JobFilter:           cfg.jobFilter,
		Branch:              cfg.branch,
		Event:               cfg.event,
		ExcludePullRequests: cfg.excludePullRequests,
		APIWorkers:          cfg.apiWorkers,
	}
	landscape, selectedRepos, landscapeWarnings, err := buildEstimateRepositoryLandscape(client, cfg.repos, repoInfos, opts, cfg, stderr)
	if err != nil {
		return report{}, err
	}
	cfg.repos = selectedRepos
	runs, estimatedTotalRuns, censusComplete, summary, stopReason, err := collectWorkflowRunCensus(client, cfg.repos, opts, skipped, stderr)
	if err != nil {
		return report{}, err
	}
	if len(runs) == 0 {
		return report{}, errors.New("not enough sampled workflow runs for estimate before API budget/rate-limit stop")
	}
	sampleCandidates := selectSampleRuns(runs, min(cfg.estimateSampleRuns, len(runs)), cfg.estimateSeed)
	sampled, sampleSummary, sampleStopReason, err := collectSampledRunJobs(client, sampleCandidates, opts)
	if err != nil {
		var stopErr requestBudgetStopError
		if !errors.As(err, &stopErr) {
			return report{}, err
		}
	}
	if sampleStopReason != "" {
		stopReason = sampleStopReason
	}
	summary.WorkflowJobs += sampleSummary.WorkflowJobs
	summary.JobsUsed += sampleSummary.JobsUsed
	for key, count := range sampleSummary.Conclusions {
		if summary.Conclusions == nil {
			summary.Conclusions = map[string]int{}
		}
		summary.Conclusions[key] += count
	}

	minSample := minViableSample(len(runs))
	if len(sampled) < minSample {
		return report{}, errors.New("not enough sampled workflow runs for estimate before API budget/rate-limit stop")
	}
	if sampleSummary.JobsUsed == 0 {
		return report{}, errors.New("not enough sampled workflow runs for estimate before API budget/rate-limit stop")
	}

	metrics, warnings := simulateEstimate(runs, sampled, cfg)
	warnings = append(landscapeWarnings, warnings...)
	if stopReason == "" {
		stopReason = client.requestBudgetStopReason()
	}
	if stopReason != "" {
		warnings = append(warnings, stopReason)
	}
	if !censusComplete {
		warnings = append(warnings, "workflow-run census stopped before all target repositories/pages were read")
	}
	warnings = append(warnings, "Peak concurrency is sensitive to rare unsampled fan-out; run exact mode before final commitments.")

	stats := client.statsSnapshot()
	runtimeS := runtimeSeconds(time.Since(started))
	summary.APIRequests = stats.Requests
	summary.Retries = stats.Retries
	summary.RateLimitSleeps = stats.RateLimitSleeps
	summary.RateLimitSleepSeconds = stats.RateLimitSleepSeconds
	summary.RuntimeSeconds = runtimeS
	if len(summary.Conclusions) == 0 {
		summary.Conclusions = nil
	}

	knownRuns := len(runs)
	estimatedTotal := estimatedTotalRuns
	if estimatedTotal < knownRuns {
		estimatedTotal = knownRuns
	}
	estimate := &estimateReport{
		Seed:                cfg.estimateSeed,
		Confidence:          cfg.estimateConfidence,
		Iterations:          cfg.estimateIterations,
		RequestBudget:       cfg.estimateMaxRequests,
		MinRemaining:        cfg.estimateMinRemaining,
		RepoLimit:           cfg.estimateRepoLimit,
		TargetSampleRuns:    cfg.estimateSampleRuns,
		SampledRuns:         len(sampled),
		UnsampledRuns:       max(0, knownRuns-len(sampled)),
		KnownRuns:           knownRuns,
		EstimatedTotalRuns:  estimatedTotal,
		SampleFraction:      roundFloat(float64(len(sampled))/float64(knownRuns), 4),
		CensusCompleteness:  roundFloat(censusCompletenessRatio(knownRuns, estimatedTotal, censusComplete), 4),
		StopReason:          stopReason,
		Warnings:            warnings,
		Metrics:             metrics,
		RepositoryLandscape: landscape,
	}

	rep := report{
		Tool:            "gh-concurrency",
		Version:         version,
		GeneratedAt:     time.Now().UTC().Format(time.RFC3339),
		RuntimeSeconds:  runtimeS,
		Parameters:      buildParameters(cfg),
		Scan:            summary,
		JobsAnalyzed:    int(math.Round(metrics.JobsAnalyzed.Median)),
		BusyHours:       roundFloat(metrics.BusyHours.Median, 2),
		PeakConcurrency: int(math.Round(metrics.PeakConcurrency.Median)),
		PercentileConcurrency: map[string]int{
			"p50": int(math.Round(metrics.PercentileConcurrency["p50"].Median)),
			"p90": int(math.Round(metrics.PercentileConcurrency["p90"].Median)),
			"p95": int(math.Round(metrics.PercentileConcurrency["p95"].Median)),
			"p99": int(math.Round(metrics.PercentileConcurrency["p99"].Median)),
		},
		RunnerPools:             nil,
		BillableMinutesEstimate: nil,
		Warnings:                warnings,
		Estimate:                estimate,
	}
	return rep, nil
}

func collectWorkflowRunCensus(client *githubClient, repos []string, opts collectOptions, skipped []skippedRepository, stderr io.Writer) ([]workflowRun, int, bool, scanSummary, string, error) {
	summary := scanSummary{
		RepositoriesQueued:  len(repos),
		SkippedRepositories: append([]skippedRepository{}, skipped...),
		Conclusions:         map[string]int{},
	}
	var out []workflowRun
	estimatedTotal := 0
	censusComplete := true
	stopReason := ""
	for _, repo := range repos {
		runs, total, complete, err := listWorkflowRunsForEstimate(client, repo, opts)
		if err != nil {
			var stopErr requestBudgetStopError
			var nf notFoundError
			var ae authError
			switch {
			case errors.As(err, &stopErr):
				stopReason = stopErr.Reason
				censusComplete = false
			case errors.As(err, &nf):
				fmt.Fprintf(stderr, "warning: %s not found or no Actions access; skipping.\n", repo)
				summary.SkippedRepositories = append(summary.SkippedRepositories, skippedRepository{Repo: repo, Reason: "not found or no Actions access"})
				continue
			case errors.As(err, &ae):
				return nil, 0, false, summary, stopReason, authError{}
			default:
				return nil, 0, false, summary, stopReason, err
			}
		}
		for _, run := range runs {
			conclusion := run.Conclusion
			if conclusion == "" {
				conclusion = "unknown"
			}
			summary.Conclusions[conclusion]++
		}
		summary.RepositoriesScanned++
		summary.WorkflowRuns += len(runs)
		out = append(out, runs...)
		if total > 0 {
			estimatedTotal += total
		} else {
			estimatedTotal += len(runs)
		}
		if !complete {
			censusComplete = false
			break
		}
		if stopReason != "" {
			break
		}
	}
	sortWorkflowRuns(out)
	sortSkippedRepositories(summary.SkippedRepositories)
	summary.RepositoriesSkipped = len(summary.SkippedRepositories)
	if len(summary.Conclusions) == 0 {
		summary.Conclusions = nil
	}
	return out, estimatedTotal, censusComplete, summary, stopReason, nil
}

func listWorkflowRunsForEstimate(client *githubClient, repo string, opts collectOptions) ([]workflowRun, int, bool, error) {
	repoPath, err := repoAPIPath(repo)
	if err != nil {
		return nil, 0, false, err
	}
	params := workflowRunParams(opts)
	params.Set("per_page", "100")
	nextURL := client.baseURL + repoPath + "/actions/runs?" + params.Encode()
	total := 0
	complete := true
	var runs []workflowRun
	for nextURL != "" {
		body, link, err := client.request(nextURL)
		if err != nil {
			var stopErr requestBudgetStopError
			if errors.As(err, &stopErr) {
				return runs, total, false, err
			}
			return runs, total, false, err
		}
		var page workflowRunPage
		if err := json.Unmarshal(body, &page); err != nil {
			return runs, total, false, err
		}
		if page.TotalCount > total {
			total = page.TotalCount
		}
		for _, run := range page.WorkflowRuns {
			run.Repo = repo
			run.Provider = githubProvider
			runs = append(runs, run)
		}
		nextURL = nextLink(link)
	}
	return runs, total, complete, nil
}

func workflowRunParams(opts collectOptions) url.Values {
	params := url.Values{"created": []string{createdQuery(opts.Since, opts.Until)}}
	if !opts.IncludeInProgress {
		params.Set("status", "completed")
	}
	if opts.Branch != "" {
		params.Set("branch", opts.Branch)
	}
	if opts.Event != "" {
		params.Set("event", opts.Event)
	}
	if opts.ExcludePullRequests {
		params.Set("exclude_pull_requests", "true")
	}
	return params
}

func sortWorkflowRuns(runs []workflowRun) {
	sort.Slice(runs, func(i, j int) bool {
		if strings.ToLower(runs[i].Repo) != strings.ToLower(runs[j].Repo) {
			return strings.ToLower(runs[i].Repo) < strings.ToLower(runs[j].Repo)
		}
		if !runAnchor(runs[i]).Equal(runAnchor(runs[j])) {
			return runAnchor(runs[i]).Before(runAnchor(runs[j]))
		}
		return runs[i].ID < runs[j].ID
	})
}

func selectSampleRuns(runs []workflowRun, target int, seed int64) []workflowRun {
	if target >= len(runs) {
		return append([]workflowRun{}, runs...)
	}
	if target < 1 {
		target = 1
	}
	groups := map[string][]workflowRun{}
	for _, run := range runs {
		groups[runStratumKey(run)] = append(groups[runStratumKey(run)], run)
	}
	keys := sortedStringKeys(groups)
	rng := newDeterministicRand(seed)
	var sample []workflowRun
	var largeKeys []string
	for _, key := range keys {
		group := append([]workflowRun{}, groups[key]...)
		sortWorkflowRuns(group)
		if len(group) <= 2 && len(sample)+len(group) <= target {
			sample = append(sample, group...)
			continue
		}
		largeKeys = append(largeKeys, key)
	}
	for _, key := range largeKeys {
		group := append([]workflowRun{}, groups[key]...)
		shuffleWorkflowRuns(group, rng)
		groups[key] = group
	}
	for len(sample) < target && len(largeKeys) > 0 {
		progress := false
		for _, key := range largeKeys {
			group := groups[key]
			if len(group) == 0 {
				continue
			}
			sample = append(sample, group[0])
			groups[key] = group[1:]
			progress = true
			if len(sample) == target {
				break
			}
		}
		if !progress {
			break
		}
	}
	sortWorkflowRuns(sample)
	return sample
}

func shuffleWorkflowRuns(runs []workflowRun, rng *deterministicRand) {
	rng.Shuffle(len(runs), func(i, j int) {
		runs[i], runs[j] = runs[j], runs[i]
	})
}

func collectSampledRunJobs(client *githubClient, runs []workflowRun, opts collectOptions) ([]sampledRun, scanSummary, string, error) {
	summary := scanSummary{Conclusions: map[string]int{}}
	var sampled []sampledRun
	stopReason := ""
	for _, run := range runs {
		repoPath, err := repoAPIPath(run.Repo)
		if err != nil {
			return sampled, summary, stopReason, err
		}
		result, err := collectRunJobs(client, run.Repo, repoPath, run.ID, opts.JobFilter)
		if err != nil {
			var stopErr requestBudgetStopError
			if errors.As(err, &stopErr) {
				stopReason = stopErr.Reason
				return sampled, summary, stopReason, err
			}
			return sampled, summary, stopReason, err
		}
		summary.WorkflowJobs += result.WorkflowJobs
		summary.JobsUsed += result.JobsUsed
		for _, rec := range result.Records {
			conclusion := rec.Conclusion
			if conclusion == "" {
				conclusion = "unknown"
			}
			summary.Conclusions[conclusion]++
		}
		sampled = append(sampled, sampledRun{Run: run, Records: result.Records})
	}
	if len(summary.Conclusions) == 0 {
		summary.Conclusions = nil
	}
	return sampled, summary, stopReason, nil
}

func simulateEstimate(runs []workflowRun, sampled []sampledRun, cfg config) (estimateMetrics, []string) {
	sampledIDs := map[int64]bool{}
	var fixedRecords []record
	var shapes []runShape
	for _, sample := range sampled {
		sampledIDs[sample.Run.ID] = true
		fixedRecords = append(fixedRecords, sample.Records...)
		shapes = append(shapes, buildRunShape(sample.Run, sample.Records))
	}
	var unsampled []workflowRun
	for _, run := range runs {
		if !sampledIDs[run.ID] {
			unsampled = append(unsampled, run)
		}
	}

	byKey := map[string][]runShape{}
	byFallback := map[string][]runShape{}
	for _, shape := range shapes {
		byKey[shape.Key] = append(byKey[shape.Key], shape)
		byFallback[shape.FallbackKey] = append(byFallback[shape.FallbackKey], shape)
	}
	iterations := cfg.estimateIterations
	if iterations < 1 {
		iterations = 1
	}
	values := make([]simulationMetric, 0, iterations)
	warnings := []string{}
	globalShapes := shapes
	if len(globalShapes) == 0 {
		return emptyEstimateMetrics(), []string{"No sampled workflow runs had usable completed jobs."}
	}
	for i := 0; i < iterations; i++ {
		rng := newDeterministicRand(cfg.estimateSeed + int64(i+1))
		records := append([]record{}, fixedRecords...)
		for _, run := range unsampled {
			anchor := runAnchor(run)
			if anchor.IsZero() {
				continue
			}
			shape := drawRunShape(run, byKey, byFallback, globalShapes, rng)
			records = append(records, applyRunShape(run, anchor, shape)...)
		}
		values = append(values, metricForRecords(records))
	}
	return intervalsForMetrics(values, cfg.estimateConfidence), warnings
}

func buildRunShape(run workflowRun, records []record) runShape {
	anchor := runAnchor(run)
	shape := runShape{Key: runStratumKey(run), FallbackKey: runFallbackKey(run)}
	for _, rec := range records {
		offset := time.Duration(0)
		if !anchor.IsZero() {
			offset = rec.Start.Sub(anchor)
		}
		shape.Jobs = append(shape.Jobs, jobShape{
			Offset:          offset,
			Duration:        rec.End.Sub(rec.Start),
			Provider:        rec.Provider,
			WorkflowName:    rec.WorkflowName,
			JobName:         rec.JobName,
			Conclusion:      rec.Conclusion,
			OS:              rec.OS,
			SelfHosted:      rec.SelfHosted,
			Labels:          append([]string{}, rec.Labels...),
			RunnerName:      rec.RunnerName,
			RunnerGroupName: rec.RunnerGroupName,
			ResourceClass:   rec.ResourceClass,
			Executor:        rec.Executor,
			Parallelism:     rec.Parallelism,
		})
	}
	return shape
}

func drawRunShape(run workflowRun, byKey, byFallback map[string][]runShape, global []runShape, rng *deterministicRand) runShape {
	if shapes := byKey[runStratumKey(run)]; len(shapes) > 0 {
		return shapes[rng.Intn(len(shapes))]
	}
	if shapes := byFallback[runFallbackKey(run)]; len(shapes) > 0 {
		return shapes[rng.Intn(len(shapes))]
	}
	return global[rng.Intn(len(global))]
}

func applyRunShape(run workflowRun, anchor time.Time, shape runShape) []record {
	var records []record
	for _, job := range shape.Jobs {
		start := anchor.Add(job.Offset)
		end := start.Add(job.Duration)
		if !end.After(start) {
			continue
		}
		records = append(records, record{
			Provider:        firstNonEmpty(run.Provider, job.Provider, githubProvider),
			Repo:            run.Repo,
			WorkflowName:    firstNonEmpty(run.Name, job.WorkflowName),
			JobName:         job.JobName,
			Conclusion:      firstNonEmpty(run.Conclusion, job.Conclusion),
			Start:           start,
			End:             end,
			OS:              job.OS,
			SelfHosted:      job.SelfHosted,
			Labels:          append([]string{}, job.Labels...),
			RunnerName:      job.RunnerName,
			RunnerGroupName: job.RunnerGroupName,
			ResourceClass:   job.ResourceClass,
			Executor:        job.Executor,
			Parallelism:     job.Parallelism,
		})
	}
	return records
}

func metricForRecords(records []record) simulationMetric {
	intervals := make([][2]time.Time, 0, len(records))
	for _, rec := range records {
		intervals = append(intervals, [2]time.Time{rec.Start, rec.End})
	}
	peak, profile := concurrencyProfile(intervals)
	pct := percentiles(profile, []int{50, 90, 95, 99})
	busySeconds := 0.0
	for _, seconds := range profile {
		busySeconds += seconds
	}
	return simulationMetric{
		JobsAnalyzed: len(records),
		BusyHours:    math.Round((busySeconds/3600.0)*100) / 100,
		Peak:         peak,
		P50:          pct[50],
		P90:          pct[90],
		P95:          pct[95],
		P99:          pct[99],
	}
}

func intervalsForMetrics(values []simulationMetric, confidence int) estimateMetrics {
	if len(values) == 0 {
		return emptyEstimateMetrics()
	}
	return estimateMetrics{
		JobsAnalyzed:    intervalFromValues(values, confidence, func(v simulationMetric) float64 { return float64(v.JobsAnalyzed) }),
		BusyHours:       intervalFromValues(values, confidence, func(v simulationMetric) float64 { return v.BusyHours }),
		PeakConcurrency: intervalFromValues(values, confidence, func(v simulationMetric) float64 { return float64(v.Peak) }),
		PercentileConcurrency: map[string]estimateInterval{
			"p50": intervalFromValues(values, confidence, func(v simulationMetric) float64 { return float64(v.P50) }),
			"p90": intervalFromValues(values, confidence, func(v simulationMetric) float64 { return float64(v.P90) }),
			"p95": intervalFromValues(values, confidence, func(v simulationMetric) float64 { return float64(v.P95) }),
			"p99": intervalFromValues(values, confidence, func(v simulationMetric) float64 { return float64(v.P99) }),
		},
	}
}

func emptyEstimateMetrics() estimateMetrics {
	zero := estimateInterval{}
	return estimateMetrics{
		JobsAnalyzed:          zero,
		BusyHours:             zero,
		PeakConcurrency:       zero,
		PercentileConcurrency: map[string]estimateInterval{"p50": zero, "p90": zero, "p95": zero, "p99": zero},
	}
}

func intervalFromValues(values []simulationMetric, confidence int, valueFor func(simulationMetric) float64) estimateInterval {
	vals := make([]float64, 0, len(values))
	for _, value := range values {
		vals = append(vals, valueFor(value))
	}
	sort.Float64s(vals)
	alpha := float64(100-confidence) / 2.0
	lower := percentileFloat(vals, alpha)
	upper := percentileFloat(vals, 100-alpha)
	median := percentileFloat(vals, 50)
	return estimateInterval{
		Median: roundFloat(median, 2),
		Lower:  roundFloat(lower, 2),
		Upper:  roundFloat(upper, 2),
	}
}

func percentileFloat(values []float64, pct float64) float64 {
	if len(values) == 0 {
		return 0
	}
	if pct <= 0 {
		return values[0]
	}
	if pct >= 100 {
		return values[len(values)-1]
	}
	idx := int(math.Ceil((pct/100.0)*float64(len(values)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(values) {
		idx = len(values) - 1
	}
	return values[idx]
}

func runAnchor(run workflowRun) time.Time {
	for _, value := range []string{run.RunStartedAt, run.CreatedAt, run.UpdatedAt} {
		if value == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, value)
		if err == nil {
			return t
		}
	}
	return time.Time{}
}

func runStratumKey(run workflowRun) string {
	workflow := run.Name
	if run.WorkflowID != 0 {
		workflow = strconv.FormatInt(run.WorkflowID, 10)
	}
	if workflow == "" {
		workflow = "unknown-workflow"
	}
	event := run.Event
	if event == "" {
		event = "unknown-event"
	}
	return strings.ToLower(run.Repo + "|" + workflow + "|" + event)
}

func runFallbackKey(run workflowRun) string {
	event := run.Event
	if event == "" {
		event = "unknown-event"
	}
	return strings.ToLower(run.Repo + "|" + event)
}

func minViableSample(knownRuns int) int {
	if knownRuns <= 0 {
		return 0
	}
	tenPercent := int(math.Ceil(float64(knownRuns) * 0.10))
	if tenPercent < 1 {
		tenPercent = 1
	}
	return min(30, tenPercent)
}

func censusCompletenessRatio(known, estimatedTotal int, complete bool) float64 {
	if complete {
		return 1
	}
	if estimatedTotal <= 0 {
		return 0
	}
	return float64(known) / float64(estimatedTotal)
}
