package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
)

type resourceMap struct {
	Rules []resourceRule `json:"rules"`
}

type resourceRule struct {
	Name   string         `json:"name,omitempty"`
	Match  resourceMatch  `json:"match"`
	Target resourceTarget `json:"target"`
}

type resourceMatch struct {
	Provider      string   `json:"provider,omitempty"`
	Repo          string   `json:"repo,omitempty"`
	Workflow      string   `json:"workflow,omitempty"`
	Job           string   `json:"job,omitempty"`
	RunnerName    string   `json:"runner_name,omitempty"`
	RunnerGroup   string   `json:"runner_group,omitempty"`
	ResourceClass string   `json:"resource_class,omitempty"`
	Labels        []string `json:"labels,omitempty"`
}

type resourceTarget struct {
	Platform string `json:"platform,omitempty"`
	Shape    string `json:"shape,omitempty"`
	VCPUs    int    `json:"vcpus"`
}

type mappedResource struct {
	Record record
	Target resourceTarget
	Source string
}

type resourceCoverage struct {
	TotalJobs            int     `json:"total_jobs"`
	MappedJobs           int     `json:"mapped_jobs"`
	UnmappedJobs         int     `json:"unmapped_jobs"`
	JobPercent           float64 `json:"job_percent"`
	TotalRuntimeMinutes  float64 `json:"total_runtime_minutes"`
	MappedRuntimeMinutes float64 `json:"mapped_runtime_minutes"`
	RuntimePercent       float64 `json:"runtime_percent"`
}

type computeDemand struct {
	Platform          string         `json:"platform,omitempty"`
	Shape             string         `json:"shape,omitempty"`
	VCPUsPerJob       int            `json:"vcpus_per_job,omitempty"`
	Jobs              int            `json:"jobs"`
	JobRuntimeMinutes float64        `json:"job_runtime_minutes"`
	VCPUMinutes       float64        `json:"vcpu_minutes"`
	PeakVCPUs         int            `json:"peak_vcpus"`
	PercentileVCPUs   map[string]int `json:"percentile_vcpus"`
}

type computeUsageSummary struct {
	Name            string         `json:"name"`
	Jobs            int            `json:"jobs"`
	VCPUMinutes     float64        `json:"vcpu_minutes"`
	PeakVCPUs       int            `json:"peak_vcpus"`
	PercentileVCPUs map[string]int `json:"percentile_vcpus"`
}

type unmappedResourceGroup struct {
	Provider        string   `json:"provider"`
	OS              string   `json:"os,omitempty"`
	RunnerGroupName string   `json:"runner_group_name,omitempty"`
	ResourceClass   string   `json:"resource_class,omitempty"`
	Labels          []string `json:"labels,omitempty"`
	Jobs            int      `json:"jobs"`
	RuntimeMinutes  float64  `json:"runtime_minutes"`
}

type computeProjection struct {
	Model                  string                  `json:"model"`
	Coverage               resourceCoverage        `json:"coverage"`
	AssignmentSources      map[string]int          `json:"assignment_sources"`
	Overall                computeDemand           `json:"overall"`
	Platforms              []computeDemand         `json:"platforms"`
	Targets                []computeDemand         `json:"targets"`
	UnmappedResourceGroups []unmappedResourceGroup `json:"unmapped_resource_groups,omitempty"`
	TopRepositories        []computeUsageSummary   `json:"top_repositories,omitempty"`
	TopWorkflows           []computeUsageSummary   `json:"top_workflows,omitempty"`
	TopJobs                []computeUsageSummary   `json:"top_jobs,omitempty"`
}

func loadResourceMap(filename string) ([]resourceRule, error) {
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	decoder := json.NewDecoder(f)
	decoder.DisallowUnknownFields()
	var mapping resourceMap
	if err := decoder.Decode(&mapping); err != nil {
		return nil, fmt.Errorf("decode %s: %w", filename, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("decode %s: multiple JSON values", filename)
		}
		return nil, fmt.Errorf("decode %s: %w", filename, err)
	}
	for i := range mapping.Rules {
		if err := validateResourceRule(&mapping.Rules[i], i); err != nil {
			return nil, fmt.Errorf("%s: %w", filename, err)
		}
	}
	return mapping.Rules, nil
}

