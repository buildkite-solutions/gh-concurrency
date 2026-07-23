package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"sort"
	"strings"
)

func splitRepoName(repo string) (string, string, error) {
	repo = strings.TrimSpace(repo)
	if strings.Count(repo, "/") != 1 {
		return "", "", fmt.Errorf("invalid repo %q; expected OWNER/NAME", repo)
	}
	parts := strings.Split(repo, "/")
	if parts[0] == "" || parts[1] == "" || strings.ContainsAny(repo, " \t\r\n") {
		return "", "", fmt.Errorf("invalid repo %q; expected OWNER/NAME", repo)
	}
	return parts[0], parts[1], nil
}

func repoAPIPath(repo string) (string, error) {
	owner, name, err := splitRepoName(repo)
	if err != nil {
		return "", err
	}
	return "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name), nil
}

func readListFile(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseListFile(string(data)), nil
}

func parseListFile(contents string) []string {
	var values []string
	for _, line := range strings.Split(contents, "\n") {
		if idx := strings.Index(line, "#"); idx >= 0 {
			line = line[:idx]
		}
		line = strings.ReplaceAll(line, ",", " ")
		for _, value := range strings.Fields(line) {
			values = append(values, strings.TrimSpace(value))
		}
	}
	return values
}

type skippedRepository struct {
	Repo   string `json:"repo"`
	Reason string `json:"reason"`
}

func resolveTargetRepos(client *githubClient, cfg config, stderr io.Writer) ([]string, []skippedRepository, error) {
	repos, _, skipped, err := resolveTargetReposWithInfo(client, cfg, stderr)
	return repos, skipped, err
}

func resolveTargetReposWithInfo(client *githubClient, cfg config, stderr io.Writer) ([]string, map[string]repositoryInfo, []skippedRepository, error) {
	var repos []string
	var skipped []skippedRepository
	var directRepos []string
	repoInfos := map[string]repositoryInfo{}
	for _, repo := range cfg.repos {
		if err := validateRepo(repo); err != nil {
			return nil, nil, nil, err
		}
		directRepos = append(directRepos, repo)
	}

	for _, path := range cfg.repoFiles {
		fileRepos, err := readListFile(path)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("read --repo-file %s: %w", path, err)
		}
		for _, repo := range fileRepos {
			if err := validateRepo(repo); err != nil {
				return nil, nil, nil, err
			}
			directRepos = append(directRepos, repo)
		}
	}
	filteredDirectRepos, directSkipped, err := resolveDirectRepoInfos(client, directRepos, cfg.includeArchived, stderr)
	if err != nil {
		return nil, nil, nil, err
	}
	skipped = append(skipped, directSkipped...)
	for _, info := range filteredDirectRepos {
		if name := rememberRepositoryInfo(repoInfos, info); name != "" {
			repos = append(repos, name)
		}
	}

	orgs := append([]string{}, cfg.orgs...)
	for _, path := range cfg.orgFiles {
		fileOrgs, err := readListFile(path)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("read --org-file %s: %w", path, err)
		}
		for _, org := range fileOrgs {
			if err := validateOrg(org); err != nil {
				return nil, nil, nil, err
			}
			orgs = append(orgs, org)
		}
	}

	for _, org := range uniqueStrings(orgs) {
		client.logf("listing repositories for org %s", org)
		orgRepos, orgSkipped, err := listOrgRepoInfos(client, org, cfg.repoType, cfg.includeArchived)
		if err != nil {
			var nf notFoundError
			if errors.As(err, &nf) {
				fmt.Fprintf(stderr, "warning: org %s not found or no repository access; skipping.\n", org)
				continue
			}
			return nil, nil, nil, err
		}
		for _, info := range orgRepos {
			if name := rememberRepositoryInfo(repoInfos, info); name != "" {
				repos = append(repos, name)
			}
		}
		skipped = append(skipped, orgSkipped...)
	}

	repos = uniqueRepos(repos)
	sort.Slice(repos, func(i, j int) bool {
		return strings.ToLower(repos[i]) < strings.ToLower(repos[j])
	})
	sortSkippedRepositories(skipped)
	return repos, repoInfos, skipped, nil
}

func resolveCircleCIProjects(cfg config) ([]string, error) {
	var projects []string
	for _, project := range cfg.circleCIProjects {
		if err := validateCircleCIProject(project); err != nil {
			return nil, err
		}
		projects = append(projects, project)
	}
	for _, path := range cfg.circleCIProjectFiles {
		fileProjects, err := readListFile(path)
		if err != nil {
			return nil, fmt.Errorf("read --circleci-project-file %s: %w", path, err)
		}
		for _, project := range fileProjects {
			if err := validateCircleCIProject(project); err != nil {
				return nil, err
			}
			projects = append(projects, project)
		}
	}

	for _, repo := range cfg.repos {
		project, err := circleCIProjectFromRepo(cfg.circleCIVCS, repo)
		if err != nil {
			return nil, err
		}
		projects = append(projects, project)
	}
	for _, path := range cfg.repoFiles {
		fileRepos, err := readListFile(path)
		if err != nil {
			return nil, fmt.Errorf("read --repo-file %s: %w", path, err)
		}
		for _, repo := range fileRepos {
			project, err := circleCIProjectFromRepo(cfg.circleCIVCS, repo)
			if err != nil {
				return nil, err
			}
			projects = append(projects, project)
		}
	}

	projects = uniqueStrings(projects)
	sort.Slice(projects, func(i, j int) bool {
		return strings.ToLower(projects[i]) < strings.ToLower(projects[j])
	})
	return projects, nil
}

