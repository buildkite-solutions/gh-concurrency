package main

import (
	"context"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"
)

func envToken() string {
	for _, key := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
		if token := strings.TrimSpace(os.Getenv(key)); token != "" {
			return token
		}
	}
	return ""
}

func resolveToken(cfg config) (string, error) {
	if cfg.token != "" {
		return cfg.token, nil
	}
	if cfg.provider == circleCIProvider {
		return "", errors.New("no token. Set CIRCLECI_TOKEN/CIRCLE_TOKEN or pass --token")
	}
	token, err := tokenFromGH(cfg.baseURL)
	if err == nil && token != "" {
		return token, nil
	}
	return "", errors.New("no token. Set GITHUB_TOKEN/GH_TOKEN, pass --token, or run `gh auth login` before using the gh extension")
}

func tokenFromGH(baseURL string) (string, error) {
	if _, err := exec.LookPath("gh"); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	args := []string{"auth", "token"}
	if host := ghHostForBaseURL(baseURL); host != "" {
		args = append(args, "--hostname", host)
	}
	cmd := exec.CommandContext(ctx, "gh", args...)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func ghHostForBaseURL(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" || u.Host == "api.github.com" {
		return ""
	}
	return u.Host
}
