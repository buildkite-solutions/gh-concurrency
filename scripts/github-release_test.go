package main

import (
	cryptorand "crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestParseRepoURL(t *testing.T) {
	tests := map[string]string{
		"git@github.com:buildkite-solutions/gh-concurrency.git":       "buildkite-solutions/gh-concurrency",
		"https://github.com/buildkite-solutions/gh-concurrency.git":   "buildkite-solutions/gh-concurrency",
		"ssh://git@github.com/buildkite-solutions/gh-concurrency.git": "buildkite-solutions/gh-concurrency",
		"buildkite-solutions/gh-concurrency":                          "buildkite-solutions/gh-concurrency",
		"":                                                            "",
	}
	for raw, want := range tests {
		if got := parseRepoURL(raw); got != want {
			t.Fatalf("parseRepoURL(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestSplitRepo(t *testing.T) {
	owner, name, err := splitRepo("buildkite-solutions/gh-concurrency")
	if err != nil {
		t.Fatal(err)
	}
	if owner != "buildkite-solutions" || name != "gh-concurrency" {
		t.Fatalf("splitRepo returned %q/%q", owner, name)
	}
	if _, _, err := splitRepo("nope"); err == nil {
		t.Fatal("splitRepo(nope) succeeded, want error")
	}
}

func TestCheckAuthDoesNotRequireVersionOrReleaseAssets(t *testing.T) {
	tokenRequests := 0
	withFakeGitHubHTTPClient(t, func(r *http.Request) (int, string) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			t.Fatalf("request missing bearer auth header")
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/buildkite-solutions/gh-concurrency/installation":
			return http.StatusOK, `{"id":123}`
		case r.Method == http.MethodPost && r.URL.Path == "/app/installations/123/access_tokens":
			tokenRequests++
			return http.StatusOK, `{"token":"installation-token","expires_at":"2030-01-01T00:00:00Z"}`
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
			return http.StatusNotFound, `{"message":"not found"}`
		}
	})

	setGitHubAppEnv(t)
	t.Setenv("VERSION", "")
	t.Setenv("RELEASE_TAG", "")
	t.Setenv("BUILDKITE_TAG", "")

	if err := run([]string{"--check-auth"}); err != nil {
		t.Fatal(err)
	}
	if tokenRequests != 1 {
		t.Fatalf("installation token requests = %d, want 1", tokenRequests)
	}
}

func TestCheckAuthWrapsGitHubAppAuthFailure(t *testing.T) {
	withFakeGitHubHTTPClient(t, func(r *http.Request) (int, string) {
		return http.StatusUnauthorized, `{"message":"A JSON web token could not be decoded"}`
	})

	setGitHubAppEnv(t)

	err := run([]string{"--check-auth"})
	if err == nil {
		t.Fatal("run(--check-auth) succeeded, want error")
	}
	if !strings.Contains(err.Error(), "GitHub App auth is invalid") {
		t.Fatalf("error = %q, want GitHub App auth is invalid", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func withFakeGitHubHTTPClient(t *testing.T, handler func(*http.Request) (int, string)) {
	t.Helper()

	previous := newGitHubHTTPClient
	newGitHubHTTPClient = func() *http.Client {
		return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			status, body := handler(r)
			return &http.Response{
				StatusCode: status,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(body)),
				Request:    r,
			}, nil
		})}
	}
	t.Cleanup(func() {
		newGitHubHTTPClient = previous
	})
}

func setGitHubAppEnv(t *testing.T) {
	t.Helper()

	key, err := rsa.GenerateKey(cryptorand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})

	t.Setenv("GITHUB_API_URL", "https://api.github.test")
	t.Setenv("GITHUB_REPOSITORY", "buildkite-solutions/gh-concurrency")
	t.Setenv("GITHUB_APP_CLIENT_ID", "Iv1.testclientid")
	t.Setenv("GITHUB_APP_PRIVATE_KEY_B64", base64.StdEncoding.EncodeToString(pemBytes))
}
