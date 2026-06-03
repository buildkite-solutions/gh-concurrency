package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type release struct {
	ID        int64   `json:"id"`
	UploadURL string  `json:"upload_url"`
	HTMLURL   string  `json:"html_url"`
	Assets    []asset `json:"assets"`
}

type asset struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	version := firstEnv("VERSION", "BUILDKITE_TAG")
	if version == "" {
		return errors.New("VERSION or BUILDKITE_TAG is required")
	}
	token := firstEnv("GITHUB_TOKEN", "GH_TOKEN")
	if token == "" {
		return errors.New("GITHUB_TOKEN or GH_TOKEN is required")
	}
	repo, err := releaseRepo()
	if err != nil {
		return err
	}

	client := &githubReleaseClient{
		apiBase: strings.TrimRight(firstEnvOr("GITHUB_API_URL", "https://api.github.com"), "/"),
		token:   token,
		http:    &http.Client{Timeout: 60 * time.Second},
	}

	rel, err := client.releaseByTag(repo, version)
	if err != nil {
		if !errors.Is(err, errNotFound) {
			return err
		}
		rel, err = client.createRelease(repo, version, releaseNotes(version))
		if err != nil {
			return err
		}
		fmt.Printf("created release %s\n", rel.HTMLURL)
	} else {
		fmt.Printf("updating release %s\n", rel.HTMLURL)
	}

	files, err := filepath.Glob("dist/*")
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return errors.New("no release assets found in dist/")
	}

	for _, file := range files {
		info, err := os.Stat(file)
		if err != nil {
			return err
		}
		if info.IsDir() {
			continue
		}
		name := filepath.Base(file)
		for _, existing := range rel.Assets {
			if existing.Name == name {
				if err := client.deleteAsset(repo, existing.ID); err != nil {
					return err
				}
				break
			}
		}
		if err := client.uploadAsset(rel.UploadURL, file, name); err != nil {
			return err
		}
		fmt.Printf("uploaded %s\n", name)
	}
	return nil
}

func firstEnv(keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}
	return ""
}

func firstEnvOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func releaseRepo() (string, error) {
	if repo := strings.TrimSpace(os.Getenv("GITHUB_REPOSITORY")); repo != "" {
		return trimRepo(repo), nil
	}
	if repo := parseRepoURL(os.Getenv("BUILDKITE_REPO")); repo != "" {
		return repo, nil
	}
	out, err := exec.Command("git", "config", "--get", "remote.origin.url").Output()
	if err != nil {
		return "", errors.New("could not determine GitHub repository; set GITHUB_REPOSITORY=owner/repo")
	}
	if repo := parseRepoURL(strings.TrimSpace(string(out))); repo != "" {
		return repo, nil
	}
	return "", errors.New("could not determine GitHub repository; set GITHUB_REPOSITORY=owner/repo")
}

func parseRepoURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(raw, "git@") {
		parts := strings.SplitN(raw, ":", 2)
		if len(parts) == 2 {
			return trimRepo(parts[1])
		}
	}
	if strings.HasPrefix(raw, "https://") || strings.HasPrefix(raw, "ssh://") {
		parsed, err := url.Parse(raw)
		if err == nil {
			return trimRepo(strings.TrimPrefix(parsed.Path, "/"))
		}
	}
	if strings.Count(raw, "/") == 1 {
		return trimRepo(raw)
	}
	return ""
}

func trimRepo(repo string) string {
	repo = strings.TrimSpace(repo)
	repo = strings.TrimPrefix(repo, "/")
	repo = strings.TrimSuffix(repo, ".git")
	return repo
}

func releaseNotes(version string) string {
	return fmt.Sprintf("Install with:\n\n```bash\ngh extension install buildkite-solutions/gh-concurrency\n```\n\nDocker image:\n\n```bash\ndocker pull ghcr.io/buildkite-solutions/gh-concurrency:%s\n```", version)
}

var errNotFound = errors.New("not found")

type githubReleaseClient struct {
	apiBase string
	token   string
	http    *http.Client
}

func (c *githubReleaseClient) releaseByTag(repo, tag string) (release, error) {
	var rel release
	err := c.request(http.MethodGet, fmt.Sprintf("/repos/%s/releases/tags/%s", repo, url.PathEscape(tag)), nil, &rel)
	return rel, err
}

func (c *githubReleaseClient) createRelease(repo, tag, notes string) (release, error) {
	payload := map[string]string{
		"tag_name": tag,
		"name":     tag,
		"body":     notes,
	}
	var rel release
	err := c.request(http.MethodPost, fmt.Sprintf("/repos/%s/releases", repo), payload, &rel)
	return rel, err
}

func (c *githubReleaseClient) deleteAsset(repo string, id int64) error {
	return c.request(http.MethodDelete, fmt.Sprintf("/repos/%s/releases/assets/%d", repo, id), nil, nil)
}

func (c *githubReleaseClient) uploadAsset(uploadURL, path, name string) error {
	base := strings.Split(uploadURL, "{")[0]
	u, err := url.Parse(base)
	if err != nil {
		return err
	}
	q := u.Query()
	q.Set("name", name)
	u.RawQuery = q.Encode()

	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, u.String(), bytes.NewReader(data))
	if err != nil {
		return err
	}
	c.addHeaders(req)
	if ct := mime.TypeByExtension(filepath.Ext(path)); ct != "" {
		req.Header.Set("Content-Type", ct)
	} else {
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	return c.do(req, nil)
}

func (c *githubReleaseClient) request(method, path string, payload any, into any) error {
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, c.apiBase+path, body)
	if err != nil {
		return err
	}
	c.addHeaders(req)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.do(req, into)
}

func (c *githubReleaseClient) addHeaders(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "gh-concurrency-release")
}

func (c *githubReleaseClient) do(req *http.Request, into any) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusNotFound {
		return errNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %s", req.Method, req.URL, strings.TrimSpace(string(body)))
	}
	if into == nil || len(body) == 0 {
		return nil
	}
	return json.Unmarshal(body, into)
}