func validateResourceRule(rule *resourceRule, index int) error {
	if rule.Target.VCPUs <= 0 {
		return fmt.Errorf("rule %d target.vcpus must be greater than zero", index+1)
	}
	rule.Name = strings.TrimSpace(rule.Name)
	if rule.Name == "" {
		rule.Name = fmt.Sprintf("rule %d", index+1)
	}
	rule.Target.Platform = strings.ToLower(strings.TrimSpace(rule.Target.Platform))
	rule.Target.Shape = strings.TrimSpace(rule.Target.Shape)
	patterns := []struct {
		name  string
		value string
	}{
		{"provider", rule.Match.Provider},
		{"repo", rule.Match.Repo},
		{"workflow", rule.Match.Workflow},
		{"job", rule.Match.Job},
		{"runner_name", rule.Match.RunnerName},
		{"runner_group", rule.Match.RunnerGroup},
		{"resource_class", rule.Match.ResourceClass},
	}
	for _, pattern := range patterns {
		if err := validateResourcePattern(pattern.value); err != nil {
			return fmt.Errorf("rule %d match.%s: %w", index+1, pattern.name, err)
		}
	}
	for labelIndex, label := range rule.Match.Labels {
		if err := validateResourcePattern(label); err != nil {
			return fmt.Errorf("rule %d match.labels[%d]: %w", index+1, labelIndex, err)
		}
	}
	return nil
}

func validateResourcePattern(pattern string) error {
	if pattern == "" {
		return nil
	}
	if _, err := path.Match(strings.ToLower(pattern), ""); err != nil {
		return fmt.Errorf("invalid glob %q: %w", pattern, err)
	}
	return nil
}