func circleCIProjectFromRepo(vcs, repo string) (string, error) {
	owner, name, err := splitRepoName(repo)
	if err != nil {
		return "", err
	}
	vcs = strings.TrimSpace(vcs)
	if vcs == "" {
		vcs = "gh"
	}
	project := vcs + "/" + owner + "/" + name
	if err := validateCircleCIProject(project); err != nil {
		return "", err
	}
	return project, nil
}

func resolveDirectRepoInfos(client *githubClient, repos []string, includeArchived bool, stderr io.Writer) ([]repositoryInfo, []skippedRepository, error) {
	if includeArchived {
		var out []repositoryInfo
		for _, repo := range uniqueRepos(repos) {
			out = append(out, repositoryInfo{FullName: repo})
		}
		return out, nil, nil
	}
	var out []repositoryInfo
	var skipped []skippedRepository
	for _, repo := range uniqueRepos(repos) {
		client.logf("%s: checking repository metadata", repo)
		info, err := getRepoInfo(client, repo)
		if err != nil {
			var nf notFoundError
			if errors.As(err, &nf) {
				fmt.Fprintf(stderr, "warning: %s not found or no repository access; skipping.\n", repo)
				skipped = append(skipped, skippedRepository{Repo: repo, Reason: "not found or no repository access"})
				continue
			}
			return nil, nil, err
		}
		if info.FullName == "" {
			info.FullName = repo
		}
		if info.Disabled {
			client.logf("skipping disabled repository %s", info.FullName)
			skipped = append(skipped, skippedRepository{Repo: info.FullName, Reason: "disabled"})
			continue
		}
		if info.Archived {
			client.logf("skipping archived repository %s", info.FullName)
			skipped = append(skipped, skippedRepository{Repo: info.FullName, Reason: "archived"})
			continue
		}
		out = append(out, info)
	}
	return out, skipped, nil
}

func sortSkippedRepositories(skipped []skippedRepository) {
	sort.Slice(skipped, func(i, j int) bool {
		if strings.ToLower(skipped[i].Repo) != strings.ToLower(skipped[j].Repo) {
			return strings.ToLower(skipped[i].Repo) < strings.ToLower(skipped[j].Repo)
		}
		return skipped[i].Reason < skipped[j].Reason
	})
}

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		key := strings.ToLower(value)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, value)
	}
	return out
}

func uniqueRepos(repos []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, repo := range repos {
		repo = strings.TrimSpace(repo)
		if repo == "" {
			continue
		}
		key := strings.ToLower(repo)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, repo)
	}
	return out
}

type repositoryInfo struct {
	FullName        string `json:"full_name"`
	Disabled        bool   `json:"disabled"`
	Archived        bool   `json:"archived"`
	Size            int    `json:"size"`
	PushedAt        string `json:"pushed_at"`
	UpdatedAt       string `json:"updated_at"`
	OpenIssuesCount int    `json:"open_issues_count"`
	StargazersCount int    `json:"stargazers_count"`
	ForksCount      int    `json:"forks_count"`
	DefaultBranch   string `json:"default_branch"`
	Fork            bool   `json:"fork"`
	Private         bool   `json:"private"`
}

func getRepoInfo(client *githubClient, repo string) (repositoryInfo, error) {
	repoPath, err := repoAPIPath(repo)
	if err != nil {
		return repositoryInfo{}, err
	}
	body, _, err := client.request(client.baseURL + repoPath)
	if err != nil {
		return repositoryInfo{}, err
	}
	var info repositoryInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return repositoryInfo{}, err
	}
	return info, nil
}

func listOrgRepos(client *githubClient, org, repoType string, includeArchived bool) ([]string, []skippedRepository, error) {
	infos, skipped, err := listOrgRepoInfos(client, org, repoType, includeArchived)
	if err != nil {
		return nil, nil, err
	}
	var repos []string
	for _, info := range infos {
		if info.FullName != "" {
			repos = append(repos, info.FullName)
		}
	}
	return repos, skipped, nil
}

func listOrgRepoInfos(client *githubClient, org, repoType string, includeArchived bool) ([]repositoryInfo, []skippedRepository, error) {
	if err := validateOrg(org); err != nil {
		return nil, nil, err
	}
	params := url.Values{
		"type":      []string{repoType},
		"sort":      []string{"full_name"},
		"direction": []string{"asc"},
	}
	var repos []repositoryInfo
	var skipped []skippedRepository
	err := client.paginate("/orgs/"+url.PathEscape(org)+"/repos", params, "", func(raw json.RawMessage) error {
		var repo repositoryInfo
		if err := json.Unmarshal(raw, &repo); err != nil {
			return err
		}
		if repo.FullName == "" {
			return nil
		}
		if repo.Disabled {
			skipped = append(skipped, skippedRepository{Repo: repo.FullName, Reason: "disabled"})
			return nil
		}
		if !includeArchived && repo.Archived {
			skipped = append(skipped, skippedRepository{Repo: repo.FullName, Reason: "archived"})
			return nil
		}
		repos = append(repos, repo)
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return repos, skipped, nil
}

func rememberRepositoryInfo(infos map[string]repositoryInfo, info repositoryInfo) string {
	name := strings.TrimSpace(info.FullName)
	if name == "" {
		return ""
	}
	info.FullName = name
	infos[repoInfoKey(name)] = info
	return name
}

func repoInfoKey(repo string) string {
	return strings.ToLower(strings.TrimSpace(repo))
}
