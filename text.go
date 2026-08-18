package main

import (
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"
)

func printText(out io.Writer, rep report) {
	p := rep.Parameters
	provider := p.Provider
	if provider == "" {
		provider = githubProvider
	}
	until := p.Until
	if until == "" {
		until = "now"
	}
	fmt.Fprintf(out, "\ngh-concurrency %s\n", displayVersion(rep.Version))
	if provider == githubProvider && len(p.Orgs) > 0 {
		fmt.Fprintf(out, "orgs:   %s\n", strings.Join(p.Orgs, ", "))
	}
	if len(p.RepoFiles) > 0 {
		fmt.Fprintf(out, "repo files: %s\n", strings.Join(p.RepoFiles, ", "))
	}
	if provider == githubProvider && len(p.OrgFiles) > 0 {
		fmt.Fprintf(out, "org files:  %s\n", strings.Join(p.OrgFiles, ", "))
	}
	if provider == circleCIProvider && len(p.CircleCIProjectFiles) > 0 {
		fmt.Fprintf(out, "project files: %s\n", strings.Join(p.CircleCIProjectFiles, ", "))
	}
	if provider == githubProvider && p.IncludeArchived {
		fmt.Fprintln(out, "archived repos: included")
	}
	if provider == githubProvider && p.RunnerInventory {
		fmt.Fprintln(out, "runner inventory: enabled")
	}
	targetLabel := "repos"
	countLabel := "repo count"
	if provider == circleCIProvider {
		targetLabel = "projects"
		countLabel = "project count"
	}
	fmt.Fprintf(out, "%s:  %s\n", targetLabel, summarizeRepos(p.Repos))
	fmt.Fprintf(out, "%s: %d\n", countLabel, p.RepositoryCount)
	fmt.Fprintf(out, "window: %s -> %s   api: %s\n", p.Since, until, p.BaseURL)
	fmt.Fprintf(out, "filters: runs=%s jobs=%s workers=%d", p.RunStatus, p.JobFilter, p.APIWorkers)
	if p.Branch != "" {
		fmt.Fprintf(out, " branch=%s", p.Branch)
	}
	if p.Event != "" {
		fmt.Fprintf(out, " event=%s", p.Event)
	}
	if p.ExcludePullRequests {
		fmt.Fprint(out, " exclude_prs=true")
	}
	fmt.Fprintln(out)
	if rep.Estimate != nil {
		printEstimateText(out, rep)
		return
	}
	fmt.Fprintf(out, "\nJobs analyzed:        %d\n", rep.JobsAnalyzed)
	fmt.Fprintf(out, "Run time:             %s\n", formatRunDuration(rep.RuntimeSeconds))
	fmt.Fprintf(out, "Total job runtime:    %.2f job-min\n", rep.JobRuntimeMinutes)
	fmt.Fprintf(out, "Active window time:   %.2fh (>=1 job running)\n", rep.ActiveWindowHours)
	fmt.Fprintf(out, "Peak job concurrency: %d jobs\n", rep.PeakConcurrency)
	for _, key := range []string{"p50", "p90", "p95", "p99"} {
		fmt.Fprintf(out, "%s job concurrency:   %d jobs\n", key, rep.PercentileConcurrency[key])
	}

	printScanSummaryForProvider(out, rep.Scan, provider)

	if len(rep.RunnerPools) > 0 {
		fmt.Fprintln(out, "\nRunner pools:")
		for _, pool := range rep.RunnerPools {
			hardware := runnerPoolHardwareSummary(pool)
			fmt.Fprintf(
				out,
				"  %-42s peak %4d jobs  p95 %4d jobs  %8s total%s\n",
				pool.Name,
				pool.PeakConcurrency,
				pool.PercentileConcurrency["p95"],
				comma(pool.Jobs),
				hardware,
			)
		}
	}

	printUsageSummaries(out, "Top repositories by total job runtime:", rep.TopRepositories)
	printUsageSummaries(out, "Top workflows by total job runtime:", rep.TopWorkflows)
	printUsageSummaries(out, "Top jobs by total job runtime:", rep.TopJobs)

	if len(rep.BillableMinutesEstimate) > 0 {
		fmt.Fprintln(out, "\nGitHub OS-multiplied minute estimate (standard-runner model):")
		total := 0
		for _, osName := range sortedStringKeys(rep.BillableMinutesEstimate) {
			slot := rep.BillableMinutesEstimate[osName]
			total += slot.BillableMinutes
			fmt.Fprintf(out, "  %-8s %6d jobs  %10s billable min\n", osName, slot.Jobs, comma(slot.BillableMinutes))
		}
		fmt.Fprintf(out, "  %-8s %6s       %10s billable min\n", "TOTAL", "", comma(total))
		fmt.Fprintln(out, "  Scope: rounds each GitHub-hosted job to a minute and applies Linux x1,")
		fmt.Fprintln(out, "  Windows x2, and macOS x10. Larger-runner SKUs and vCPU are not modeled;")
		fmt.Fprintln(out, "  self-hosted jobs are excluded.")
	}

	if rep.QueueSeconds != nil {
		q := rep.QueueSeconds
		fmt.Fprintf(out, "\nQueue time: median %.0fs  p95 %.0fs  max %.0fs\n", q.MedianS, q.P95S, q.MaxS)
	}

	for _, warning := range rep.Warnings {
		fmt.Fprintf(out, "\nWARNING: %s\n", warning)
	}
}

