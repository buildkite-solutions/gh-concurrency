package main

import (
	"bytes"
	"crypto"
	cryptorand "crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
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

type repoInstallation struct {
	ID int64 `json:"id"`
}

type installationToken struct {
	Token     string `json:"token"`
	ExpiresAt string `json:"expires_at"`
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
	repo, err := releaseRepo()
	if err != nil {
		return err
	}

	client := &githubReleaseClient{
		apiBase: strings.TrimRight(firstEnvOr("GITHUB_API_URL", "https://api.github.com"), "/"),
		http:    &http.Client{Timeout: 60 * time.Second},
	}

	token, err := client.githubAppInstallationToken(repo)
	if err != nil {
		return err
	}
	client.token = token

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

func githubAppJWT() (string, error) {
	issuer := firstEnv("GITHUB_APP_CLIENT_ID", "GITHUB_APP_ID")
	if issuer == "" {
		return "", errors.New("GITHUB_APP_CLIENT_ID or GITHUB_APP_ID is required")
	}
	key, err := githubAppPrivateKey()
	if err != nil {
		return "", err
	}

	now := time.Now().Unix()
	header := map[string]string{"typ": "JWT", "alg": "RS256"}
	payload := map[string]any{
		"iat": now - 60,
		"exp": now + 600,
		"iss": issuer,
	}

	headerJSON, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	signingInput := base64.RawURLEncoding.EncodeToString(headerJSON) + "." + base64.RawURLEncoding.EncodeToString(payloadJSON)
	digest := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(cryptorand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func githubAppPrivateKey() (*rsa.PrivateKey, error) {
	var data []byte
	if raw := os.Getenv("GITHUB_APP_PRIVATE_KEY"); strings.TrimSpace(raw) != "" {
		data = []byte(strings.ReplaceAll(raw, `\n`, "\n"))
	} else if raw := os.Getenv("GITHUB_APP_PRIVATE_KEY_B64"); strings.TrimSpace(raw) != "" {
		decoded, err := decodeBase64(strings.TrimSpace(raw))
		if err != nil {
			return nil, fmt.Errorf("decode GITHUB_APP_PRIVATE_KEY_B64: %w", err)
		}
		data = decoded
	} else {
		return nil, errors.New("GITHUB_APP_PRIVATE_KEY_B64 or GITHUB_APP_PRIVATE_KEY is required")
	}

	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("GitHub App private key is not valid PEM")
	}
	switch block.Type {
	case "RSA PRIVATE KEY":
		return x509.ParsePKCS1PrivateKey(block.Bytes)
	case "PRIVATE KEY":
		key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, err
		}
		rsaKey, ok := key.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("GitHub App private key is not an RSA key")
		}
		return rsaKey, nil
	default:
		return nil, fmt.Errorf("unsupported private key PEM type %q", block.Type)
	}
}

func decodeBase64(value string) ([]byte, error) {
	if decoded, err := base64.StdEncoding.DecodeString(value); err == nil {
		return decoded, nil
	}
	return base64.RawStdEncoding.DecodeString(value)
}

var errNotFound = errors.New("not found")

type githubReleaseClient struct {
	apiBase string
	token   string
	http    *http.Client
}

func (c *githubReleaseClient) githubAppInstallationToken(repo string) (string, error) {
	jwt, err := githubAppJWT()
	if err != nil {
		return "", err
	}

	owner, repoName, err := splitRepo(repo)
	if err != nil {
		return "", err
	}

	var installation repoInstallation
	if err := c.appRequest(http.MethodGet, fmt.Sprintf("/repos/%s/%s/installation", owner, repoName), nil, &installation, jwt); err != nil {
		return "", fmt.Errorf("find GitHub App installation for %s: %w", repo, err)
	}
	if installation.ID == 0 {
		return "", fmt.Errorf("GitHub App installation for %s did not include an installation ID", repo)
	}

	payload := map[string]any{
		"repositories": []string{repoName},
		"permissions": map[string]string{
			"contents": "write",
		},
	}
	var token installationToken
	if err := c.appRequest(http.MethodPost, fmt.Sprintf("/app/installations/%d/access_tokens", installation.ID), payload, &token, jwt); err != nil {
		return "", fmt.Errorf("mint GitHub App installation token: %w", err)
	}
	if token.Token == "" {
		return "", errors.New("GitHub App installation token response did not include a token")
	}
	fmt.Printf("minted GitHub App installation token for %s (expires %s)\n", repo, token.ExpiresAt)
	return token.Token, nil
}

func splitRepo(repo string) (string, string, error) {
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid GitHub repository %q; expected owner/repo", repo)
	}
	return parts[0], parts[1], nil
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
	c.addHeaders(req, c.token)
	if ct := mime.TypeByExtension(filepath.Ext(path)); ct != "" {
		req.Header.Set("Content-Type", ct)
	} else {
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	return c.do(req, nil)
}

func (c *githubReleaseClient) request(method, path string, payload any, into any) error {
	return c.requestWithBearer(method, path, payload, into, c.token)
}

func (c *githubReleaseClient) appRequest(method, path string, payload any, into any, jwt string) error {
	return c.requestWithBearer(method, path, payload, into, jwt)
}

func (c *githubReleaseClient) requestWithBearer(method, path string, payload any, into any, bearer string) error {
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
	c.addHeaders(req, bearer)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.do(req, into)
}

func (c *githubReleaseClient) addHeaders(req *http.Request, bearer string) {
	req.Header.Set("Authorization", "Bearer "+bearer)
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
