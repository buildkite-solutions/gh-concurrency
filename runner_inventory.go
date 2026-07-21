package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

const (
	standardRunnerSpecSource    = "github-standard-runner-reference"
	standardRunnerSpecUpdatedAt = "2026-07-20"
	hostedRunnerInventorySource = "github-hosted-runner-inventory"
)

type runnerMachineSpec struct {
	Platform     string
	Architecture string
	CPUCores     int
	MemoryGB     int
	StorageGB    int
}

type standardRunnerSpecs struct {
	Public  runnerMachineSpec
	Private runnerMachineSpec
}

// githubStandardRunnerSpecs mirrors the current public and private/internal
// tables at https://docs.github.com/actions/reference/runners/github-hosted-runners.
var githubStandardRunnerSpecs = buildGitHubStandardRunnerSpecs()

func buildGitHubStandardRunnerSpecs() map[string]standardRunnerSpecs {
	linuxPublic := runnerMachineSpec{Platform: "linux-x64", Architecture: "x64", CPUCores: 4, MemoryGB: 16, StorageGB: 14}
	linuxPrivate := runnerMachineSpec{Platform: "linux-x64", Architecture: "x64", CPUCores: 2, MemoryGB: 8, StorageGB: 14}
	windowsPublic := runnerMachineSpec{Platform: "win-x64", Architecture: "x64", CPUCores: 4, MemoryGB: 16, StorageGB: 14}
	windowsPrivate := runnerMachineSpec{Platform: "win-x64", Architecture: "x64", CPUCores: 2, MemoryGB: 8, StorageGB: 14}
	linuxArmPublic := runnerMachineSpec{Platform: "linux-arm64", Architecture: "arm64", CPUCores: 4, MemoryGB: 16, StorageGB: 14}
	linuxArmPrivate := runnerMachineSpec{Platform: "linux-arm64", Architecture: "arm64", CPUCores: 2, MemoryGB: 8, StorageGB: 14}
	windowsArmPublic := runnerMachineSpec{Platform: "win-arm64", Architecture: "arm64", CPUCores: 4, MemoryGB: 16, StorageGB: 14}
	windowsArmPrivate := runnerMachineSpec{Platform: "win-arm64", Architecture: "arm64", CPUCores: 2, MemoryGB: 8, StorageGB: 14}
	macIntel := runnerMachineSpec{Platform: "macos-x64", Architecture: "x64", CPUCores: 4, MemoryGB: 14, StorageGB: 14}
	macArm := runnerMachineSpec{Platform: "macos-arm64", Architecture: "arm64", CPUCores: 3, MemoryGB: 7, StorageGB: 14}
	slim := runnerMachineSpec{Platform: "linux-x64", Architecture: "x64", CPUCores: 1, MemoryGB: 5, StorageGB: 14}

	out := map[string]standardRunnerSpecs{}
	add := func(labels []string, public, private runnerMachineSpec) {
		for _, label := range labels {
			out[strings.ToLower(label)] = standardRunnerSpecs{Public: public, Private: private}
		}
	}
	add([]string{"ubuntu-slim"}, slim, slim)
	add([]string{"ubuntu-latest", "ubuntu-24.04", "ubuntu-22.04", "ubuntu-26.04"}, linuxPublic, linuxPrivate)
	add([]string{"windows-latest", "windows-2025", "windows-2022"}, windowsPublic, windowsPrivate)
	add([]string{"ubuntu-24.04-arm", "ubuntu-22.04-arm", "ubuntu-26.04-arm"}, linuxArmPublic, linuxArmPrivate)
	add([]string{"windows-11-arm", "windows-11-vs2026-arm"}, windowsArmPublic, windowsArmPrivate)
	add([]string{"macos-15-intel", "macos-26-intel"}, macIntel, macIntel)
	add([]string{"macos-latest", "macos-14", "macos-15", "macos-26"}, macArm, macArm)
	// This public-preview image is currently documented only for public repositories.
	out["windows-2025-vs2026"] = standardRunnerSpecs{Public: windowsPublic}
	return out
}

type hostedRunnerInventoryEntry struct {
	ID                 int64                   `json:"id"`
	Name               string                  `json:"name"`
	RunnerGroupID      int64                   `json:"runner_group_id"`
	Platform           string                  `json:"platform"`
	Image              hostedRunnerImage       `json:"image"`
	MachineSizeDetails hostedRunnerMachineSize `json:"machine_size_details"`
}

type hostedRunnerImage struct {
	ID      string `json:"id"`
	Source  string `json:"source"`
	Version string `json:"version"`
}