func runnerPoolHardwareSummary(pool runnerPool) string {
	if !pool.GitHubHosted {
		return ""
	}
	var details []string
	if pool.RunnerType != "" {
		details = append(details, pool.RunnerType)
	}
	if pool.CPUCores > 0 && pool.MemoryGB > 0 {
		details = append(details, fmt.Sprintf("%d vCPU, %d GB RAM", pool.CPUCores, pool.MemoryGB))
		if pool.StorageGB > 0 {
			details = append(details, fmt.Sprintf("%d GB SSD", pool.StorageGB))
		}
	} else {
		details = append(details, "hardware unknown")
	}
	if pool.Architecture != "" && pool.Architecture != "unknown" {
		details = append(details, pool.Architecture)
	}
	return "  [" + strings.Join(details, "; ") + "]"
}

func printEstimateText(out io.Writer, rep report) {
	est := rep.Estimate
	fmt.Fprintf(out, "\nESTIMATE MODE: sampled %s of %s known workflow runs; %d%% simulation interval; seed %d\n",
		comma(est.SampledRuns), comma(est.KnownRuns), est.Confidence, est.Seed)
	if est.CensusCompleteness < 1 {
		fmt.Fprintf(out, "Census completeness: %.1f%% of estimated workflow runs\n", est.CensusCompleteness*100)
	}
	if est.StopReason != "" {
		fmt.Fprintf(out, "Stopped early: %s\n", est.StopReason)
	}
	printEstimateLandscapeText(out, est.RepositoryLandscape, rep.Parameters.Top)
	fmt.Fprintf(out, "\nJobs analyzed:        median %s (%d%% range %s-%s)\n",
		formatEstimateNumber(est.Metrics.JobsAnalyzed.Median),
		est.Confidence,
		formatEstimateNumber(est.Metrics.JobsAnalyzed.Lower),
		formatEstimateNumber(est.Metrics.JobsAnalyzed.Upper))
	fmt.Fprintf(out, "Run time:             %s\n", formatRunDuration(rep.RuntimeSeconds))
	fmt.Fprintf(out, "Total job runtime:    median %.2f job-min (%d%% range %.2f-%.2f)\n",
		est.Metrics.JobRuntimeMinutes.Median, est.Confidence, est.Metrics.JobRuntimeMinutes.Lower, est.Metrics.JobRuntimeMinutes.Upper)
	fmt.Fprintf(out, "Active window time:   median %.2fh (%d%% range %.2f-%.2fh)\n",
		est.Metrics.ActiveWindowHours.Median, est.Confidence, est.Metrics.ActiveWindowHours.Lower, est.Metrics.ActiveWindowHours.Upper)
	fmt.Fprintf(out, "Peak job concurrency: median %s jobs (%d%% range %s-%s)\n",
		formatEstimateNumber(est.Metrics.PeakConcurrency.Median),
		est.Confidence,
		formatEstimateNumber(est.Metrics.PeakConcurrency.Lower),
		formatEstimateNumber(est.Metrics.PeakConcurrency.Upper))
	for _, key := range []string{"p50", "p90", "p95", "p99"} {
		interval := est.Metrics.PercentileConcurrency[key]
		fmt.Fprintf(out, "%s job concurrency:   median %s jobs (%d%% range %s-%s)\n",
			key,
			formatEstimateNumber(interval.Median),
			est.Confidence,
			formatEstimateNumber(interval.Lower),
			formatEstimateNumber(interval.Upper))
	}
	printScanSummaryForProvider(out, rep.Scan, rep.Parameters.Provider)
	fmt.Fprintln(out, "\nEstimate notes:")
	fmt.Fprintln(out, "  These are sampled simulation intervals, not billing-grade exact measurements.")
	for _, warning := range est.Warnings {
		fmt.Fprintf(out, "  WARNING: %s\n", warning)
	}
}

