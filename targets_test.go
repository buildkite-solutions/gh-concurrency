package main

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseListFile(t *testing.T) {
	got := parseListFile(`
# comments are ignored
buildkite-solutions/gh-concurrency, buildkite-solutions/another
other-org/repo # inline comment

`)
	want := []string{
		"buildkite-solutions/gh-concurrency",
		"buildkite-solutions/another",
		"other-org/repo",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("parseListFile = %v, want %v", got, want)
	}
}

func TestResolveTargetReposExpandsOrgsAndFiles(t *testing.T) {
	dir := t.TempDir()
	repoFile := filepath.Join(dir, "repos.txt")
	orgFile := filepath.Join(dir, "orgs.txt")
	if err := os.WriteFile(repoFile, []byte("file-org/file-repo\nacme/api\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(orgFile, []byte("other\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	responses := map[string]fakeResponse{
		"/repos/explicit/repo": {
			body: map[string]any{"full_name": "explicit/repo"},
		},
		"/repos/file-org/file-repo": {
			body: map[string]any{"full_name": "file-org/file-repo"},
		},
		"/repos/acme/api": {
			body: map[string]any{"full_name": "acme/api"},
		},
		"/orgs/acme/repos": {
			body: []map[string]any{
				{"full_name": "acme/api"},
				{"full_name": "acme/web"},
				{"full_name": "acme/archived", "archived": true},
				{"full_name": "acme/disabled", "disabled": true},
			},
		},
		"/orgs/other/repos": {
			body: []map[string]any{
				{"full_name": "other/cli"},
			},
		},
	}
	client := newGitHubClient("https://api.github.com", "tok", 1, false)
	client.httpClient = &http.Client{Transport: fakeTransport{responses: responses}}
	client.sleep = func(time.Duration) {}

	got, skipped, err := resolveTargetRepos(client, config{
		repos:     []string{"explicit/repo"},
		orgs:      []string{"acme"},
		repoFiles: []string{repoFile},
		orgFiles:  []string{orgFile},
		repoType:  "all",
	}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"acme/api", "acme/web", "explicit/repo", "file-org/file-repo", "other/cli"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("resolveTargetRepos = %v, want %v", got, want)
	}
	if len(skipped) != 2 {
		t.Fatalf("skipped = %v, want archived and disabled repos", skipped)
	}
}

func TestResolveTargetReposIncludesArchivedWhenRequested(t *testing.T) {
	responses := map[string]fakeResponse{
		"/orgs/acme/repos": {
			body: []map[string]any{
				{"full_name": "acme/archived", "archived": true},
				{"full_name": "acme/web"},
			},
		},
	}
	client := newGitHubClient("https://api.github.com", "tok", 1, false)
	client.httpClient = &http.Client{Transport: fakeTransport{responses: responses}}
	client.sleep = func(time.Duration) {}

	got, skipped, err := resolveTargetRepos(client, config{
		orgs:            []string{"acme"},
		repoType:        "all",
		includeArchived: true,
	}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"acme/archived", "acme/web"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("resolveTargetRepos = %v, want %v", got, want)
	}
	if len(skipped) != 0 {
		t.Fatalf("skipped = %v, want empty", skipped)
	}
}

func TestResolveTargetReposWithInfoReadsVisibilityForIncludedArchivedDirectRepo(t *testing.T) {
	responses := map[string]fakeResponse{
		"/repos/acme/old": {
			body: map[string]any{"full_name": "acme/old", "archived": true, "private": true, "visibility": "private"},
		},
	}
	client := newGitHubClient("https://api.github.com", "tok", 1, false)
	client.httpClient = &http.Client{Transport: fakeTransport{responses: responses}}
	client.sleep = func(time.Duration) {}

	repos, infos, skipped, err := resolveTargetReposWithInfo(client, config{
		repos:           []string{"acme/old"},
		repoType:        "all",
		includeArchived: true,
	}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 1 || len(skipped) != 0 {
		t.Fatalf("repos/skipped = %v/%v, want included archived repo", repos, skipped)
	}
	if info := infos["acme/old"]; !info.MetadataKnown || info.Visibility != "private" {
		t.Fatalf("repository info = %#v, want known private visibility", info)
	}
}

func TestResolveTargetReposSkipsArchivedDirectReposByDefault(t *testing.T) {
	responses := map[string]fakeResponse{
		"/repos/acme/live": {
			body: map[string]any{"full_name": "acme/live"},
		},
		"/repos/acme/old": {
			body: map[string]any{"full_name": "acme/old", "archived": true},
		},
	}
	client := newGitHubClient("https://api.github.com", "tok", 1, false)
	client.httpClient = &http.Client{Transport: fakeTransport{responses: responses}}
	client.sleep = func(time.Duration) {}

	got, skipped, err := resolveTargetRepos(client, config{
		repos:    []string{"acme/live", "acme/old"},
		repoType: "all",
	}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"acme/live"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("resolveTargetRepos = %v, want %v", got, want)
	}
	if len(skipped) != 1 || skipped[0].Repo != "acme/old" || skipped[0].Reason != "archived" {
		t.Fatalf("skipped = %v, want acme/old archived", skipped)
	}
}

func TestResolveCircleCIProjectsExpandsReposAndFiles(t *testing.T) {
	dir := t.TempDir()
	repoFile := filepath.Join(dir, "repos.txt")
	projectFile := filepath.Join(dir, "projects.txt")
	if err := os.WriteFile(repoFile, []byte("acme/web\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(projectFile, []byte("circleci/org-id/project-id\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := resolveCircleCIProjects(config{
		provider:             circleCIProvider,
		repos:                []string{"acme/api"},
		repoFiles:            []string{repoFile},
		circleCIProjects:     []string{"gh/acme/mobile"},
		circleCIProjectFiles: []string{projectFile},
		circleCIVCS:          "gh",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"circleci/org-id/project-id", "gh/acme/api", "gh/acme/mobile", "gh/acme/web"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("resolveCircleCIProjects = %v, want %v", got, want)
	}
}
