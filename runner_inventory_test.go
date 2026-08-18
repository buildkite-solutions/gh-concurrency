package main

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestGitHubStandardRunnerSpecCatalog(t *testing.T) {
	tests := []struct {
		label        string
		visibility   string
		cpu          int
		memory       int
		architecture string
		known        bool
	}{
		{label: "ubuntu-slim", visibility: "private", cpu: 1, memory: 5, architecture: "x64", known: true},
		{label: "ubuntu-24.04-arm", visibility: "private", cpu: 2, memory: 8, architecture: "arm64", known: true},
		{label: "windows-11-arm", visibility: "public", cpu: 4, memory: 16, architecture: "arm64", known: true},
		{label: "macos-15-intel", visibility: "private", cpu: 4, memory: 14, architecture: "x64", known: true},
		{label: "macos-latest", visibility: "public", cpu: 3, memory: 7, architecture: "arm64", known: true},
		{label: "windows-2025-vs2026", visibility: "public", cpu: 4, memory: 16, architecture: "x64", known: true},
		{label: "windows-2025-vs2026", visibility: "private", known: false},
	}

	for _, tt := range tests {
		t.Run(tt.label+"/"+tt.visibility, func(t *testing.T) {
			specs, exists := githubStandardRunnerSpecs[tt.label]
			if !exists {
				t.Fatalf("standard catalog missing %q", tt.label)
			}
			spec, known := standardSpecForVisibility(specs, tt.visibility)
			if known != tt.known {
				t.Fatalf("known = %t, want %t", known, tt.known)
			}
			if known && (spec.CPUCores != tt.cpu || spec.MemoryGB != tt.memory || spec.Architecture != tt.architecture || spec.StorageGB != 14) {
				t.Fatalf("spec = %#v, want %d vCPU/%d GB/%s/14 GB SSD", spec, tt.cpu, tt.memory, tt.architecture)
			}
		})
	}
}

func TestPrepareGitHubRunnerMetadataEnrichesStandardRunners(t *testing.T) {
	records := []record{
		{Repo: "o/public", Labels: []string{"ubuntu-latest"}, OS: "linux"},
		{Repo: "o/private", Labels: []string{"ubuntu-latest"}, OS: "linux"},
		{Repo: "o/mac", Labels: []string{"macos-latest"}, OS: "macos"},
	}
	repoInfos := map[string]repositoryInfo{
		"o/public":  {FullName: "o/public", Visibility: "public", MetadataKnown: true},
		"o/private": {FullName: "o/private", Visibility: "private", Private: true, MetadataKnown: true},
		"o/mac":     {FullName: "o/mac", Visibility: "private", Private: true, MetadataKnown: true},
	}

	warnings := prepareGitHubRunnerMetadata(records, repoInfos, hostedRunnerInventoryResult{})
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none", warnings)
	}
	if got := records[0]; got.RunnerLabel != "ubuntu-latest" || got.RunnerType != "standard" || got.RepoVisibility != "public" || got.CPUCores != 4 || got.MemoryGB != 16 || got.SpecSource != standardRunnerSpecSource {
		t.Fatalf("public runner = %#v, want enriched public standard runner", got)
	}
	if got := records[1]; got.RepoVisibility != "private" || got.CPUCores != 2 || got.MemoryGB != 8 {
		t.Fatalf("private runner = %#v, want 2 vCPU/8 GB", got)
	}
	if got := records[2]; got.CPUCores != 3 || got.MemoryGB != 7 || got.Architecture != "arm64" {
		t.Fatalf("mac runner = %#v, want 3 vCPU/7 GB arm64", got)
	}
}

func TestPrepareGitHubRunnerMetadataLeavesVisibilityDependentSpecUnknown(t *testing.T) {
	records := []record{{Repo: "o/r", Labels: []string{"ubuntu-latest"}, OS: "linux"}}
	prepareGitHubRunnerMetadata(records, nil, hostedRunnerInventoryResult{})

	got := records[0]
	if got.RunnerType != "standard" || got.RepoVisibility != "unknown" {
		t.Fatalf("runner = %#v, want standard with unknown visibility", got)
	}
	if got.CPUCores != 0 || got.MemoryGB != 0 || got.SpecSource != "" {
		t.Fatalf("runner spec = %#v, want hardware unknown without repository visibility", got)
	}
}

func TestCollectAndApplyGitHubHostedRunnerInventory(t *testing.T) {
	responses := map[string]fakeResponse{
		"/orgs/o/actions/hosted-runners": {body: map[string]any{
			"runners": []map[string]any{{
				"id":              5,
				"name":            "ubuntu-24.04-8core",
				"runner_group_id": 9,
				"platform":        "linux-x64",
				"image":           map[string]any{"id": "ubuntu-24.04", "source": "github"},
				"machine_size_details": map[string]any{
					"id":         "8-core",
					"cpu_cores":  8,
					"memory_gb":  32,
					"storage_gb": 300,
				},
			}},
		}},
	}
	client := newGitHubClient(defaultBaseURL, "tok", 1, false)
	client.httpClient = &http.Client{Transport: fakeTransport{responses: responses}}
	client.sleep = func(time.Duration) {}
	records := []record{{
		Repo:            "o/r",
		Labels:          []string{"ubuntu-24.04-8core"},
		RunnerID:        1044951360,
		RunnerGroupID:   9,
		RunnerGroupName: "large-runner-group",
		OS:              "linux",
	}}

	inventory := collectGitHubHostedRunnerInventory(client, records)
	warnings := prepareGitHubRunnerMetadata(records, map[string]repositoryInfo{
		"o/r": {FullName: "o/r", Visibility: "public", MetadataKnown: true},
	}, inventory)
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none", warnings)
	}
	got := records[0]
	if got.RunnerType != "larger" || got.RunnerLabel != "ubuntu-24.04-8core" || got.MachineSize != "8-core" || got.CPUCores != 8 || got.MemoryGB != 32 || got.StorageGB != 300 {
		t.Fatalf("larger runner = %#v, want inventory-enriched 8-core runner", got)
	}
	if got.Platform != "linux-x64" || got.Architecture != "x64" || got.RunnerImage != "ubuntu-24.04" || got.SpecSource != hostedRunnerInventorySource {
		t.Fatalf("larger runner platform metadata = %#v", got)
	}
	pool := classifyRunnerPool(got)
	if pool.name != "GitHub-hosted/ubuntu-24.04-8core/8-core" {
		t.Fatalf("pool name = %q, want label and machine size", pool.name)
	}
}

