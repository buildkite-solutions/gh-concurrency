package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestComputeProjectionWeightsVCPUs(t *testing.T) {
	small := rec(300, "linux", false)
	small.Repo = "acme/api"
	small.WorkflowName = "CI"
	small.JobName = "small test"
	small.Labels = []string{"ubuntu-latest", "small"}
	large := small
	large.JobName = "large test"
	large.Labels = []string{"ubuntu-latest", "large"}

	cfg := config{
		resourceMapFile: "resources.json",
		resourceRules: []resourceRule{
			{Name: "small", Match: resourceMatch{Labels: []string{"small"}}, Target: resourceTarget{Platform: "linux", Shape: "small", VCPUs: 2}},
			{Name: "large", Match: resourceMatch{Labels: []string{"large"}}, Target: resourceTarget{Platform: "linux", Shape: "medium", VCPUs: 4}},
		},
		top: 10,
	}
	projection := buildComputeProjection([]record{small, large}, cfg)
	if projection == nil {
		t.Fatal("projection is nil")
	}
	if projection.Coverage.MappedJobs != 2 || projection.Coverage.RuntimePercent != 100 {
		t.Fatalf("coverage = %#v", projection.Coverage)
	}
	if projection.Overall.VCPUMinutes != 30 || projection.Overall.PeakVCPUs != 6 || projection.Overall.PercentileVCPUs["p95"] != 6 {
		t.Fatalf("overall = %#v, want 30 vCPU-min and peak/p95 6", projection.Overall)
	}
	if len(projection.Targets) != 2 || projection.Targets[0].VCPUsPerJob != 2 || projection.Targets[1].VCPUsPerJob != 4 {
		t.Fatalf("targets = %#v", projection.Targets)
	}
	if len(projection.TopRepositories) != 1 || projection.TopRepositories[0].VCPUMinutes != 30 {
		t.Fatalf("top repositories = %#v", projection.TopRepositories)
	}
}

func TestComputeProjectionReportsPartialCoverage(t *testing.T) {
	mapped := rec(300, "linux", false)
	mapped.Labels = []string{"small"}
	unmapped := rec(600, "linux", false)
	unmapped.Labels = []string{"custom-large"}

	projection := buildComputeProjection([]record{mapped, unmapped}, config{
		resourceMapFile: "resources.json",
		resourceRules: []resourceRule{{
			Name: "small", Match: resourceMatch{Labels: []string{"small"}}, Target: resourceTarget{VCPUs: 2},
		}},
	})
	if projection.Coverage.JobPercent != 50 || projection.Coverage.RuntimePercent != 33.3 {
		t.Fatalf("coverage = %#v, want 50%% jobs and 33.3%% runtime", projection.Coverage)
	}
	if projection.Overall.VCPUMinutes != 10 || projection.Overall.PeakVCPUs != 2 {
		t.Fatalf("overall = %#v, want mapped-only 10 vCPU-min", projection.Overall)
	}
	if len(projection.UnmappedResourceGroups) != 1 || projection.UnmappedResourceGroups[0].RuntimeMinutes != 10 {
		t.Fatalf("unmapped groups = %#v", projection.UnmappedResourceGroups)
	}
}

func TestComputeProjectionUsesExplicitDefault(t *testing.T) {
	projection := buildComputeProjection([]record{rec(300, "linux", false), rec(600, "linux", false)}, config{defaultVCPUs: 2})
	if projection == nil || projection.Coverage.MappedJobs != 2 || projection.AssignmentSources["default"] != 2 {
		t.Fatalf("projection = %#v", projection)
	}
	if projection.Overall.VCPUMinutes != 30 || projection.Overall.PeakVCPUs != 4 {
		t.Fatalf("overall = %#v, want 30 vCPU-min and peak 4", projection.Overall)
	}
}

func TestResourceRuleMatchingIsOrderedAndCaseInsensitive(t *testing.T) {
	rec := record{
		Provider:        githubProvider,
		Repo:            "Acme/API",
		WorkflowName:    "CI",
		JobName:         "Test Linux",
		Labels:          []string{"SELF-HOSTED", "large-8vcpu"},
		RunnerGroupName: "Linux-Large",
		OS:              "linux",
	}
	cfg := config{resourceRules: []resourceRule{
		{Name: "specific", Match: resourceMatch{Repo: "acme/*", Job: "test*", RunnerGroup: "linux-*", Labels: []string{"*-8VCPU"}}, Target: resourceTarget{Shape: "large", VCPUs: 8}},
		{Name: "fallback", Match: resourceMatch{}, Target: resourceTarget{VCPUs: 2}},
	}}
	mapped, ok := resolveResource(rec, cfg)
	if !ok || mapped.Target.VCPUs != 8 || mapped.Source != "rule:specific" {
		t.Fatalf("mapped = %#v/%v", mapped, ok)
	}
}

func TestLoadResourceMapStrictValidation(t *testing.T) {
	dir := t.TempDir()
	valid := filepath.Join(dir, "valid.json")
	if err := os.WriteFile(valid, []byte(`{
  "rules": [{
    "name": "large linux",
    "match": {"runner_group": "linux-*", "labels": ["*8vcpu*"]},
    "target": {"platform": "linux", "shape": "large", "vcpus": 8}
  }]
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	rules, err := loadResourceMap(valid)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 1 || rules[0].Target.VCPUs != 8 {
		t.Fatalf("rules = %#v", rules)
	}

	invalid := filepath.Join(dir, "invalid.json")
	if err := os.WriteFile(invalid, []byte(`{"rules":[{"match":{},"target":{"vcpus":0}}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadResourceMap(invalid); err == nil || !strings.Contains(err.Error(), "greater than zero") {
		t.Fatalf("loadResourceMap err = %v", err)
	}

	invalidGlob := filepath.Join(dir, "invalid-glob.json")
	if err := os.WriteFile(invalidGlob, []byte(`{"rules":[{"match":{"job":"["},"target":{"vcpus":2}}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadResourceMap(invalidGlob); err == nil || !strings.Contains(err.Error(), "invalid glob") {
		t.Fatalf("loadResourceMap err = %v", err)
	}

	unknown := filepath.Join(dir, "unknown.json")
	if err := os.WriteFile(unknown, []byte(`{"rules":[],"unexpected":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadResourceMap(unknown); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("loadResourceMap err = %v", err)
	}
}
