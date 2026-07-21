package main

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

var osMultiplier = map[string]int{
	"linux":   1,
	"windows": 2,
	"macos":   10,
}

func concurrencyProfile(intervals [][2]time.Time) (int, map[int]float64) {
	type event struct {
		t     time.Time
		delta int
	}
	var events []event
	for _, interval := range intervals {
		start, end := interval[0], interval[1]
		if !end.After(start) {
			continue
		}
		events = append(events, event{t: start, delta: 1}, event{t: end, delta: -1})
	}
	sort.Slice(events, func(i, j int) bool {
		if events[i].t.Equal(events[j].t) {
			return events[i].delta < events[j].delta
		}
		return events[i].t.Before(events[j].t)
	})

	running, peak := 0, 0
	var prev time.Time
	timeAtLevel := map[int]float64{}
	for i, event := range events {
		if i > 0 && running > 0 {
			timeAtLevel[running] += event.t.Sub(prev).Seconds()
		}
		running += event.delta
		if running < 0 {
			running = 0
		}
		if running > peak {
			peak = running
		}
		prev = event.t
	}
	return peak, timeAtLevel
}

func percentiles(timeAtLevel map[int]float64, ps []int) map[int]int {
	total := 0.0
	for _, seconds := range timeAtLevel {
		total += seconds
	}
	out := map[int]int{}
	if total == 0 {
		for _, p := range ps {
			out[p] = 0
		}
		return out
	}

	levels := sortedIntKeys(timeAtLevel)
	for _, p := range ps {
		target := total * float64(p) / 100.0
		cum := 0.0
		for _, level := range levels {
			cum += timeAtLevel[level]
			if cum >= target {
				out[p] = level
				break
			}
		}
	}
	return out
}

func sortedIntKeys[T any](m map[int]T) []int {
	keys := make([]int, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Ints(keys)
	return keys
}

type billableSlot struct {
	Jobs            int `json:"jobs"`
	BillableMinutes int `json:"billable_minutes"`
}

func billableMinutes(records []record) map[string]billableSlot {
	out := map[string]billableSlot{}
	for _, rec := range records {
		if rec.SelfHosted {
			continue
		}
		rawMinutes := rec.End.Sub(rec.Start).Seconds() / 60.0
		rounded := int(math.Ceil(rawMinutes))
		if rounded < 1 {
			rounded = 1
		}
		multiplier := osMultiplier[rec.OS]
		if multiplier == 0 {
			multiplier = 1
		}
		slot := out[rec.OS]
		slot.Jobs++
		slot.BillableMinutes += rounded * multiplier
		out[rec.OS] = slot
	}
	return out
}

type runnerPool struct {
	Name                  string         `json:"name"`
	Provider              string         `json:"provider,omitempty"`
	Jobs                  int            `json:"jobs"`
	BusyHours             float64        `json:"busy_hours"`
	PeakConcurrency       int            `json:"peak_concurrency"`
	PercentileConcurrency map[string]int `json:"percentile_concurrency"`
	GitHubHosted          bool           `json:"github_hosted"`
	SelfHosted            bool           `json:"self_hosted"`
	OS                    string         `json:"os,omitempty"`
	RunnerLabel           string         `json:"runner_label,omitempty"`
	RunnerType            string         `json:"runner_type,omitempty"`
	RepositoryVisibility  string         `json:"repository_visibility,omitempty"`
	Platform              string         `json:"platform,omitempty"`
	Architecture          string         `json:"architecture,omitempty"`
	RunnerImage           string         `json:"runner_image,omitempty"`
	MachineSize           string         `json:"machine_size,omitempty"`
	CPUCores              int            `json:"cpu_cores,omitempty"`
	MemoryGB              int            `json:"memory_gb,omitempty"`
	StorageGB             int            `json:"storage_gb,omitempty"`
	SpecSource            string         `json:"spec_source,omitempty"`
	SpecUpdatedAt         string         `json:"spec_updated_at,omitempty"`
	RunnerGroupName       string         `json:"runner_group_name,omitempty"`
	ResourceClass         string         `json:"resource_class,omitempty"`
	Executor              string         `json:"executor,omitempty"`
}

type usageSummary struct {
	Name                  string         `json:"name"`
	Jobs                  int            `json:"jobs"`
	BusyHours             float64        `json:"busy_hours"`
	PeakConcurrency       int            `json:"peak_concurrency"`
	PercentileConcurrency map[string]int `json:"percentile_concurrency"`
}

type runnerPoolKey struct {
	provider        string
	name            string
	gitHubHosted    bool
	selfHosted      bool
	osName          string
	runnerLabel     string
	runnerType      string
	repoVisibility  string
	platform        string
	architecture    string
	runnerImage     string
	machineSize     string
	cpuCores        int
	memoryGB        int
	storageGB       int
	specSource      string
	specUpdatedAt   string
	runnerGroupName string
	resourceClass   string
	executor        string
}

func runnerPools(records []record) []runnerPool {
	grouped := map[runnerPoolKey][]record{}
	for _, rec := range records {
		key := classifyRunnerPool(rec)
		grouped[key] = append(grouped[key], rec)
	}

	pools := make([]runnerPool, 0, len(grouped))
	for key, poolRecords := range grouped {
		intervals := make([][2]time.Time, 0, len(poolRecords))
		for _, rec := range poolRecords {
			intervals = append(intervals, [2]time.Time{rec.Start, rec.End})
		}
		peak, profile := concurrencyProfile(intervals)
		pct := percentiles(profile, []int{50, 90, 95, 99})
		busySeconds := 0.0
		for _, seconds := range profile {
			busySeconds += seconds
		}

		pools = append(pools, runnerPool{
			Name:                  key.name,
			Provider:              key.provider,
			Jobs:                  len(poolRecords),
			BusyHours:             math.Round((busySeconds/3600.0)*100) / 100,
			PeakConcurrency:       peak,
			PercentileConcurrency: map[string]int{"p50": pct[50], "p90": pct[90], "p95": pct[95], "p99": pct[99]},
			GitHubHosted:          key.gitHubHosted,
			SelfHosted:            key.selfHosted,
			OS:                    key.osName,
			RunnerLabel:           key.runnerLabel,
			RunnerType:            key.runnerType,
			RepositoryVisibility:  key.repoVisibility,
			Platform:              key.platform,
			Architecture:          key.architecture,
			RunnerImage:           key.runnerImage,
			MachineSize:           key.machineSize,
			CPUCores:              key.cpuCores,
			MemoryGB:              key.memoryGB,
			StorageGB:             key.storageGB,
			SpecSource:            key.specSource,
			SpecUpdatedAt:         key.specUpdatedAt,
			RunnerGroupName:       key.runnerGroupName,
			ResourceClass:         key.resourceClass,
			Executor:              key.executor,
		})
	}

	sort.Slice(pools, func(i, j int) bool {
		if pools[i].PeakConcurrency != pools[j].PeakConcurrency {
			return pools[i].PeakConcurrency > pools[j].PeakConcurrency
		}
		if pools[i].Jobs != pools[j].Jobs {
			return pools[i].Jobs > pools[j].Jobs
		}
		return strings.ToLower(pools[i].Name) < strings.ToLower(pools[j].Name)
	})
	return pools
}

func topUsageSummaries(records []record, top int, keyFor func(record) string) []usageSummary {
	if top <= 0 {
		return nil
	}
	grouped := map[string][]record{}
	for _, rec := range records {
		key := strings.TrimSpace(keyFor(rec))
		if key == "" {
			key = "unknown"
		}
		grouped[key] = append(grouped[key], rec)
	}
	summaries := make([]usageSummary, 0, len(grouped))
	for name, groupRecords := range grouped {
		summaries = append(summaries, summarizeUsage(name, groupRecords))
	}
	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].BusyHours != summaries[j].BusyHours {
			return summaries[i].BusyHours > summaries[j].BusyHours
		}
		if summaries[i].Jobs != summaries[j].Jobs {
			return summaries[i].Jobs > summaries[j].Jobs
		}
		if summaries[i].PeakConcurrency != summaries[j].PeakConcurrency {
			return summaries[i].PeakConcurrency > summaries[j].PeakConcurrency
		}
		return strings.ToLower(summaries[i].Name) < strings.ToLower(summaries[j].Name)
	})
	if len(summaries) > top {
		summaries = summaries[:top]
	}
	return summaries
}