func resourceRuleMatches(rule resourceRule, rec record) bool {
	provider := rec.Provider
	if provider == "" {
		provider = githubProvider
	}
	checks := [][2]string{
		{rule.Match.Provider, provider},
		{rule.Match.Repo, rec.Repo},
		{rule.Match.Workflow, rec.WorkflowName},
		{rule.Match.Job, rec.JobName},
		{rule.Match.RunnerName, rec.RunnerName},
		{rule.Match.RunnerGroup, rec.RunnerGroupName},
		{rule.Match.ResourceClass, rec.ResourceClass},
	}
	for _, check := range checks {
		if !resourcePatternMatches(check[0], check[1]) {
			return false
		}
	}
	for _, pattern := range rule.Match.Labels {
		matched := false
		for _, label := range rec.Labels {
			if resourcePatternMatches(pattern, label) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

func resourcePatternMatches(pattern, value string) bool {
	if pattern == "" {
		return true
	}
	matched, err := path.Match(strings.ToLower(pattern), strings.ToLower(value))
	return err == nil && matched
}

func resolveResource(rec record, cfg config) (mappedResource, bool) {
	for _, rule := range cfg.resourceRules {
		if resourceRuleMatches(rule, rec) {
			target := normalizedResourceTarget(rule.Target, rec)
			return mappedResource{Record: rec, Target: target, Source: "rule:" + rule.Name}, true
		}
	}
	if cfg.defaultVCPUs > 0 {
		target := normalizedResourceTarget(resourceTarget{VCPUs: cfg.defaultVCPUs}, rec)
		return mappedResource{Record: rec, Target: target, Source: "default"}, true
	}
	return mappedResource{}, false
}

func normalizedResourceTarget(target resourceTarget, rec record) resourceTarget {
	target.Platform = strings.ToLower(strings.TrimSpace(target.Platform))
	if target.Platform == "" {
		target.Platform = strings.ToLower(strings.TrimSpace(rec.OS))
	}
	if target.Platform == "" {
		target.Platform = "unknown"
	}
	target.Shape = strings.TrimSpace(target.Shape)
	return target
}

func buildComputeProjection(records []record, cfg config) *computeProjection {
	return buildComputeProjectionWithDetails(records, cfg, true)
}

func buildComputeProjectionWithDetails(records []record, cfg config, includeDetails bool) *computeProjection {
	if cfg.resourceMapFile == "" && cfg.defaultVCPUs == 0 {
		return nil
	}

	mapped := make([]mappedResource, 0, len(records))
	unmapped := make([]record, 0)
	sources := map[string]int{}
	for _, rec := range records {
		assignment, ok := resolveResource(rec, cfg)
		if !ok {
			unmapped = append(unmapped, rec)
			continue
		}
		mapped = append(mapped, assignment)
		sources[assignment.Source]++
	}

	totalRuntimeMinutes := totalJobRuntimeSeconds(records) / 60.0
	mappedRuntimeMinutes := totalMappedRuntimeSeconds(mapped) / 60.0
	coverage := resourceCoverage{
		TotalJobs:            len(records),
		MappedJobs:           len(mapped),
		UnmappedJobs:         len(records) - len(mapped),
		TotalRuntimeMinutes:  roundFloat(totalRuntimeMinutes, 2),
		MappedRuntimeMinutes: roundFloat(mappedRuntimeMinutes, 2),
	}
	if coverage.TotalJobs > 0 {
		coverage.JobPercent = roundFloat(float64(coverage.MappedJobs)*100/float64(coverage.TotalJobs), 1)
	}
	if totalRuntimeMinutes > 0 {
		coverage.RuntimePercent = roundFloat(mappedRuntimeMinutes*100/totalRuntimeMinutes, 1)
	}

	byPlatform := map[string][]mappedResource{}
	for _, assignment := range mapped {
		byPlatform[assignment.Target.Platform] = append(byPlatform[assignment.Target.Platform], assignment)
	}
	platformNames := make([]string, 0, len(byPlatform))
	for platform := range byPlatform {
		platformNames = append(platformNames, platform)
	}
	sort.Strings(platformNames)
	platforms := make([]computeDemand, 0, len(platformNames))
	for _, platform := range platformNames {
		platforms = append(platforms, computeDemandFor(platform, byPlatform[platform]))
	}

	projection := &computeProjection{
		Model:             "target_vcpu",
		Coverage:          coverage,
		AssignmentSources: sources,
		Overall:           computeDemandFor("", mapped),
		Platforms:         platforms,
		Targets:           computeTargetDemands(mapped),
	}
	if includeDetails {
		projection.UnmappedResourceGroups = summarizeUnmappedResources(unmapped)
		projection.TopRepositories = topComputeUsageSummaries(mapped, cfg.top, func(rec record) string { return rec.Repo })
		projection.TopWorkflows = topComputeUsageSummaries(mapped, cfg.top, workflowSummaryName)
		projection.TopJobs = topComputeUsageSummaries(mapped, cfg.top, jobSummaryName)
	}
	return projection
}

type targetResourceKey struct {
	platform string
	shape    string
	vcpus    int
}

func computeTargetDemands(mapped []mappedResource) []computeDemand {
	grouped := map[targetResourceKey][]mappedResource{}
	for _, assignment := range mapped {
		key := targetResourceKey{
			platform: assignment.Target.Platform,
			shape:    assignment.Target.Shape,
			vcpus:    assignment.Target.VCPUs,
		}
		grouped[key] = append(grouped[key], assignment)
	}
	keys := make([]targetResourceKey, 0, len(grouped))
	for key := range grouped {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].platform != keys[j].platform {
			return keys[i].platform < keys[j].platform
		}
		if keys[i].vcpus != keys[j].vcpus {
			return keys[i].vcpus < keys[j].vcpus
		}
		return keys[i].shape < keys[j].shape
	})
	out := make([]computeDemand, 0, len(keys))
	for _, key := range keys {
		demand := computeDemandFor(key.platform, grouped[key])
		demand.Shape = key.shape
		demand.VCPUsPerJob = key.vcpus
		out = append(out, demand)
	}
	return out
}

func totalMappedRuntimeSeconds(mapped []mappedResource) float64 {
	total := 0.0
	for _, assignment := range mapped {
		if assignment.Record.End.After(assignment.Record.Start) {
			total += assignment.Record.End.Sub(assignment.Record.Start).Seconds()
		}
	}
	return total
}

func computeDemandFor(platform string, mapped []mappedResource) computeDemand {
	intervals := make([]weightedInterval, 0, len(mapped))
	runtimeSeconds := 0.0
	vcpuSeconds := 0.0
	for _, assignment := range mapped {
		duration := assignment.Record.End.Sub(assignment.Record.Start).Seconds()
		if duration <= 0 {
			continue
		}
		intervals = append(intervals, weightedInterval{
			Start:  assignment.Record.Start,
			End:    assignment.Record.End,
			Weight: assignment.Target.VCPUs,
		})
		runtimeSeconds += duration
		vcpuSeconds += duration * float64(assignment.Target.VCPUs)
	}
	peak, profile := weightedConcurrencyProfile(intervals)
	pct := percentiles(profile, []int{50, 90, 95, 99})
	return computeDemand{
		Platform:          platform,
		Jobs:              len(mapped),
		JobRuntimeMinutes: roundFloat(runtimeSeconds/60.0, 2),
		VCPUMinutes:       roundFloat(vcpuSeconds/60.0, 2),
		PeakVCPUs:         peak,
		PercentileVCPUs:   map[string]int{"p50": pct[50], "p90": pct[90], "p95": pct[95], "p99": pct[99]},
	}
}

func topComputeUsageSummaries(mapped []mappedResource, top int, keyFor func(record) string) []computeUsageSummary {
	if top <= 0 {
		return nil
	}
	grouped := map[string][]mappedResource{}
	for _, assignment := range mapped {
		name := strings.TrimSpace(keyFor(assignment.Record))
		if name == "" {
			name = "unknown"
		}
		grouped[name] = append(grouped[name], assignment)
	}
	summaries := make([]computeUsageSummary, 0, len(grouped))
	for name, assignments := range grouped {
		demand := computeDemandFor("", assignments)
		summaries = append(summaries, computeUsageSummary{
			Name:            name,
			Jobs:            demand.Jobs,
			VCPUMinutes:     demand.VCPUMinutes,
			PeakVCPUs:       demand.PeakVCPUs,
			PercentileVCPUs: demand.PercentileVCPUs,
		})
	}
	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].VCPUMinutes != summaries[j].VCPUMinutes {
			return summaries[i].VCPUMinutes > summaries[j].VCPUMinutes
		}
		return strings.ToLower(summaries[i].Name) < strings.ToLower(summaries[j].Name)
	})
	if len(summaries) > top {
		summaries = summaries[:top]
	}
	return summaries
}