type hostedRunnerMachineSize struct {
	ID        string `json:"id"`
	CPUCores  int    `json:"cpu_cores"`
	MemoryGB  int    `json:"memory_gb"`
	StorageGB int    `json:"storage_gb"`
}

type hostedRunnerInventoryResult struct {
	ByOwner  map[string][]hostedRunnerInventoryEntry
	Fetched  map[string]bool
	Warnings []string
}

func prepareGitHubRunnerMetadata(records []record, repoInfos map[string]repositoryInfo, inventory hostedRunnerInventoryResult) []string {
	for i := range records {
		rec := &records[i]
		if rec.Provider != "" && rec.Provider != githubProvider {
			continue
		}
		rec.RepoVisibility = repositoryVisibility(repoInfos[repoInfoKey(rec.Repo)])
		if rec.SelfHosted {
			rec.RunnerType = "self-hosted"
			continue
		}

		rec.RunnerLabel = githubHostedRunnerLabel(rec.Labels)
		owner, _, ownerErr := splitRepoName(rec.Repo)
		if ownerErr == nil {
			if entry, ok := matchHostedRunnerInventory(*rec, inventory.ByOwner[strings.ToLower(owner)]); ok {
				rec.RunnerLabel = entry.Name
				rec.RunnerType = "larger"
				rec.Platform = entry.Platform
				rec.Architecture = platformArchitecture(entry.Platform)
				rec.RunnerImage = entry.Image.ID
				rec.MachineSize = entry.MachineSizeDetails.ID
				rec.CPUCores = entry.MachineSizeDetails.CPUCores
				rec.MemoryGB = entry.MachineSizeDetails.MemoryGB
				rec.StorageGB = entry.MachineSizeDetails.StorageGB
				rec.SpecSource = hostedRunnerInventorySource
				if osName := platformOS(entry.Platform); osName != "unknown" {
					rec.OS = osName
				}
				continue
			}
		}

		specs, standard := githubStandardRunnerSpecs[strings.ToLower(rec.RunnerLabel)]
		if standard {
			rec.RunnerType = "standard"
			if spec, ok := standardSpecForVisibility(specs, rec.RepoVisibility); ok {
				applyMachineSpec(rec, spec)
				rec.SpecSource = standardRunnerSpecSource
				rec.SpecUpdatedAt = standardRunnerSpecUpdatedAt
			}
			continue
		}

		rec.RunnerType = "github-hosted-unknown"
	}

	warnings := append([]string{}, inventory.Warnings...)
	unmatched := unmatchedInventoryLabels(records, inventory.Fetched)
	if len(unmatched) > 0 {
		warnings = append(warnings, "Larger-runner inventory did not match these GitHub-hosted labels: "+strings.Join(unmatched, ", ")+". They may be enterprise-scoped, renamed, or unavailable to the token.")
	}
	return warnings
}

func repositoryVisibility(info repositoryInfo) string {
	if !info.MetadataKnown {
		return "unknown"
	}
	if visibility := strings.ToLower(strings.TrimSpace(info.Visibility)); visibility != "" {
		return visibility
	}
	if info.Private {
		return "private"
	}
	return "public"
}

func standardSpecForVisibility(specs standardRunnerSpecs, visibility string) (runnerMachineSpec, bool) {
	switch strings.ToLower(visibility) {
	case "public":
		return specs.Public, specs.Public.CPUCores > 0
	case "private", "internal":
		return specs.Private, specs.Private.CPUCores > 0
	default:
		if specs.Public == specs.Private && specs.Public.CPUCores > 0 {
			return specs.Public, true
		}
		return runnerMachineSpec{}, false
	}
}

func applyMachineSpec(rec *record, spec runnerMachineSpec) {
	rec.Platform = spec.Platform
	rec.Architecture = spec.Architecture
	rec.CPUCores = spec.CPUCores
	rec.MemoryGB = spec.MemoryGB
	rec.StorageGB = spec.StorageGB
	if osName := platformOS(spec.Platform); osName != "unknown" {
		rec.OS = osName
	}
}

func githubHostedRunnerLabel(labels []string) string {
	cleaned := make([]string, 0, len(labels))
	seen := map[string]bool{}
	for _, label := range labels {
		label = strings.TrimSpace(label)
		key := strings.ToLower(label)
		if label == "" || seen[key] {
			continue
		}
		seen[key] = true
		cleaned = append(cleaned, label)
	}
	if len(cleaned) == 0 {
		return "unknown"
	}
	if len(cleaned) == 1 {
		return cleaned[0]
	}
	sort.Slice(cleaned, func(i, j int) bool { return strings.ToLower(cleaned[i]) < strings.ToLower(cleaned[j]) })
	return strings.Join(cleaned, "+")
}