func summarizeUsage(name string, records []record) usageSummary {
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
	return usageSummary{
		Name:                  name,
		Jobs:                  len(records),
		BusyHours:             math.Round((busySeconds/3600.0)*100) / 100,
		PeakConcurrency:       peak,
		PercentileConcurrency: map[string]int{"p50": pct[50], "p90": pct[90], "p95": pct[95], "p99": pct[99]},
	}
}

func workflowSummaryName(rec record) string {
	if rec.WorkflowName != "" {
		return rec.WorkflowName
	}
	return "unknown workflow"
}

func jobSummaryName(rec record) string {
	workflow := workflowSummaryName(rec)
	job := rec.JobName
	if job == "" {
		job = "unknown job"
	}
	return workflow + " / " + job
}

func classifyRunnerPool(rec record) runnerPoolKey {
	provider := rec.Provider
	if provider == "" {
		provider = githubProvider
	}
	osName := rec.OS
	if osName == "" {
		osName = "unknown"
	}
	if provider == circleCIProvider {
		resourceClass := strings.TrimSpace(rec.ResourceClass)
		if resourceClass == "" {
			resourceClass = "unknown"
		}
		executor := strings.TrimSpace(rec.Executor)
		return runnerPoolKey{
			provider:      circleCIProvider,
			name:          "CircleCI/" + resourceClass,
			osName:        osName,
			resourceClass: resourceClass,
			executor:      executor,
		}
	}
	if !rec.SelfHosted {
		if rec.RunnerLabel == "" {
			rec.RunnerLabel = githubHostedRunnerLabel(rec.Labels)
		}
		if rec.RunnerType == "" {
			if specs, ok := githubStandardRunnerSpecs[strings.ToLower(rec.RunnerLabel)]; ok {
				rec.RunnerType = "standard"
				if spec, known := standardSpecForVisibility(specs, rec.RepoVisibility); known {
					applyMachineSpec(&rec, spec)
					rec.SpecSource = standardRunnerSpecSource
					rec.SpecUpdatedAt = standardRunnerSpecUpdatedAt
				}
			} else {
				rec.RunnerType = "github-hosted-unknown"
			}
		}
		name := "GitHub-hosted/" + rec.RunnerLabel
		poolVisibility := ""
		if rec.RunnerType == "standard" {
			name += "/" + nonEmpty(rec.RepoVisibility, "visibility-unknown")
			poolVisibility = rec.RepoVisibility
		} else if rec.RunnerType == "larger" && rec.MachineSize != "" {
			name += "/" + rec.MachineSize
		}
		return runnerPoolKey{
			provider:       githubProvider,
			name:           name,
			gitHubHosted:   true,
			osName:         nonEmpty(rec.OS, "unknown"),
			runnerLabel:    rec.RunnerLabel,
			runnerType:     rec.RunnerType,
			repoVisibility: poolVisibility,
			platform:       rec.Platform,
			architecture:   rec.Architecture,
			runnerImage:    rec.RunnerImage,
			machineSize:    rec.MachineSize,
			cpuCores:       rec.CPUCores,
			memoryGB:       rec.MemoryGB,
			storageGB:      rec.StorageGB,
			specSource:     rec.SpecSource,
			specUpdatedAt:  rec.SpecUpdatedAt,
		}
	}

	groupName := runnerGroupPoolName(rec.RunnerGroupName)
	if groupName == "" {
		groupName = runnerPoolLabelHint(rec.Labels)
	}
	if groupName == "" {
		groupName = "unknown"
	}
	return runnerPoolKey{
		provider:        githubProvider,
		name:            "self-hosted/" + groupName,
		selfHosted:      true,
		runnerGroupName: groupName,
	}
}