func TestRunnerInventoryTakesPrecedenceOverCollidingStandardLabel(t *testing.T) {
	records := []record{{Repo: "o/r", Labels: []string{"ubuntu-latest"}, RunnerGroupID: 9, OS: "linux"}}
	warnings := prepareGitHubRunnerMetadata(records, map[string]repositoryInfo{
		"o/r": {FullName: "o/r", Visibility: "private", MetadataKnown: true},
	}, hostedRunnerInventoryResult{
		ByOwner: map[string][]hostedRunnerInventoryEntry{"o": {{
			Name:          "ubuntu-latest",
			RunnerGroupID: 9,
			Platform:      "linux-x64",
			MachineSizeDetails: hostedRunnerMachineSize{
				ID: "16-core", CPUCores: 16, MemoryGB: 64, StorageGB: 600,
			},
		}}},
		Fetched: map[string]bool{"o": true},
	})
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none", warnings)
	}
	if got := records[0]; got.RunnerType != "larger" || got.MachineSize != "16-core" || got.CPUCores != 16 {
		t.Fatalf("runner = %#v, want inventory larger runner to override static standard label", got)
	}
}

func TestRunnerInventoryRequiresMatchingRunnerGroupWhenJobReportsOne(t *testing.T) {
	records := []record{{Repo: "o/r", Labels: []string{"ubuntu-latest"}, RunnerGroupID: 10, OS: "linux"}}
	prepareGitHubRunnerMetadata(records, map[string]repositoryInfo{
		"o/r": {FullName: "o/r", Visibility: "private", MetadataKnown: true},
	}, hostedRunnerInventoryResult{
		ByOwner: map[string][]hostedRunnerInventoryEntry{"o": {{
			Name: "ubuntu-latest", RunnerGroupID: 9, Platform: "linux-x64",
			MachineSizeDetails: hostedRunnerMachineSize{ID: "16-core", CPUCores: 16, MemoryGB: 64},
		}}},
		Fetched: map[string]bool{"o": true},
	})
	if got := records[0]; got.RunnerType != "standard" || got.CPUCores != 2 || got.MachineSize != "" {
		t.Fatalf("runner = %#v, want private standard fallback after runner-group mismatch", got)
	}
}

func TestGitHubHostedRunnerLabelPreservesMultipleExactLabels(t *testing.T) {
	got := githubHostedRunnerLabel([]string{"high-memory", "ubuntu-latest"})
	if got != "high-memory+ubuntu-latest" {
		t.Fatalf("githubHostedRunnerLabel = %q, want canonical exact-label set", got)
	}
}

func TestRunnerInventoryIncludesOwnersWithStandardLabels(t *testing.T) {
	owners := githubHostedOwners([]record{
		{Repo: "one/r", Labels: []string{"ubuntu-latest"}},
		{Repo: "two/r", Labels: []string{"larger-runner"}},
		{Repo: "three/r", Labels: []string{"self-hosted"}, SelfHosted: true},
		{Provider: circleCIProvider, Repo: "four/r"},
	})
	if got := strings.Join(owners, ","); got != "one,two" {
		t.Fatalf("githubHostedOwners = %q, want standard and non-standard GitHub-hosted owners", got)
	}
}

func TestRunnerInventoryUnavailableFallsBackToExactLabel(t *testing.T) {
	client := newGitHubClient(defaultBaseURL, "tok", 1, false)
	client.httpClient = &http.Client{Transport: fakeTransport{responses: map[string]fakeResponse{}}}
	client.sleep = func(time.Duration) {}
	records := []record{{Repo: "o/r", Labels: []string{"high-memory"}, OS: "unknown"}}

	inventory := collectGitHubHostedRunnerInventory(client, records)
	warnings := prepareGitHubRunnerMetadata(records, nil, inventory)
	if len(warnings) != 1 || !strings.Contains(warnings[0], "not found or is not visible") {
		t.Fatalf("warnings = %v, want permission-safe inventory warning", warnings)
	}
	if got := records[0]; got.RunnerLabel != "high-memory" || got.RunnerType != "github-hosted-unknown" || got.CPUCores != 0 {
		t.Fatalf("fallback runner = %#v, want exact label with unknown hardware", got)
	}
}

func TestRunnerInventoryWarnsWhenFetchedInventoryDoesNotMatch(t *testing.T) {
	records := []record{{Repo: "o/r", Labels: []string{"enterprise-runner"}, OS: "unknown"}}
	warnings := prepareGitHubRunnerMetadata(records, nil, hostedRunnerInventoryResult{
		ByOwner: map[string][]hostedRunnerInventoryEntry{"o": {}},
		Fetched: map[string]bool{"o": true},
	})
	if len(warnings) != 1 || !strings.Contains(warnings[0], "enterprise-runner") || !strings.Contains(warnings[0], "enterprise-scoped") {
		t.Fatalf("warnings = %v, want unmatched enterprise runner warning", warnings)
	}
}