func collectGitHubHostedRunnerInventory(client *githubClient, records []record) hostedRunnerInventoryResult {
	result := hostedRunnerInventoryResult{ByOwner: map[string][]hostedRunnerInventoryEntry{}, Fetched: map[string]bool{}}
	for _, owner := range githubHostedOwners(records) {
		client.logf("%s: listing GitHub-hosted larger runner inventory", owner)
		var entries []hostedRunnerInventoryEntry
		err := client.paginate("/orgs/"+url.PathEscape(owner)+"/actions/hosted-runners", nil, "runners", func(raw json.RawMessage) error {
			var entry hostedRunnerInventoryEntry
			if err := json.Unmarshal(raw, &entry); err != nil {
				return err
			}
			entries = append(entries, entry)
			return nil
		})
		if err != nil {
			result.Warnings = append(result.Warnings, hostedRunnerInventoryWarning(owner, err))
			continue
		}
		key := strings.ToLower(owner)
		result.ByOwner[key] = entries
		result.Fetched[key] = true
	}
	return result
}

func githubHostedOwners(records []record) []string {
	seen := map[string]string{}
	for _, rec := range records {
		if (rec.Provider != "" && rec.Provider != githubProvider) || rec.SelfHosted {
			continue
		}
		owner, _, err := splitRepoName(rec.Repo)
		if err == nil {
			seen[strings.ToLower(owner)] = owner
		}
	}
	owners := make([]string, 0, len(seen))
	for _, owner := range seen {
		owners = append(owners, owner)
	}
	sort.Slice(owners, func(i, j int) bool { return strings.ToLower(owners[i]) < strings.ToLower(owners[j]) })
	return owners
}

func matchHostedRunnerInventory(rec record, entries []hostedRunnerInventoryEntry) (hostedRunnerInventoryEntry, bool) {
	var candidates []hostedRunnerInventoryEntry
	for _, entry := range entries {
		for _, label := range rec.Labels {
			if strings.EqualFold(strings.TrimSpace(label), strings.TrimSpace(entry.Name)) {
				candidates = append(candidates, entry)
				break
			}
		}
	}
	if rec.RunnerGroupID != 0 {
		for _, entry := range candidates {
			if entry.RunnerGroupID == rec.RunnerGroupID {
				return entry, true
			}
		}
		return hostedRunnerInventoryEntry{}, false
	}
	if len(candidates) == 1 {
		return candidates[0], true
	}
	return hostedRunnerInventoryEntry{}, false
}

func unmatchedInventoryLabels(records []record, fetched map[string]bool) []string {
	seen := map[string]string{}
	for _, rec := range records {
		if rec.RunnerType != "github-hosted-unknown" {
			continue
		}
		owner, _, err := splitRepoName(rec.Repo)
		if err != nil || !fetched[strings.ToLower(owner)] {
			continue
		}
		label := githubHostedRunnerLabel(rec.Labels)
		seen[strings.ToLower(label)] = label
	}
	labels := make([]string, 0, len(seen))
	for _, label := range seen {
		labels = append(labels, label)
	}
	sort.Slice(labels, func(i, j int) bool { return strings.ToLower(labels[i]) < strings.ToLower(labels[j]) })
	return labels
}

func hostedRunnerInventoryWarning(owner string, err error) string {
	var nf notFoundError
	var ae authError
	switch {
	case errors.As(err, &nf):
		return fmt.Sprintf("GitHub larger-runner inventory for %s was not found or is not visible to the token; continuing with workflow-job labels only.", owner)
	case errors.As(err, &ae):
		return fmt.Sprintf("GitHub larger-runner inventory for %s was unauthorized; continuing with workflow-job labels only.", owner)
	default:
		return fmt.Sprintf("GitHub larger-runner inventory for %s was unavailable (%v); continuing with workflow-job labels only.", owner, err)
	}
}

func platformOS(platform string) string {
	platform = strings.ToLower(strings.TrimSpace(platform))
	switch {
	case strings.HasPrefix(platform, "linux"):
		return "linux"
	case strings.HasPrefix(platform, "win"):
		return "windows"
	case strings.HasPrefix(platform, "mac"):
		return "macos"
	default:
		return "unknown"
	}
}

func platformArchitecture(platform string) string {
	platform = strings.ToLower(strings.TrimSpace(platform))
	switch {
	case strings.Contains(platform, "arm64"):
		return "arm64"
	case strings.Contains(platform, "x64"):
		return "x64"
	default:
		return "unknown"
	}
}