func nonEmpty(value, fallback string) string {
	if value = strings.TrimSpace(value); value != "" {
		return value
	}
	return fallback
}

func runnerGroupPoolName(groupName string) string {
	groupName = strings.TrimSpace(groupName)
	if groupName == "" || strings.EqualFold(groupName, "default") {
		return ""
	}
	return groupName
}

func runnerPoolLabelHint(labels []string) string {
	for _, label := range labels {
		trimmed := strings.TrimSpace(label)
		normalized := strings.ToLower(trimmed)
		if normalized == "" || isGenericRunnerLabel(normalized) {
			continue
		}
		switch {
		case normalized == "blacksmith" || strings.HasPrefix(normalized, "blacksmith-"):
			return "blacksmith"
		case normalized == "runs-on" || strings.HasPrefix(normalized, "runs-on-") || strings.HasPrefix(normalized, "runson-"):
			return "runs-on"
		case normalized == "arc" || strings.HasPrefix(normalized, "arc-") || strings.Contains(normalized, "actions-runner-controller"):
			return "arc"
		default:
			return trimmed
		}
	}
	return ""
}

func isGenericRunnerLabel(label string) bool {
	switch label {
	case "self-hosted", "linux", "windows", "macos", "mac", "x64", "x86", "x86_64", "amd64", "arm", "arm64", "aarch64", "ubuntu", "ubuntu-latest", "default":
		return true
	}
	return strings.HasPrefix(label, "ubuntu-")
}

type queueStats struct {
	Count   int     `json:"count"`
	MedianS float64 `json:"median_s"`
	P95S    float64 `json:"p95_s"`
	MaxS    float64 `json:"max_s"`
}

func computeQueueStats(records []record) *queueStats {
	var qs []float64
	for _, rec := range records {
		if rec.QueueSeconds != nil {
			qs = append(qs, *rec.QueueSeconds)
		}
	}
	if len(qs) == 0 {
		return nil
	}
	sort.Float64s(qs)
	n := len(qs)
	return &queueStats{
		Count:   n,
		MedianS: qs[n/2],
		P95S:    qs[min(n-1, int(math.Ceil(float64(n)*0.95))-1)],
		MaxS:    qs[n-1],
	}
}

func detectWarnings(provider string, peak int, pct map[int]int, qstats *queueStats) []string {
	var warnings []string
	if qstats != nil && qstats.P95S > 60 {
		warnings = append(warnings, fmt.Sprintf(
			"95th-percentile queue time is %.0fs. Sustained queueing means jobs waited instead of running in parallel, so true demand is likely higher than the concurrency reported here.",
			qstats.P95S,
		))
	}
	roundPeaks := map[int]bool{5: true, 10: true, 20: true, 40: true, 60: true, 180: true, 300: true}
	if roundPeaks[peak] && pct[95] == peak {
		limitName := "a GitHub concurrency limit"
		if provider == circleCIProvider {
			limitName = "a CircleCI concurrency or resource limit"
		}
		warnings = append(warnings, fmt.Sprintf(
			"Peak (%d) sits at a round number and equals p95, which can indicate you were hitting %s. If so, real demand exceeds this figure.",
			peak, limitName,
		))
	}
	return warnings
}