func printEstimateLandscapeText(out io.Writer, landscape *estimateRepositoryLandscape, top int) {
	if landscape == nil || len(landscape.Repositories) == 0 {
		return
	}
	limit := "all"
	if landscape.RepoLimit > 0 {
		limit = strconv.Itoa(landscape.RepoLimit)
	}
	status := "complete"
	if !landscape.ProbeComplete {
		status = "partial"
	}
	fmt.Fprintf(out, "\nRepository landscape: ranked %d repos, selected %d, limit %s, probes %s\n",
		landscape.RankedRepos, landscape.SelectedRepos, limit, status)
	if top <= 0 {
		return
	}
	shown := min(top, len(landscape.Repositories))
	for _, repo := range landscape.Repositories[:shown] {
		runs := "unknown"
		if repo.WorkflowRunCountKnown {
			runs = comma(repo.WorkflowRunCount)
		}
		selected := "not selected"
		if repo.Selected {
			selected = "selected"
		}
		fmt.Fprintf(out, "  #%d %-36s runs %8s  size %7s  pushed %-10s  %s\n",
			repo.Rank,
			truncate(repo.Repo, 36),
			runs,
			comma(repo.Size),
			formatRepositoryLandscapeDate(repo.PushedAt),
			selected,
		)
	}
}

func formatRepositoryLandscapeDate(value string) string {
	if value == "" {
		return "-"
	}
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return t.Format("2006-01-02")
	}
	if len(value) >= len("2006-01-02") {
		return value[:len("2006-01-02")]
	}
	return value
}

func formatEstimateNumber(value float64) string {
	return comma(int(math.Round(value)))
}

func printScanSummary(out io.Writer, summary scanSummary) {
	printScanSummaryForProvider(out, summary, githubProvider)
}

func printScanSummaryForProvider(out io.Writer, summary scanSummary, provider string) {
	targetLabel := "repositories"
	if provider == circleCIProvider {
		targetLabel = "projects"
	}
	fmt.Fprintln(out, "\nScan summary:")
	fmt.Fprintf(out, "  %s: queued %d  scanned %d  skipped %d\n",
		targetLabel,
		summary.RepositoriesQueued, summary.RepositoriesScanned, summary.RepositoriesSkipped)
	if summary.Pipelines > 0 {
		fmt.Fprintf(out, "  pipelines: %s\n", comma(summary.Pipelines))
	}
	fmt.Fprintf(out, "  workflow runs: %s  workflow jobs seen: %s  jobs used: %s\n",
		comma(summary.WorkflowRuns), comma(summary.WorkflowJobs), comma(summary.JobsUsed))
	fmt.Fprintf(out, "  API: %s requests  %s retries  %s rate-limit sleeps (%.1fs)\n",
		comma(summary.APIRequests), comma(summary.Retries), comma(summary.RateLimitSleeps), summary.RateLimitSleepSeconds)
	if len(summary.Conclusions) > 0 {
		var parts []string
		for _, key := range sortedStringKeys(summary.Conclusions) {
			parts = append(parts, key+"="+comma(summary.Conclusions[key]))
		}
		fmt.Fprintf(out, "  conclusions: %s\n", strings.Join(parts, ", "))
	}
	if len(summary.SkippedRepositories) > 0 {
		fmt.Fprintf(out, "  skipped %s:\n", targetLabel)
		for _, skipped := range summary.SkippedRepositories {
			fmt.Fprintf(out, "    %s (%s)\n", skipped.Repo, skipped.Reason)
		}
	}
}

func printUsageSummaries(out io.Writer, title string, summaries []usageSummary) {
	if len(summaries) == 0 {
		return
	}
	fmt.Fprintf(out, "\n%s\n", title)
	for _, summary := range summaries {
		fmt.Fprintf(
			out,
			"  %-40s runtime %7.2fh  peak %4d jobs  p95 %4d jobs  %8s total\n",
			truncate(summary.Name, 40),
			summary.JobRuntimeHours,
			summary.PeakConcurrency,
			summary.PercentileConcurrency["p95"],
			comma(summary.Jobs),
		)
	}
}

func truncate(value string, max int) string {
	if max < 1 || len(value) <= max {
		return value
	}
	if max <= 3 {
		return value[:max]
	}
	return value[:max-3] + "..."
}

func summarizeRepos(repos []string) string {
	if len(repos) == 0 {
		return "(none)"
	}
	if len(repos) <= 12 {
		return strings.Join(repos, ", ")
	}
	shown := append([]string{}, repos[:12]...)
	return strings.Join(shown, ", ") + fmt.Sprintf(", ... (%d total)", len(repos))
}

func comma(n int) string {
	s := strconv.Itoa(n)
	if len(s) <= 3 {
		return s
	}
	var out []byte
	first := len(s) % 3
	if first == 0 {
		first = 3
	}
	out = append(out, s[:first]...)
	for i := first; i < len(s); i += 3 {
		out = append(out, ',')
		out = append(out, s[i:i+3]...)
	}
	return string(out)
}