type unmappedResourceKey struct {
	provider        string
	osName          string
	runnerGroupName string
	resourceClass   string
	labels          string
}

func summarizeUnmappedResources(records []record) []unmappedResourceGroup {
	groups := map[unmappedResourceKey]*unmappedResourceGroup{}
	for _, rec := range records {
		provider := rec.Provider
		if provider == "" {
			provider = githubProvider
		}
		labels := append([]string{}, rec.Labels...)
		sort.Strings(labels)
		key := unmappedResourceKey{
			provider:        strings.ToLower(provider),
			osName:          strings.ToLower(rec.OS),
			runnerGroupName: strings.ToLower(rec.RunnerGroupName),
			resourceClass:   strings.ToLower(rec.ResourceClass),
			labels:          strings.ToLower(strings.Join(labels, "\x00")),
		}
		group := groups[key]
		if group == nil {
			group = &unmappedResourceGroup{
				Provider:        provider,
				OS:              rec.OS,
				RunnerGroupName: rec.RunnerGroupName,
				ResourceClass:   rec.ResourceClass,
				Labels:          labels,
			}
			groups[key] = group
		}
		group.Jobs++
		group.RuntimeMinutes += rec.End.Sub(rec.Start).Seconds() / 60.0
	}
	out := make([]unmappedResourceGroup, 0, len(groups))
	for _, group := range groups {
		group.RuntimeMinutes = roundFloat(group.RuntimeMinutes, 2)
		out = append(out, *group)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].RuntimeMinutes != out[j].RuntimeMinutes {
			return out[i].RuntimeMinutes > out[j].RuntimeMinutes
		}
		return unmappedResourceName(out[i]) < unmappedResourceName(out[j])
	})
	return out
}

func unmappedResourceName(group unmappedResourceGroup) string {
	parts := []string{group.Provider}
	if group.RunnerGroupName != "" {
		parts = append(parts, "runner_group="+group.RunnerGroupName)
	}
	if group.ResourceClass != "" {
		parts = append(parts, "resource_class="+group.ResourceClass)
	}
	if len(group.Labels) > 0 {
		parts = append(parts, "labels="+strings.Join(group.Labels, ","))
	} else if group.OS != "" {
		parts = append(parts, "os="+group.OS)
	}
	return strings.Join(parts, " ")
}
