package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

const defaultBaseURL = "https://api.github.com"

var osMultiplier = map[string]int{
	"linux":   1,
	"windows": 2,
	"macos":   10,
}

type config struct {
	repos                []string
	orgs                 []string
	repoFiles            []string
	orgFiles             []string
	repoType             string
	since                string
	until                string
	baseURL              string
	token                string
	format               string
	maxRetries           int
	requestDelayMS       int
	apiWorkers           int
	includeArchived      bool
	includeInProgress    bool
	jobFilter            string
	branch               string
	event                string
	excludePullRequests  bool
	top                  int
	estimate             bool
	estimateMaxRequests  int
	estimateMinRemaining int
	estimateSampleRuns   int
	estimateIterations   int
	estimateConfidence   int
	estimateSeed         int64
	verbose              bool
	debug                bool
	showVer              bool
}

type stringList []string

func (s *stringList) Set(value string) error {
	*s = append(*s, value)
	return nil
}

func (s *stringList) String() string {
	return strings.Join(*s, ",")
}

func parseArgs(argv []string, stderr io.Writer) (config, error) {
	var cfg config
	var repos stringList
	var orgs stringList
	var repoFiles stringList
	var orgFiles stringList
	baseURL := os.Getenv("GITHUB_API_URL")
	if baseURL == "" {
		baseURL = defaultBaseURL
	}

	fs := flag.NewFlagSet("gh-concurrency", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Var(&repos, "repo", "repository in OWNER/NAME form (repeatable; repos pool into one profile)")
	fs.Var(&orgs, "org", "GitHub organization whose accessible repositories should be pooled (repeatable)")
	fs.Var(&repoFiles, "repo-file", "file containing OWNER/NAME repositories, one per line or comma/space separated (repeatable)")
	fs.Var(&orgFiles, "org-file", "file containing organization names, one per line or comma/space separated (repeatable)")
	fs.StringVar(&cfg.repoType, "repo-type", "all", "organization repository type: all, public, private, forks, sources, or member")
	fs.StringVar(&cfg.since, "since", "", "lower bound on workflow-run creation date (YYYY-MM-DD)")
	fs.StringVar(&cfg.until, "until", "", "optional upper bound on workflow-run creation date (YYYY-MM-DD)")
	fs.StringVar(&cfg.baseURL, "base-url", baseURL, "API base URL. GHES: https://HOST/api/v3 (env: GITHUB_API_URL)")
	fs.StringVar(&cfg.token, "token", envToken(), "GitHub token (default: GITHUB_TOKEN or GH_TOKEN; gh auth fallback when available)")
	fs.StringVar(&cfg.format, "format", "text", "output format: text or json")
	fs.IntVar(&cfg.maxRetries, "max-retries", 6, "maximum HTTP retry attempts")
	fs.IntVar(&cfg.requestDelayMS, "request-delay-ms", 100, "minimum delay before each GitHub API request; helps avoid secondary rate limits")
	fs.IntVar(&cfg.apiWorkers, "api-workers", 4, "maximum concurrent GitHub API requests")
	fs.BoolVar(&cfg.includeArchived, "include-archived", false, "include archived repositories instead of skipping them during target resolution")
	fs.BoolVar(&cfg.includeInProgress, "include-in-progress", false, "include non-completed workflow runs instead of querying only completed runs")
	fs.StringVar(&cfg.jobFilter, "job-filter", "all", "workflow-run job filter: all or latest")
	fs.StringVar(&cfg.branch, "branch", "", "only include workflow runs for this branch")
	fs.StringVar(&cfg.event, "event", "", "only include workflow runs for this event, such as push or pull_request")
	fs.BoolVar(&cfg.excludePullRequests, "exclude-pull-requests", false, "omit pull request workflow runs")
	fs.IntVar(&cfg.top, "top", 10, "number of top repositories, workflows, and jobs to show")
	fs.BoolVar(&cfg.estimate, "estimate", false, "use sampled workflow-run jobs and simulation to estimate concurrency faster")
	fs.IntVar(&cfg.estimateMaxRequests, "estimate-max-requests", 1000, "maximum GitHub API requests to spend in --estimate mode after target resolution")
	fs.IntVar(&cfg.estimateMinRemaining, "estimate-min-remaining", 500, "stop --estimate mode before the primary rate-limit remaining count reaches this value")
	fs.IntVar(&cfg.estimateSampleRuns, "estimate-sample-runs", 250, "target workflow runs to sample in --estimate mode")
	fs.IntVar(&cfg.estimateIterations, "estimate-iterations", 1000, "Monte Carlo iterations for --estimate mode")
	fs.IntVar(&cfg.estimateConfidence, "estimate-confidence", 90, "confidence interval percentage for --estimate mode")
	fs.Int64Var(&cfg.estimateSeed, "estimate-seed", 0, "random seed for --estimate mode; default is generated and printed")
	fs.BoolVar(&cfg.verbose, "verbose", false, "progress and rate-limit logging to stderr")
	fs.BoolVar(&cfg.verbose, "v", false, "alias for --verbose")
	fs.BoolVar(&cfg.debug, "debug", false, "HTTP request and pagination diagnostics to stderr; implies --verbose")
	fs.BoolVar(&cfg.debug, "d", false, "alias for --debug")
	fs.BoolVar(&cfg.showVer, "version", false, "print version and exit")

	if err := fs.Parse(argv); err != nil {
		return cfg, err
	}
	cfg.repos = repos
	cfg.orgs = orgs
	cfg.repoFiles = repoFiles
	cfg.orgFiles = orgFiles
	cfg.baseURL = strings.TrimRight(cfg.baseURL, "/")
	if cfg.maxRetries < 1 {
		cfg.maxRetries = 1
	}
	if cfg.requestDelayMS < 0 {
		cfg.requestDelayMS = 0
	}
	if cfg.apiWorkers < 1 {
		cfg.apiWorkers = 1
	}
	if cfg.apiWorkers > 32 {
		cfg.apiWorkers = 32
	}
	if cfg.top < 0 {
		cfg.top = 0
	}
	if cfg.estimateMaxRequests < 1 {
		cfg.estimateMaxRequests = 1
	}
	if cfg.estimateMinRemaining < 0 {
		cfg.estimateMinRemaining = 0
	}
	if cfg.estimateSampleRuns < 1 {
		cfg.estimateSampleRuns = 1
	}
	if cfg.estimateIterations < 1 {
		cfg.estimateIterations = 1
	}
	if cfg.estimateConfidence < 1 {
		cfg.estimateConfidence = 1
	}
	if cfg.estimateConfidence > 99 {
		cfg.estimateConfidence = 99
	}
	if cfg.debug {
		cfg.verbose = true
	}
	return cfg, nil
}

func validateConfig(cfg config) error {
	if cfg.showVer {
		return nil
	}
	if len(cfg.repos) == 0 && len(cfg.orgs) == 0 && len(cfg.repoFiles) == 0 && len(cfg.orgFiles) == 0 {
		return errors.New("at least one --repo, --org, --repo-file, or --org-file is required")
	}
	for _, repo := range cfg.repos {
		if err := validateRepo(repo); err != nil {
			return err
		}
	}
	for _, org := range cfg.orgs {
		if err := validateOrg(org); err != nil {
			return err
		}
	}
	switch cfg.repoType {
	case "all", "public", "private", "forks", "sources", "member":
	default:
		return fmt.Errorf("invalid --repo-type %q; expected all, public, private, forks, sources, or member", cfg.repoType)
	}
	for _, path := range append(append([]string{}, cfg.repoFiles...), cfg.orgFiles...) {
		if strings.TrimSpace(path) == "" {
			return errors.New("target file path cannot be empty")
		}
	}
	if cfg.since == "" {
		return errors.New("--since YYYY-MM-DD is required")
	}
	if _, err := time.Parse("2006-01-02", cfg.since); err != nil {
		return fmt.Errorf("invalid --since %q; expected YYYY-MM-DD", cfg.since)
	}
	if cfg.until != "" {
		if _, err := time.Parse("2006-01-02", cfg.until); err != nil {
			return fmt.Errorf("invalid --until %q; expected YYYY-MM-DD", cfg.until)
		}
	}
	if cfg.format != "text" && cfg.format != "json" {
		return fmt.Errorf("invalid --format %q; expected text or json", cfg.format)
	}
	if cfg.jobFilter != "all" && cfg.jobFilter != "latest" {
		return fmt.Errorf("invalid --job-filter %q; expected all or latest", cfg.jobFilter)
	}
	u, err := url.Parse(cfg.baseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("invalid --base-url %q", cfg.baseURL)
	}
	return nil
}

func validateRepo(repo string) error {
	owner, name, err := splitRepoName(repo)
	if err != nil {
		return err
	}
	if owner == "" || name == "" {
		return fmt.Errorf("invalid repo %q; expected OWNER/NAME", repo)
	}
	return nil
}

func validateOrg(org string) error {
	org = strings.TrimSpace(org)
	if org == "" || strings.Contains(org, "/") || strings.ContainsAny(org, " \t\r\n") {
		return fmt.Errorf("invalid org %q; expected organization slug", org)
	}
	return nil
}

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
	var repos []string
	var skipped []skippedRepository
	var directRepos []string
	for _, repo := range cfg.repos {
		if err := validateRepo(repo); err != nil {
			return nil, nil, err
		}
		directRepos = append(directRepos, repo)
	}

	for _, path := range cfg.repoFiles {
		fileRepos, err := readListFile(path)
		if err != nil {
			return nil, nil, fmt.Errorf("read --repo-file %s: %w", path, err)
		}
		for _, repo := range fileRepos {
			if err := validateRepo(repo); err != nil {
				return nil, nil, err
			}
			directRepos = append(directRepos, repo)
		}
	}
	filteredDirectRepos, directSkipped, err := resolveDirectRepos(client, directRepos, cfg.includeArchived, stderr)
	if err != nil {
		return nil, nil, err
	}
	skipped = append(skipped, directSkipped...)
	repos = append(repos, filteredDirectRepos...)

	orgs := append([]string{}, cfg.orgs...)
	for _, path := range cfg.orgFiles {
		fileOrgs, err := readListFile(path)
		if err != nil {
			return nil, nil, fmt.Errorf("read --org-file %s: %w", path, err)
		}
		for _, org := range fileOrgs {
			if err := validateOrg(org); err != nil {
				return nil, nil, err
			}
			orgs = append(orgs, org)
		}
	}

	for _, org := range uniqueStrings(orgs) {
		client.logf("listing repositories for org %s", org)
		orgRepos, orgSkipped, err := listOrgRepos(client, org, cfg.repoType, cfg.includeArchived)
		if err != nil {
			var nf notFoundError
			if errors.As(err, &nf) {
				fmt.Fprintf(stderr, "warning: org %s not found or no repository access; skipping.\n", org)
				continue
			}
			return nil, nil, err
		}
		repos = append(repos, orgRepos...)
		skipped = append(skipped, orgSkipped...)
	}

	repos = uniqueRepos(repos)
	sort.Slice(repos, func(i, j int) bool {
		return strings.ToLower(repos[i]) < strings.ToLower(repos[j])
	})
	sortSkippedRepositories(skipped)
	return repos, skipped, nil
}

func resolveDirectRepos(client *githubClient, repos []string, includeArchived bool, stderr io.Writer) ([]string, []skippedRepository, error) {
	if includeArchived {
		return repos, nil, nil
	}
	var out []string
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
		out = append(out, info.FullName)
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
	FullName string `json:"full_name"`
	Disabled bool   `json:"disabled"`
	Archived bool   `json:"archived"`
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
	if err := validateOrg(org); err != nil {
		return nil, nil, err
	}
	params := url.Values{
		"type":      []string{repoType},
		"sort":      []string{"full_name"},
		"direction": []string{"asc"},
	}
	var repos []string
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
		repos = append(repos, repo.FullName)
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return repos, skipped, nil
}

func envToken() string {
	for _, key := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
		if token := strings.TrimSpace(os.Getenv(key)); token != "" {
			return token
		}
	}
	return ""
}

func resolveToken(explicitToken, baseURL string) (string, error) {
	if explicitToken != "" {
		return explicitToken, nil
	}
	token, err := tokenFromGH(baseURL)
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

type githubClient struct {
	baseURL          string
	token            string
	maxRetries       int
	timeout          time.Duration
	requestDelay     time.Duration
	apiSlots         chan struct{}
	throttleMu       sync.Mutex
	lastRequestStart time.Time
	statsMu          sync.Mutex
	stats            requestStats
	budgetMu         sync.Mutex
	budget           *requestBudget
	verbose          bool
	debug            bool
	logWriter        io.Writer
	httpClient       *http.Client
	sleep            func(time.Duration)
	now              func() time.Time
}

func newGitHubClient(baseURL, token string, maxRetries int, verbose bool) *githubClient {
	return &githubClient{
		baseURL:    strings.TrimRight(baseURL, "/"),
		token:      token,
		maxRetries: maxRetries,
		timeout:    30 * time.Second,
		apiSlots:   make(chan struct{}, 1),
		verbose:    verbose,
		logWriter:  os.Stderr,
		httpClient: &http.Client{Timeout: 30 * time.Second},
		sleep:      time.Sleep,
		now:        time.Now,
	}
}

type requestStats struct {
	Requests              int     `json:"api_requests"`
	Retries               int     `json:"retries"`
	RateLimitSleeps       int     `json:"rate_limit_sleeps"`
	RateLimitSleepSeconds float64 `json:"rate_limit_sleep_seconds"`
}

type requestBudget struct {
	MaxRequests   int
	MinRemaining  int
	StartRequests int
	LastRemaining *int
	StopReason    string
}

type requestBudgetStopError struct {
	Reason string
}

func (e requestBudgetStopError) Error() string {
	if e.Reason == "" {
		return "API request budget stop"
	}
	return e.Reason
}

func (c *githubClient) setAPIWorkers(workers int) {
	if workers < 1 {
		workers = 1
	}
	if workers > 32 {
		workers = 32
	}
	c.apiSlots = make(chan struct{}, workers)
}

func (c *githubClient) statsSnapshot() requestStats {
	c.statsMu.Lock()
	defer c.statsMu.Unlock()
	out := c.stats
	out.RateLimitSleepSeconds = math.Round(out.RateLimitSleepSeconds*1000) / 1000
	return out
}

func (c *githubClient) enableRequestBudget(maxRequests, minRemaining int) {
	if maxRequests < 1 {
		maxRequests = 1
	}
	if minRemaining < 0 {
		minRemaining = 0
	}
	startRequests := c.statsSnapshot().Requests
	c.budgetMu.Lock()
	c.budget = &requestBudget{MaxRequests: maxRequests, MinRemaining: minRemaining, StartRequests: startRequests}
	c.budgetMu.Unlock()
}

func (c *githubClient) disableRequestBudget() {
	c.budgetMu.Lock()
	c.budget = nil
	c.budgetMu.Unlock()
}

func (c *githubClient) requestBudgetStopReason() string {
	c.budgetMu.Lock()
	defer c.budgetMu.Unlock()
	if c.budget == nil {
		return ""
	}
	return c.budget.StopReason
}

func (c *githubClient) checkRequestBudget() error {
	c.budgetMu.Lock()
	budget := c.budget
	c.budgetMu.Unlock()
	if budget == nil {
		return nil
	}
	stats := c.statsSnapshot()
	c.budgetMu.Lock()
	defer c.budgetMu.Unlock()
	if c.budget == nil {
		return nil
	}
	spent := stats.Requests - c.budget.StartRequests
	if c.budget.MaxRequests > 0 && spent >= c.budget.MaxRequests {
		c.budget.StopReason = fmt.Sprintf("estimate API request budget reached (%d requests)", c.budget.MaxRequests)
		return requestBudgetStopError{Reason: c.budget.StopReason}
	}
	if c.budget.LastRemaining != nil && *c.budget.LastRemaining <= c.budget.MinRemaining {
		c.budget.StopReason = fmt.Sprintf("estimate stopped with GitHub rate-limit remaining=%d (minimum %d)", *c.budget.LastRemaining, c.budget.MinRemaining)
		return requestBudgetStopError{Reason: c.budget.StopReason}
	}
	return nil
}

func (c *githubClient) updateRateLimitRemaining(value string) {
	if value == "" {
		return
	}
	remaining, err := strconv.Atoi(value)
	if err != nil {
		return
	}
	c.budgetMu.Lock()
	defer c.budgetMu.Unlock()
	if c.budget != nil {
		c.budget.LastRemaining = &remaining
	}
}

func (c *githubClient) budgetModeEnabled() bool {
	c.budgetMu.Lock()
	defer c.budgetMu.Unlock()
	return c.budget != nil
}

func (c *githubClient) budgetStop(reason string) error {
	c.budgetMu.Lock()
	if c.budget != nil {
		c.budget.StopReason = reason
	}
	c.budgetMu.Unlock()
	return requestBudgetStopError{Reason: reason}
}

func (c *githubClient) recordRequest() {
	c.statsMu.Lock()
	c.stats.Requests++
	c.statsMu.Unlock()
}

func (c *githubClient) recordRetry() {
	c.statsMu.Lock()
	c.stats.Retries++
	c.statsMu.Unlock()
}

func (c *githubClient) recordRateLimitSleep(delay time.Duration) {
	c.statsMu.Lock()
	c.stats.RateLimitSleeps++
	c.stats.RateLimitSleepSeconds += delay.Seconds()
	c.statsMu.Unlock()
}

func (c *githubClient) acquireAPISlot() func() {
	if c.apiSlots == nil {
		return func() {}
	}
	c.apiSlots <- struct{}{}
	return func() {
		<-c.apiSlots
	}
}

func (c *githubClient) waitForRequestStart() {
	c.throttleMu.Lock()
	defer c.throttleMu.Unlock()
	if c.requestDelay > 0 && !c.lastRequestStart.IsZero() {
		wait := c.lastRequestStart.Add(c.requestDelay).Sub(c.currentTime())
		if wait > 0 {
			c.sleep(wait)
		}
	}
	c.lastRequestStart = c.currentTime()
}

func (c *githubClient) pauseRequests(delay time.Duration, reason string) {
	if delay < time.Second {
		delay = time.Second
	}
	c.throttleMu.Lock()
	defer c.throttleMu.Unlock()
	c.recordRateLimitSleep(delay)
	c.logf("%s: pausing API requests for %.0fs", reason, delay.Seconds())
	c.sleep(delay)
	c.lastRequestStart = c.currentTime()
}

func (c *githubClient) currentTime() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

func (c *githubClient) logf(format string, args ...interface{}) {
	if c.verbose || c.debug {
		fmt.Fprintf(c.logOutput(), "[gh-concurrency] "+format+"\n", args...)
	}
}

func (c *githubClient) debugf(format string, args ...interface{}) {
	if c.debug {
		fmt.Fprintf(c.logOutput(), "[gh-concurrency debug] "+format+"\n", args...)
	}
}

func (c *githubClient) logOutput() io.Writer {
	if c.logWriter != nil {
		return c.logWriter
	}
	return io.Discard
}

func (c *githubClient) request(rawURL string) ([]byte, string, error) {
	var lastErr error
	for attempt := 0; attempt < c.maxRetries; attempt++ {
		if err := c.checkRequestBudget(); err != nil {
			return nil, "", err
		}
		if attempt > 0 {
			c.recordRetry()
		}
		req, err := http.NewRequest(http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, "", err
		}
		req.Header.Set("Authorization", "Bearer "+c.token)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		req.Header.Set("User-Agent", "gh-concurrency/"+version)

		c.debugf("GET %s (attempt %d/%d)", requestURLForLog(rawURL), attempt+1, c.maxRetries)
		release := c.acquireAPISlot()
		c.waitForRequestStart()
		c.recordRequest()
		resp, err := c.httpClient.Do(req)
		if err != nil {
			release()
			lastErr = err
			c.logf("network error: %v; retrying", err)
			if attempt == c.maxRetries-1 {
				break
			}
			c.backoff(attempt)
			continue
		}
		c.debugResponse(rawURL, resp)
		c.updateRateLimitRemaining(resp.Header.Get("X-RateLimit-Remaining"))

		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		release()
		if readErr != nil {
			lastErr = readErr
			if attempt == c.maxRetries-1 {
				break
			}
			c.backoff(attempt)
			continue
		}

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			if remaining := resp.Header.Get("X-RateLimit-Remaining"); remaining != "" {
				if n, err := strconv.Atoi(remaining); err == nil && n <= 1 {
					if reset := resp.Header.Get("X-RateLimit-Reset"); reset != "" {
						if epoch, err := strconv.ParseInt(reset, 10, 64); err == nil {
							if c.budgetModeEnabled() {
								return body, resp.Header.Get("Link"), nil
							}
							c.sleepUntil(epoch, "primary rate limit")
						}
					}
				}
			}
			return body, resp.Header.Get("Link"), nil
		}

		err = c.httpError(resp, body)
		switch {
		case resp.StatusCode == http.StatusNotFound:
			return nil, "", notFoundError{URL: rawURL}
		case resp.StatusCode == http.StatusUnauthorized:
			return nil, "", authError{}
		case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests:
			if retryAfter := resp.Header.Get("Retry-After"); retryAfter != "" {
				delay, parseErr := strconv.Atoi(retryAfter)
				if parseErr == nil && attempt < c.maxRetries-1 {
					if c.budgetModeEnabled() {
						return nil, "", c.budgetStop(fmt.Sprintf("estimate stopped on Retry-After=%ds", delay))
					}
					c.logf("%d: honoring Retry-After=%ds", resp.StatusCode, delay)
					c.pauseRequests(time.Duration(delay+1)*time.Second, "Retry-After")
					continue
				}
			}
			if resp.Header.Get("X-RateLimit-Remaining") == "0" {
				if reset := resp.Header.Get("X-RateLimit-Reset"); reset != "" && attempt < c.maxRetries-1 {
					if epoch, parseErr := strconv.ParseInt(reset, 10, 64); parseErr == nil {
						if c.budgetModeEnabled() {
							return nil, "", c.budgetStop("estimate stopped on primary rate limit")
						}
						c.sleepUntil(epoch, "primary rate limit")
						continue
					}
				}
			}
			if isSecondaryRateLimit(resp.StatusCode, body) && attempt < c.maxRetries-1 {
				if c.budgetModeEnabled() {
					return nil, "", c.budgetStop("estimate stopped on secondary rate limit")
				}
				c.secondaryBackoff(attempt)
				continue
			}
			return nil, "", err
		case resp.StatusCode >= 500:
			lastErr = err
			if attempt == c.maxRetries-1 {
				break
			}
			c.backoff(attempt)
			continue
		default:
			return nil, "", err
		}
	}
	if lastErr == nil {
		lastErr = errors.New("unknown request failure")
	}
	return nil, "", fmt.Errorf("exhausted %d retries for %s: %w", c.maxRetries, rawURL, lastErr)
}

func (c *githubClient) debugResponse(rawURL string, resp *http.Response) {
	if !c.debug {
		return
	}
	var details []string
	if remaining := resp.Header.Get("X-RateLimit-Remaining"); remaining != "" {
		details = append(details, "remaining="+remaining)
	}
	if reset := resp.Header.Get("X-RateLimit-Reset"); reset != "" {
		details = append(details, "reset="+formatRateReset(reset))
	}
	if retryAfter := resp.Header.Get("Retry-After"); retryAfter != "" {
		details = append(details, "retry-after="+retryAfter+"s")
	}
	suffix := ""
	if len(details) > 0 {
		suffix = " (" + strings.Join(details, ", ") + ")"
	}
	c.debugf("%s -> %s%s", requestURLForLog(rawURL), resp.Status, suffix)
}

func requestURLForLog(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	return u.String()
}

func formatRateReset(value string) string {
	epoch, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return value
	}
	return time.Unix(epoch, 0).UTC().Format(time.RFC3339)
}

func (c *githubClient) httpError(resp *http.Response, body []byte) error {
	msg := strings.TrimSpace(string(bytes.TrimSpace(body)))
	if len(msg) > 500 {
		msg = msg[:500] + "..."
	}
	if msg == "" {
		msg = resp.Status
	}
	return fmt.Errorf("GitHub API %s: %s", resp.Status, msg)
}

func (c *githubClient) backoff(attempt int) {
	base := time.Duration(1<<uint(min(attempt, 6))) * time.Second
	jitter := time.Duration(rand.Intn(1000)) * time.Millisecond
	delay := base + jitter
	c.logf("backing off %.1fs (attempt %d)", delay.Seconds(), attempt+1)
	c.sleep(delay)
}

func (c *githubClient) secondaryBackoff(attempt int) {
	base := time.Duration(1<<uint(min(attempt, 5))) * time.Minute
	jitter := time.Duration(rand.Intn(5000)) * time.Millisecond
	delay := base + jitter
	c.logf("secondary rate limit: backing off %.0fs (attempt %d)", delay.Seconds(), attempt+1)
	c.pauseRequests(delay, "secondary rate limit")
}

func isSecondaryRateLimit(status int, body []byte) bool {
	if status == http.StatusTooManyRequests {
		return true
	}
	msg := strings.ToLower(string(body))
	return strings.Contains(msg, "secondary rate limit") ||
		strings.Contains(msg, "abuse detection") ||
		strings.Contains(msg, "too many requests") ||
		strings.Contains(msg, "retry your request again later")
}

func (c *githubClient) sleepUntil(resetEpoch int64, reason string) {
	wait := time.Unix(resetEpoch, 0).Sub(c.currentTime()) + time.Second
	if wait < 0 {
		wait = time.Second
	}
	c.pauseRequests(wait, reason)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

type progressReporter struct {
	mu        sync.Mutex
	out       io.Writer
	enabled   bool
	total     int
	started   int
	done      int
	totalJobs int
	startedAt time.Time
}

func newProgressReporter(out io.Writer, enabled bool, total int) *progressReporter {
	return &progressReporter{
		out:       out,
		enabled:   enabled && out != nil,
		total:     total,
		startedAt: time.Now(),
	}
}

func (p *progressReporter) Begin() {
	if !p.enabled {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	fmt.Fprintf(p.out, "[gh-concurrency] repositories queued: %d\n", p.total)
	fmt.Fprintf(p.out, "[gh-concurrency] progress: %s 0/%d\n", progressBar(0, p.total, 24), p.total)
}

func (p *progressReporter) Start(repo string) {
	if !p.enabled {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.started++
	fmt.Fprintf(p.out, "[gh-concurrency] examining repo %d/%d: %s\n", p.started, p.total, repo)
}

func (p *progressReporter) Done(repo string, jobs int) {
	if !p.enabled {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.done++
	p.totalJobs += jobs
	fmt.Fprintf(p.out, "[gh-concurrency] progress: %s %d/%d done: %s (%d jobs, %d total)\n",
		progressBar(p.done, p.total, 24), p.done, p.total, repo, jobs, p.totalJobs)
}

func (p *progressReporter) Skip(repo string) {
	if !p.enabled {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.done++
	fmt.Fprintf(p.out, "[gh-concurrency] progress: %s %d/%d skipped: %s (%d total jobs)\n",
		progressBar(p.done, p.total, 24), p.done, p.total, repo, p.totalJobs)
}

func (p *progressReporter) Complete() {
	if !p.enabled {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	elapsed := time.Since(p.startedAt).Round(time.Second)
	fmt.Fprintf(p.out, "[gh-concurrency] complete: %d/%d repositories, %d jobs, %s elapsed\n",
		p.done, p.total, p.totalJobs, elapsed)
}

func progressBar(done, total, width int) string {
	if width < 1 {
		width = 1
	}
	if total < 1 {
		total = 1
	}
	if done < 0 {
		done = 0
	}
	if done > total {
		done = total
	}
	filled := int(math.Round(float64(done) / float64(total) * float64(width)))
	if filled > width {
		filled = width
	}
	return "[" + strings.Repeat("#", filled) + strings.Repeat("-", width-filled) + "]"
}

func (c *githubClient) paginate(path string, params url.Values, itemsKey string, handle func(json.RawMessage) error) error {
	params = cloneValues(params)
	params.Set("per_page", "100")
	nextURL := c.baseURL + path + "?" + params.Encode()
	page := 1
	for nextURL != "" {
		currentURL := nextURL
		body, link, err := c.request(nextURL)
		if err != nil {
			return err
		}
		items, err := extractItems(body, itemsKey)
		if err != nil {
			return err
		}
		c.debugf("page %d %s returned %d items", page, requestURLForLog(currentURL), len(items))
		for _, item := range items {
			if err := handle(item); err != nil {
				return err
			}
		}
		nextURL = nextLink(link)
		if nextURL != "" {
			c.debugf("next page: %s", requestURLForLog(nextURL))
		}
		page++
	}
	return nil
}

func cloneValues(values url.Values) url.Values {
	out := url.Values{}
	for key, vals := range values {
		for _, value := range vals {
			out.Add(key, value)
		}
	}
	return out
}

func extractItems(body []byte, itemsKey string) ([]json.RawMessage, error) {
	if itemsKey == "" {
		var items []json.RawMessage
		return items, json.Unmarshal(body, &items)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, err
	}
	raw, ok := obj[itemsKey]
	if !ok || bytes.Equal(raw, []byte("null")) {
		return nil, nil
	}
	var items []json.RawMessage
	return items, json.Unmarshal(raw, &items)
}

func nextLink(linkHeader string) string {
	for _, part := range strings.Split(linkHeader, ",") {
		segs := strings.Split(part, ";")
		if len(segs) < 2 {
			continue
		}
		rawURL := strings.Trim(strings.TrimSpace(segs[0]), "<>")
		for _, seg := range segs[1:] {
			if strings.TrimSpace(seg) == `rel="next"` {
				return rawURL
			}
		}
	}
	return ""
}

type notFoundError struct {
	URL string
}

func (e notFoundError) Error() string {
	return "not found: " + e.URL
}

type authError struct{}

func (authError) Error() string {
	return "unauthorized"
}

type workflowRun struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	WorkflowID   int64  `json:"workflow_id"`
	Event        string `json:"event"`
	HeadBranch   string `json:"head_branch"`
	Status       string `json:"status"`
	Conclusion   string `json:"conclusion"`
	CreatedAt    string `json:"created_at"`
	RunStartedAt string `json:"run_started_at"`
	UpdatedAt    string `json:"updated_at"`
	Repo         string `json:"-"`
}

type workflowJob struct {
	Name            string   `json:"name"`
	Conclusion      string   `json:"conclusion"`
	WorkflowName    string   `json:"workflow_name"`
	StartedAt       string   `json:"started_at"`
	CompletedAt     string   `json:"completed_at"`
	CreatedAt       string   `json:"created_at"`
	Labels          []string `json:"labels"`
	RunnerName      string   `json:"runner_name"`
	RunnerGroupName string   `json:"runner_group_name"`
}

type record struct {
	Repo            string
	WorkflowName    string
	JobName         string
	Conclusion      string
	Start           time.Time
	End             time.Time
	QueueSeconds    *float64
	OS              string
	SelfHosted      bool
	Labels          []string
	RunnerName      string
	RunnerGroupName string
}

type collectOptions struct {
	Since               string
	Until               string
	IncludeInProgress   bool
	JobFilter           string
	Branch              string
	Event               string
	ExcludePullRequests bool
	APIWorkers          int
}

type repoScanResult struct {
	Repo         string
	Records      []record
	WorkflowRuns int
	WorkflowJobs int
	JobsUsed     int
}

func collectJobs(client *githubClient, repo string, opts collectOptions) (repoScanResult, error) {
	result := repoScanResult{Repo: repo}
	created := createdQuery(opts.Since, opts.Until)
	repoPath, err := repoAPIPath(repo)
	if err != nil {
		return result, err
	}

	client.logf("%s: listing workflow runs created %s", repo, created)
	params := url.Values{"created": []string{created}}
	if !opts.IncludeInProgress {
		params.Set("status", "completed")
	}
	if opts.Branch != "" {
		params.Set("branch", opts.Branch)
	}
	if opts.Event != "" {
		params.Set("event", opts.Event)
	}
	if opts.ExcludePullRequests {
		params.Set("exclude_pull_requests", "true")
	}
	var runs []workflowRun
	err = client.paginate(repoPath+"/actions/runs", params, "workflow_runs", func(raw json.RawMessage) error {
		var run workflowRun
		if err := json.Unmarshal(raw, &run); err != nil {
			return err
		}
		runs = append(runs, run)
		return nil
	})
	if err != nil {
		return result, err
	}
	result.WorkflowRuns = len(runs)
	if len(runs) == 0 {
		client.logf("%s: 0 workflow runs, 0 workflow jobs, 0 completed jobs used", repo)
		return result, nil
	}

	workers := boundedWorkerCount(opts.APIWorkers, len(runs))
	runCh := make(chan workflowRun)
	resultCh := make(chan repoScanResult, workers)
	errCh := make(chan error, workers)
	var wg sync.WaitGroup

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for run := range runCh {
				runResult, err := collectRunJobs(client, repo, repoPath, run.ID, opts.JobFilter)
				if err != nil {
					errCh <- err
					continue
				}
				resultCh <- runResult
			}
		}()
	}

	go func() {
		for _, run := range runs {
			runCh <- run
		}
		close(runCh)
		wg.Wait()
		close(resultCh)
		close(errCh)
	}()

	var firstErr error
	for resultCh != nil || errCh != nil {
		select {
		case err, ok := <-errCh:
			if !ok {
				errCh = nil
				continue
			}
			if firstErr == nil {
				firstErr = err
			}
		case runResult, ok := <-resultCh:
			if !ok {
				resultCh = nil
				continue
			}
			result.WorkflowJobs += runResult.WorkflowJobs
			result.JobsUsed += runResult.JobsUsed
			result.Records = append(result.Records, runResult.Records...)
		}
	}
	if firstErr != nil {
		return result, firstErr
	}

	sortRecords(result.Records)
	client.logf("%s: %d workflow runs, %d workflow jobs, %d completed jobs used", repo, result.WorkflowRuns, result.WorkflowJobs, result.JobsUsed)
	return result, nil
}

func collectRunJobs(client *githubClient, repo, repoPath string, runID int64, jobFilter string) (repoScanResult, error) {
	result := repoScanResult{Repo: repo}
	client.debugf("%s: listing jobs for workflow run %d", repo, runID)
	params := url.Values{"filter": []string{jobFilter}}
	err := client.paginate(repoPath+"/actions/runs/"+strconv.FormatInt(runID, 10)+"/jobs", params, "jobs", func(rawJob json.RawMessage) error {
		var job workflowJob
		if err := json.Unmarshal(rawJob, &job); err != nil {
			return err
		}
		result.WorkflowJobs++
		rec, err := normalizeJob(job, repo)
		if err != nil {
			return err
		}
		if rec != nil {
			result.JobsUsed++
			result.Records = append(result.Records, *rec)
		}
		return nil
	})
	return result, err
}

func createdQuery(since, until string) string {
	if until != "" {
		return since + ".." + until
	}
	return ">=" + since
}

func boundedWorkerCount(workers, items int) int {
	if workers < 1 {
		workers = 1
	}
	if items > 0 && workers > items {
		return items
	}
	return workers
}

func normalizeJob(job workflowJob, repo string) (*record, error) {
	if job.StartedAt == "" || job.CompletedAt == "" {
		return nil, nil
	}
	start, err := time.Parse(time.RFC3339, job.StartedAt)
	if err != nil {
		return nil, fmt.Errorf("parse started_at %q: %w", job.StartedAt, err)
	}
	end, err := time.Parse(time.RFC3339, job.CompletedAt)
	if err != nil {
		return nil, fmt.Errorf("parse completed_at %q: %w", job.CompletedAt, err)
	}
	if !end.After(start) {
		return nil, nil
	}

	var queueSeconds *float64
	if job.CreatedAt != "" {
		created, err := time.Parse(time.RFC3339, job.CreatedAt)
		if err != nil {
			return nil, fmt.Errorf("parse created_at %q: %w", job.CreatedAt, err)
		}
		q := start.Sub(created).Seconds()
		if q < 0 {
			q = 0
		}
		queueSeconds = &q
	}

	return &record{
		Repo:            repo,
		WorkflowName:    strings.TrimSpace(job.WorkflowName),
		JobName:         strings.TrimSpace(job.Name),
		Conclusion:      strings.TrimSpace(job.Conclusion),
		Start:           start,
		End:             end,
		QueueSeconds:    queueSeconds,
		OS:              inferOS(job.Labels),
		SelfHosted:      isSelfHosted(job.Labels),
		Labels:          append([]string{}, job.Labels...),
		RunnerName:      strings.TrimSpace(job.RunnerName),
		RunnerGroupName: strings.TrimSpace(job.RunnerGroupName),
	}, nil
}

func inferOS(labels []string) string {
	joined := strings.ToLower(strings.Join(labels, " "))
	switch {
	case strings.Contains(joined, "windows"):
		return "windows"
	case strings.Contains(joined, "macos"), strings.Contains(joined, "mac-"):
		return "macos"
	default:
		return "linux"
	}
}

func isSelfHosted(labels []string) bool {
	for _, label := range labels {
		if strings.ToLower(label) == "self-hosted" {
			return true
		}
	}
	return false
}

func concurrencyProfile(intervals [][2]time.Time) (int, map[int]float64) {
	type event struct {
		t     time.Time
		delta int
	}
	var events []event
	for _, interval := range intervals {
		start, end := interval[0], interval[1]
		if !end.After(start) {
			continue
		}
		events = append(events, event{t: start, delta: 1}, event{t: end, delta: -1})
	}
	sort.Slice(events, func(i, j int) bool {
		if events[i].t.Equal(events[j].t) {
			return events[i].delta < events[j].delta
		}
		return events[i].t.Before(events[j].t)
	})

	running, peak := 0, 0
	var prev time.Time
	timeAtLevel := map[int]float64{}
	for i, event := range events {
		if i > 0 && running > 0 {
			timeAtLevel[running] += event.t.Sub(prev).Seconds()
		}
		running += event.delta
		if running < 0 {
			running = 0
		}
		if running > peak {
			peak = running
		}
		prev = event.t
	}
	return peak, timeAtLevel
}

func percentiles(timeAtLevel map[int]float64, ps []int) map[int]int {
	total := 0.0
	for _, seconds := range timeAtLevel {
		total += seconds
	}
	out := map[int]int{}
	if total == 0 {
		for _, p := range ps {
			out[p] = 0
		}
		return out
	}

	levels := sortedIntKeys(timeAtLevel)
	for _, p := range ps {
		target := total * float64(p) / 100.0
		cum := 0.0
		for _, level := range levels {
			cum += timeAtLevel[level]
			if cum >= target {
				out[p] = level
				break
			}
		}
	}
	return out
}

func sortedIntKeys[T any](m map[int]T) []int {
	keys := make([]int, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Ints(keys)
	return keys
}

type billableSlot struct {
	Jobs            int `json:"jobs"`
	BillableMinutes int `json:"billable_minutes"`
}

func billableMinutes(records []record) map[string]billableSlot {
	out := map[string]billableSlot{}
	for _, rec := range records {
		if rec.SelfHosted {
			continue
		}
		rawMinutes := rec.End.Sub(rec.Start).Seconds() / 60.0
		rounded := int(math.Ceil(rawMinutes))
		if rounded < 1 {
			rounded = 1
		}
		multiplier := osMultiplier[rec.OS]
		if multiplier == 0 {
			multiplier = 1
		}
		slot := out[rec.OS]
		slot.Jobs++
		slot.BillableMinutes += rounded * multiplier
		out[rec.OS] = slot
	}
	return out
}

type runnerPool struct {
	Name                  string         `json:"name"`
	Jobs                  int            `json:"jobs"`
	BusyHours             float64        `json:"busy_hours"`
	PeakConcurrency       int            `json:"peak_concurrency"`
	PercentileConcurrency map[string]int `json:"percentile_concurrency"`
	GitHubHosted          bool           `json:"github_hosted"`
	SelfHosted            bool           `json:"self_hosted"`
	OS                    string         `json:"os,omitempty"`
	RunnerGroupName       string         `json:"runner_group_name,omitempty"`
}

type usageSummary struct {
	Name                  string         `json:"name"`
	Jobs                  int            `json:"jobs"`
	BusyHours             float64        `json:"busy_hours"`
	PeakConcurrency       int            `json:"peak_concurrency"`
	PercentileConcurrency map[string]int `json:"percentile_concurrency"`
}

type runnerPoolKey struct {
	name            string
	gitHubHosted    bool
	selfHosted      bool
	osName          string
	runnerGroupName string
}

func runnerPools(records []record) []runnerPool {
	grouped := map[runnerPoolKey][]record{}
	for _, rec := range records {
		key := classifyRunnerPool(rec)
		grouped[key] = append(grouped[key], rec)
	}

	pools := make([]runnerPool, 0, len(grouped))
	for key, poolRecords := range grouped {
		intervals := make([][2]time.Time, 0, len(poolRecords))
		for _, rec := range poolRecords {
			intervals = append(intervals, [2]time.Time{rec.Start, rec.End})
		}
		peak, profile := concurrencyProfile(intervals)
		pct := percentiles(profile, []int{50, 90, 95, 99})
		busySeconds := 0.0
		for _, seconds := range profile {
			busySeconds += seconds
		}

		pools = append(pools, runnerPool{
			Name:                  key.name,
			Jobs:                  len(poolRecords),
			BusyHours:             math.Round((busySeconds/3600.0)*100) / 100,
			PeakConcurrency:       peak,
			PercentileConcurrency: map[string]int{"p50": pct[50], "p90": pct[90], "p95": pct[95], "p99": pct[99]},
			GitHubHosted:          key.gitHubHosted,
			SelfHosted:            key.selfHosted,
			OS:                    key.osName,
			RunnerGroupName:       key.runnerGroupName,
		})
	}

	sort.Slice(pools, func(i, j int) bool {
		if pools[i].PeakConcurrency != pools[j].PeakConcurrency {
			return pools[i].PeakConcurrency > pools[j].PeakConcurrency
		}
		if pools[i].Jobs != pools[j].Jobs {
			return pools[i].Jobs > pools[j].Jobs
		}
		return strings.ToLower(pools[i].Name) < strings.ToLower(pools[j].Name)
	})
	return pools
}

func topUsageSummaries(records []record, top int, keyFor func(record) string) []usageSummary {
	if top <= 0 {
		return nil
	}
	grouped := map[string][]record{}
	for _, rec := range records {
		key := strings.TrimSpace(keyFor(rec))
		if key == "" {
			key = "unknown"
		}
		grouped[key] = append(grouped[key], rec)
	}
	summaries := make([]usageSummary, 0, len(grouped))
	for name, groupRecords := range grouped {
		summaries = append(summaries, summarizeUsage(name, groupRecords))
	}
	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].BusyHours != summaries[j].BusyHours {
			return summaries[i].BusyHours > summaries[j].BusyHours
		}
		if summaries[i].Jobs != summaries[j].Jobs {
			return summaries[i].Jobs > summaries[j].Jobs
		}
		if summaries[i].PeakConcurrency != summaries[j].PeakConcurrency {
			return summaries[i].PeakConcurrency > summaries[j].PeakConcurrency
		}
		return strings.ToLower(summaries[i].Name) < strings.ToLower(summaries[j].Name)
	})
	if len(summaries) > top {
		summaries = summaries[:top]
	}
	return summaries
}

func summarizeUsage(name string, records []record) usageSummary {
	intervals := make([][2]time.Time, 0, len(records))
	for _, rec := range records {
		intervals = append(intervals, [2]time.Time{rec.Start, rec.End})
	}
	peak, profile := concurrencyProfile(intervals)
	pct := percentiles(profile, []int{50, 90, 95, 99})
	busySeconds := 0.0
	for _, seconds := range profile {
		busySeconds += seconds
	}
	return usageSummary{
		Name:                  name,
		Jobs:                  len(records),
		BusyHours:             math.Round((busySeconds/3600.0)*100) / 100,
		PeakConcurrency:       peak,
		PercentileConcurrency: map[string]int{"p50": pct[50], "p90": pct[90], "p95": pct[95], "p99": pct[99]},
	}
}

func workflowSummaryName(rec record) string {
	if rec.WorkflowName != "" {
		return rec.WorkflowName
	}
	return "unknown workflow"
}

func jobSummaryName(rec record) string {
	workflow := workflowSummaryName(rec)
	job := rec.JobName
	if job == "" {
		job = "unknown job"
	}
	return workflow + " / " + job
}

func classifyRunnerPool(rec record) runnerPoolKey {
	osName := rec.OS
	if osName == "" {
		osName = "unknown"
	}
	if !rec.SelfHosted {
		return runnerPoolKey{
			name:         "GitHub-hosted/" + osName,
			gitHubHosted: true,
			osName:       osName,
		}
	}

	groupName := runnerGroupPoolName(rec.RunnerGroupName)
	if groupName == "" {
		groupName = runnerPoolLabelHint(rec.Labels)
	}
	if groupName == "" {
		groupName = "unknown"
	}
	return runnerPoolKey{
		name:            "self-hosted/" + groupName,
		selfHosted:      true,
		runnerGroupName: groupName,
	}
}

func runnerGroupPoolName(groupName string) string {
	groupName = strings.TrimSpace(groupName)
	if groupName == "" || strings.EqualFold(groupName, "default") {
		return ""
	}
	return groupName
}

func runnerPoolLabelHint(labels []string) string {
	for _, label := range labels {
		trimmed := strings.TrimSpace(label)
		normalized := strings.ToLower(trimmed)
		if normalized == "" || isGenericRunnerLabel(normalized) {
			continue
		}
		switch {
		case normalized == "blacksmith" || strings.HasPrefix(normalized, "blacksmith-"):
			return "blacksmith"
		case normalized == "runs-on" || strings.HasPrefix(normalized, "runs-on-") || strings.HasPrefix(normalized, "runson-"):
			return "runs-on"
		case normalized == "arc" || strings.HasPrefix(normalized, "arc-") || strings.Contains(normalized, "actions-runner-controller"):
			return "arc"
		default:
			return trimmed
		}
	}
	return ""
}

func isGenericRunnerLabel(label string) bool {
	switch label {
	case "self-hosted", "linux", "windows", "macos", "mac", "x64", "x86", "x86_64", "amd64", "arm", "arm64", "aarch64", "ubuntu", "ubuntu-latest", "default":
		return true
	}
	return strings.HasPrefix(label, "ubuntu-")
}

type queueStats struct {
	Count   int     `json:"count"`
	MedianS float64 `json:"median_s"`
	P95S    float64 `json:"p95_s"`
	MaxS    float64 `json:"max_s"`
}

type scanSummary struct {
	RepositoriesQueued    int                 `json:"repositories_queued"`
	RepositoriesScanned   int                 `json:"repositories_scanned"`
	RepositoriesSkipped   int                 `json:"repositories_skipped"`
	SkippedRepositories   []skippedRepository `json:"skipped_repositories,omitempty"`
	WorkflowRuns          int                 `json:"workflow_runs"`
	WorkflowJobs          int                 `json:"workflow_jobs"`
	JobsUsed              int                 `json:"jobs_used"`
	Conclusions           map[string]int      `json:"conclusions,omitempty"`
	APIRequests           int                 `json:"api_requests"`
	Retries               int                 `json:"retries"`
	RateLimitSleeps       int                 `json:"rate_limit_sleeps"`
	RateLimitSleepSeconds float64             `json:"rate_limit_sleep_seconds"`
	RuntimeSeconds        float64             `json:"runtime_seconds"`
}

type estimateInterval struct {
	Median float64 `json:"median"`
	Lower  float64 `json:"lower"`
	Upper  float64 `json:"upper"`
}

type estimateMetrics struct {
	JobsAnalyzed          estimateInterval            `json:"jobs_analyzed"`
	BusyHours             estimateInterval            `json:"busy_hours"`
	PeakConcurrency       estimateInterval            `json:"peak_concurrency"`
	PercentileConcurrency map[string]estimateInterval `json:"percentile_concurrency"`
}

type estimateReport struct {
	Seed               int64           `json:"seed"`
	Confidence         int             `json:"confidence"`
	Iterations         int             `json:"iterations"`
	RequestBudget      int             `json:"request_budget"`
	MinRemaining       int             `json:"min_remaining"`
	TargetSampleRuns   int             `json:"target_sample_runs"`
	SampledRuns        int             `json:"sampled_runs"`
	UnsampledRuns      int             `json:"unsampled_runs"`
	KnownRuns          int             `json:"known_runs"`
	EstimatedTotalRuns int             `json:"estimated_total_runs"`
	SampleFraction     float64         `json:"sample_fraction"`
	CensusCompleteness float64         `json:"census_completeness"`
	StopReason         string          `json:"stop_reason,omitempty"`
	Warnings           []string        `json:"warnings,omitempty"`
	Metrics            estimateMetrics `json:"metrics"`
}

func collectRepositories(client *githubClient, repos []string, opts collectOptions, progress *progressReporter, skipped []skippedRepository, stderr io.Writer) ([]record, scanSummary, error) {
	summary := scanSummary{
		RepositoriesQueued:  len(repos),
		SkippedRepositories: append([]skippedRepository{}, skipped...),
		Conclusions:         map[string]int{},
	}
	workers := boundedWorkerCount(opts.APIWorkers, len(repos))
	repoCh := make(chan string)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var records []record
	var fatalErr error

	setFatal := func(err error) {
		mu.Lock()
		defer mu.Unlock()
		if fatalErr == nil {
			fatalErr = err
		}
	}
	hasFatal := func() bool {
		mu.Lock()
		defer mu.Unlock()
		return fatalErr != nil
	}
	addSkipped := func(repo, reason string) {
		mu.Lock()
		defer mu.Unlock()
		summary.SkippedRepositories = append(summary.SkippedRepositories, skippedRepository{Repo: repo, Reason: reason})
	}
	addResult := func(result repoScanResult) {
		mu.Lock()
		defer mu.Unlock()
		summary.RepositoriesScanned++
		summary.WorkflowRuns += result.WorkflowRuns
		summary.WorkflowJobs += result.WorkflowJobs
		summary.JobsUsed += result.JobsUsed
		for _, rec := range result.Records {
			conclusion := rec.Conclusion
			if conclusion == "" {
				conclusion = "unknown"
			}
			summary.Conclusions[conclusion]++
		}
		records = append(records, result.Records...)
	}

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for repo := range repoCh {
				if hasFatal() {
					continue
				}
				progress.Start(repo)
				result, err := collectJobs(client, repo, opts)
				if err != nil {
					var nf notFoundError
					var ae authError
					switch {
					case errors.As(err, &nf):
						fmt.Fprintf(stderr, "warning: %s not found or no Actions access; skipping.\n", repo)
						addSkipped(repo, "not found or no Actions access")
						progress.Skip(repo)
						continue
					case errors.As(err, &ae):
						setFatal(authError{})
						continue
					default:
						setFatal(err)
						continue
					}
				}
				addResult(result)
				progress.Done(repo, len(result.Records))
			}
		}()
	}

	for _, repo := range repos {
		if hasFatal() {
			break
		}
		repoCh <- repo
	}
	close(repoCh)
	wg.Wait()

	sortSkippedRepositories(summary.SkippedRepositories)
	summary.RepositoriesSkipped = len(summary.SkippedRepositories)
	if len(summary.Conclusions) == 0 {
		summary.Conclusions = nil
	}
	sortRecords(records)

	mu.Lock()
	err := fatalErr
	mu.Unlock()
	if err != nil {
		return nil, summary, err
	}
	return records, summary, nil
}

type workflowRunPage struct {
	TotalCount   int           `json:"total_count"`
	WorkflowRuns []workflowRun `json:"workflow_runs"`
}

type sampledRun struct {
	Run     workflowRun
	Records []record
}

type jobShape struct {
	Offset          time.Duration
	Duration        time.Duration
	WorkflowName    string
	JobName         string
	Conclusion      string
	OS              string
	SelfHosted      bool
	Labels          []string
	RunnerName      string
	RunnerGroupName string
}

type runShape struct {
	Key         string
	FallbackKey string
	Jobs        []jobShape
}

type simulationMetric struct {
	JobsAnalyzed int
	BusyHours    float64
	Peak         int
	P50          int
	P90          int
	P95          int
	P99          int
}

func runEstimate(client *githubClient, cfg config, skipped []skippedRepository, started time.Time, stderr io.Writer) (report, error) {
	if cfg.estimateSeed == 0 {
		cfg.estimateSeed = time.Now().UnixNano()
	}
	client.enableRequestBudget(cfg.estimateMaxRequests, cfg.estimateMinRemaining)
	defer client.disableRequestBudget()

	opts := collectOptions{
		Since:               cfg.since,
		Until:               cfg.until,
		IncludeInProgress:   cfg.includeInProgress,
		JobFilter:           cfg.jobFilter,
		Branch:              cfg.branch,
		Event:               cfg.event,
		ExcludePullRequests: cfg.excludePullRequests,
		APIWorkers:          cfg.apiWorkers,
	}
	runs, estimatedTotalRuns, censusComplete, summary, stopReason, err := collectWorkflowRunCensus(client, cfg.repos, opts, skipped, stderr)
	if err != nil {
		return report{}, err
	}
	if len(runs) == 0 {
		return report{}, errors.New("not enough sampled workflow runs for estimate before API budget/rate-limit stop")
	}
	sampleCandidates := selectSampleRuns(runs, min(cfg.estimateSampleRuns, len(runs)), cfg.estimateSeed)
	sampled, sampleSummary, sampleStopReason, err := collectSampledRunJobs(client, sampleCandidates, opts)
	if err != nil {
		var stopErr requestBudgetStopError
		if !errors.As(err, &stopErr) {
			return report{}, err
		}
	}
	if sampleStopReason != "" {
		stopReason = sampleStopReason
	}
	summary.WorkflowJobs += sampleSummary.WorkflowJobs
	summary.JobsUsed += sampleSummary.JobsUsed
	for key, count := range sampleSummary.Conclusions {
		if summary.Conclusions == nil {
			summary.Conclusions = map[string]int{}
		}
		summary.Conclusions[key] += count
	}

	minSample := minViableSample(len(runs))
	if len(sampled) < minSample {
		return report{}, errors.New("not enough sampled workflow runs for estimate before API budget/rate-limit stop")
	}
	if sampleSummary.JobsUsed == 0 {
		return report{}, errors.New("not enough sampled workflow runs for estimate before API budget/rate-limit stop")
	}

	metrics, warnings := simulateEstimate(runs, sampled, cfg)
	if stopReason == "" {
		stopReason = client.requestBudgetStopReason()
	}
	if stopReason != "" {
		warnings = append(warnings, stopReason)
	}
	if !censusComplete {
		warnings = append(warnings, "workflow-run census stopped before all target repositories/pages were read")
	}
	warnings = append(warnings, "Peak concurrency is sensitive to rare unsampled fan-out; run exact mode before final commitments.")

	stats := client.statsSnapshot()
	runtimeS := runtimeSeconds(time.Since(started))
	summary.APIRequests = stats.Requests
	summary.Retries = stats.Retries
	summary.RateLimitSleeps = stats.RateLimitSleeps
	summary.RateLimitSleepSeconds = stats.RateLimitSleepSeconds
	summary.RuntimeSeconds = runtimeS
	if len(summary.Conclusions) == 0 {
		summary.Conclusions = nil
	}

	knownRuns := len(runs)
	estimatedTotal := estimatedTotalRuns
	if estimatedTotal < knownRuns {
		estimatedTotal = knownRuns
	}
	estimate := &estimateReport{
		Seed:               cfg.estimateSeed,
		Confidence:         cfg.estimateConfidence,
		Iterations:         cfg.estimateIterations,
		RequestBudget:      cfg.estimateMaxRequests,
		MinRemaining:       cfg.estimateMinRemaining,
		TargetSampleRuns:   cfg.estimateSampleRuns,
		SampledRuns:        len(sampled),
		UnsampledRuns:      max(0, knownRuns-len(sampled)),
		KnownRuns:          knownRuns,
		EstimatedTotalRuns: estimatedTotal,
		SampleFraction:     roundFloat(float64(len(sampled))/float64(knownRuns), 4),
		CensusCompleteness: roundFloat(censusCompletenessRatio(knownRuns, estimatedTotal, censusComplete), 4),
		StopReason:         stopReason,
		Warnings:           warnings,
		Metrics:            metrics,
	}

	rep := report{
		Tool:            "gh-concurrency",
		Version:         version,
		GeneratedAt:     time.Now().UTC().Format(time.RFC3339),
		RuntimeSeconds:  runtimeS,
		Parameters:      buildParameters(cfg),
		Scan:            summary,
		JobsAnalyzed:    int(math.Round(metrics.JobsAnalyzed.Median)),
		BusyHours:       roundFloat(metrics.BusyHours.Median, 2),
		PeakConcurrency: int(math.Round(metrics.PeakConcurrency.Median)),
		PercentileConcurrency: map[string]int{
			"p50": int(math.Round(metrics.PercentileConcurrency["p50"].Median)),
			"p90": int(math.Round(metrics.PercentileConcurrency["p90"].Median)),
			"p95": int(math.Round(metrics.PercentileConcurrency["p95"].Median)),
			"p99": int(math.Round(metrics.PercentileConcurrency["p99"].Median)),
		},
		RunnerPools:             nil,
		BillableMinutesEstimate: nil,
		Warnings:                warnings,
		Estimate:                estimate,
	}
	return rep, nil
}

func collectWorkflowRunCensus(client *githubClient, repos []string, opts collectOptions, skipped []skippedRepository, stderr io.Writer) ([]workflowRun, int, bool, scanSummary, string, error) {
	summary := scanSummary{
		RepositoriesQueued:  len(repos),
		SkippedRepositories: append([]skippedRepository{}, skipped...),
		Conclusions:         map[string]int{},
	}
	var out []workflowRun
	estimatedTotal := 0
	censusComplete := true
	stopReason := ""
	for _, repo := range repos {
		runs, total, complete, err := listWorkflowRunsForEstimate(client, repo, opts)
		if err != nil {
			var stopErr requestBudgetStopError
			var nf notFoundError
			var ae authError
			switch {
			case errors.As(err, &stopErr):
				stopReason = stopErr.Reason
				censusComplete = false
			case errors.As(err, &nf):
				fmt.Fprintf(stderr, "warning: %s not found or no Actions access; skipping.\n", repo)
				summary.SkippedRepositories = append(summary.SkippedRepositories, skippedRepository{Repo: repo, Reason: "not found or no Actions access"})
				continue
			case errors.As(err, &ae):
				return nil, 0, false, summary, stopReason, authError{}
			default:
				return nil, 0, false, summary, stopReason, err
			}
		}
		for _, run := range runs {
			conclusion := run.Conclusion
			if conclusion == "" {
				conclusion = "unknown"
			}
			summary.Conclusions[conclusion]++
		}
		summary.RepositoriesScanned++
		summary.WorkflowRuns += len(runs)
		out = append(out, runs...)
		if total > 0 {
			estimatedTotal += total
		} else {
			estimatedTotal += len(runs)
		}
		if !complete {
			censusComplete = false
			break
		}
		if stopReason != "" {
			break
		}
	}
	sortWorkflowRuns(out)
	sortSkippedRepositories(summary.SkippedRepositories)
	summary.RepositoriesSkipped = len(summary.SkippedRepositories)
	if len(summary.Conclusions) == 0 {
		summary.Conclusions = nil
	}
	return out, estimatedTotal, censusComplete, summary, stopReason, nil
}

func listWorkflowRunsForEstimate(client *githubClient, repo string, opts collectOptions) ([]workflowRun, int, bool, error) {
	repoPath, err := repoAPIPath(repo)
	if err != nil {
		return nil, 0, false, err
	}
	params := workflowRunParams(opts)
	params.Set("per_page", "100")
	nextURL := client.baseURL + repoPath + "/actions/runs?" + params.Encode()
	total := 0
	complete := true
	var runs []workflowRun
	for nextURL != "" {
		body, link, err := client.request(nextURL)
		if err != nil {
			var stopErr requestBudgetStopError
			if errors.As(err, &stopErr) {
				return runs, total, false, err
			}
			return runs, total, false, err
		}
		var page workflowRunPage
		if err := json.Unmarshal(body, &page); err != nil {
			return runs, total, false, err
		}
		if page.TotalCount > total {
			total = page.TotalCount
		}
		for _, run := range page.WorkflowRuns {
			run.Repo = repo
			runs = append(runs, run)
		}
		nextURL = nextLink(link)
	}
	return runs, total, complete, nil
}

func workflowRunParams(opts collectOptions) url.Values {
	params := url.Values{"created": []string{createdQuery(opts.Since, opts.Until)}}
	if !opts.IncludeInProgress {
		params.Set("status", "completed")
	}
	if opts.Branch != "" {
		params.Set("branch", opts.Branch)
	}
	if opts.Event != "" {
		params.Set("event", opts.Event)
	}
	if opts.ExcludePullRequests {
		params.Set("exclude_pull_requests", "true")
	}
	return params
}

func sortWorkflowRuns(runs []workflowRun) {
	sort.Slice(runs, func(i, j int) bool {
		if strings.ToLower(runs[i].Repo) != strings.ToLower(runs[j].Repo) {
			return strings.ToLower(runs[i].Repo) < strings.ToLower(runs[j].Repo)
		}
		if !runAnchor(runs[i]).Equal(runAnchor(runs[j])) {
			return runAnchor(runs[i]).Before(runAnchor(runs[j]))
		}
		return runs[i].ID < runs[j].ID
	})
}

func selectSampleRuns(runs []workflowRun, target int, seed int64) []workflowRun {
	if target >= len(runs) {
		return append([]workflowRun{}, runs...)
	}
	if target < 1 {
		target = 1
	}
	groups := map[string][]workflowRun{}
	for _, run := range runs {
		groups[runStratumKey(run)] = append(groups[runStratumKey(run)], run)
	}
	keys := sortedStringKeys(groups)
	rng := rand.New(rand.NewSource(seed))
	var sample []workflowRun
	var largeKeys []string
	for _, key := range keys {
		group := append([]workflowRun{}, groups[key]...)
		sortWorkflowRuns(group)
		if len(group) <= 2 && len(sample)+len(group) <= target {
			sample = append(sample, group...)
			continue
		}
		largeKeys = append(largeKeys, key)
	}
	for _, key := range largeKeys {
		group := append([]workflowRun{}, groups[key]...)
		shuffleWorkflowRuns(group, rng)
		groups[key] = group
	}
	for len(sample) < target && len(largeKeys) > 0 {
		progress := false
		for _, key := range largeKeys {
			group := groups[key]
			if len(group) == 0 {
				continue
			}
			sample = append(sample, group[0])
			groups[key] = group[1:]
			progress = true
			if len(sample) == target {
				break
			}
		}
		if !progress {
			break
		}
	}
	sortWorkflowRuns(sample)
	return sample
}

func shuffleWorkflowRuns(runs []workflowRun, rng *rand.Rand) {
	rng.Shuffle(len(runs), func(i, j int) {
		runs[i], runs[j] = runs[j], runs[i]
	})
}

func collectSampledRunJobs(client *githubClient, runs []workflowRun, opts collectOptions) ([]sampledRun, scanSummary, string, error) {
	summary := scanSummary{Conclusions: map[string]int{}}
	var sampled []sampledRun
	stopReason := ""
	for _, run := range runs {
		repoPath, err := repoAPIPath(run.Repo)
		if err != nil {
			return sampled, summary, stopReason, err
		}
		result, err := collectRunJobs(client, run.Repo, repoPath, run.ID, opts.JobFilter)
		if err != nil {
			var stopErr requestBudgetStopError
			if errors.As(err, &stopErr) {
				stopReason = stopErr.Reason
				return sampled, summary, stopReason, err
			}
			return sampled, summary, stopReason, err
		}
		summary.WorkflowJobs += result.WorkflowJobs
		summary.JobsUsed += result.JobsUsed
		for _, rec := range result.Records {
			conclusion := rec.Conclusion
			if conclusion == "" {
				conclusion = "unknown"
			}
			summary.Conclusions[conclusion]++
		}
		sampled = append(sampled, sampledRun{Run: run, Records: result.Records})
	}
	if len(summary.Conclusions) == 0 {
		summary.Conclusions = nil
	}
	return sampled, summary, stopReason, nil
}

func simulateEstimate(runs []workflowRun, sampled []sampledRun, cfg config) (estimateMetrics, []string) {
	sampledIDs := map[int64]bool{}
	var fixedRecords []record
	var shapes []runShape
	for _, sample := range sampled {
		sampledIDs[sample.Run.ID] = true
		fixedRecords = append(fixedRecords, sample.Records...)
		shapes = append(shapes, buildRunShape(sample.Run, sample.Records))
	}
	var unsampled []workflowRun
	for _, run := range runs {
		if !sampledIDs[run.ID] {
			unsampled = append(unsampled, run)
		}
	}

	byKey := map[string][]runShape{}
	byFallback := map[string][]runShape{}
	for _, shape := range shapes {
		byKey[shape.Key] = append(byKey[shape.Key], shape)
		byFallback[shape.FallbackKey] = append(byFallback[shape.FallbackKey], shape)
	}
	iterations := cfg.estimateIterations
	if iterations < 1 {
		iterations = 1
	}
	values := make([]simulationMetric, 0, iterations)
	warnings := []string{}
	globalShapes := shapes
	if len(globalShapes) == 0 {
		return emptyEstimateMetrics(), []string{"No sampled workflow runs had usable completed jobs."}
	}
	for i := 0; i < iterations; i++ {
		rng := rand.New(rand.NewSource(cfg.estimateSeed + int64(i+1)))
		records := append([]record{}, fixedRecords...)
		for _, run := range unsampled {
			anchor := runAnchor(run)
			if anchor.IsZero() {
				continue
			}
			shape := drawRunShape(run, byKey, byFallback, globalShapes, rng)
			records = append(records, applyRunShape(run, anchor, shape)...)
		}
		values = append(values, metricForRecords(records))
	}
	return intervalsForMetrics(values, cfg.estimateConfidence), warnings
}

func buildRunShape(run workflowRun, records []record) runShape {
	anchor := runAnchor(run)
	shape := runShape{Key: runStratumKey(run), FallbackKey: runFallbackKey(run)}
	for _, rec := range records {
		offset := time.Duration(0)
		if !anchor.IsZero() {
			offset = rec.Start.Sub(anchor)
		}
		shape.Jobs = append(shape.Jobs, jobShape{
			Offset:          offset,
			Duration:        rec.End.Sub(rec.Start),
			WorkflowName:    rec.WorkflowName,
			JobName:         rec.JobName,
			Conclusion:      rec.Conclusion,
			OS:              rec.OS,
			SelfHosted:      rec.SelfHosted,
			Labels:          append([]string{}, rec.Labels...),
			RunnerName:      rec.RunnerName,
			RunnerGroupName: rec.RunnerGroupName,
		})
	}
	return shape
}

func drawRunShape(run workflowRun, byKey, byFallback map[string][]runShape, global []runShape, rng *rand.Rand) runShape {
	if shapes := byKey[runStratumKey(run)]; len(shapes) > 0 {
		return shapes[rng.Intn(len(shapes))]
	}
	if shapes := byFallback[runFallbackKey(run)]; len(shapes) > 0 {
		return shapes[rng.Intn(len(shapes))]
	}
	return global[rng.Intn(len(global))]
}

func applyRunShape(run workflowRun, anchor time.Time, shape runShape) []record {
	var records []record
	for _, job := range shape.Jobs {
		start := anchor.Add(job.Offset)
		end := start.Add(job.Duration)
		if !end.After(start) {
			continue
		}
		records = append(records, record{
			Repo:            run.Repo,
			WorkflowName:    firstNonEmpty(run.Name, job.WorkflowName),
			JobName:         job.JobName,
			Conclusion:      firstNonEmpty(run.Conclusion, job.Conclusion),
			Start:           start,
			End:             end,
			OS:              job.OS,
			SelfHosted:      job.SelfHosted,
			Labels:          append([]string{}, job.Labels...),
			RunnerName:      job.RunnerName,
			RunnerGroupName: job.RunnerGroupName,
		})
	}
	return records
}

func metricForRecords(records []record) simulationMetric {
	intervals := make([][2]time.Time, 0, len(records))
	for _, rec := range records {
		intervals = append(intervals, [2]time.Time{rec.Start, rec.End})
	}
	peak, profile := concurrencyProfile(intervals)
	pct := percentiles(profile, []int{50, 90, 95, 99})
	busySeconds := 0.0
	for _, seconds := range profile {
		busySeconds += seconds
	}
	return simulationMetric{
		JobsAnalyzed: len(records),
		BusyHours:    math.Round((busySeconds/3600.0)*100) / 100,
		Peak:         peak,
		P50:          pct[50],
		P90:          pct[90],
		P95:          pct[95],
		P99:          pct[99],
	}
}

func intervalsForMetrics(values []simulationMetric, confidence int) estimateMetrics {
	if len(values) == 0 {
		return emptyEstimateMetrics()
	}
	return estimateMetrics{
		JobsAnalyzed:    intervalFromValues(values, confidence, func(v simulationMetric) float64 { return float64(v.JobsAnalyzed) }),
		BusyHours:       intervalFromValues(values, confidence, func(v simulationMetric) float64 { return v.BusyHours }),
		PeakConcurrency: intervalFromValues(values, confidence, func(v simulationMetric) float64 { return float64(v.Peak) }),
		PercentileConcurrency: map[string]estimateInterval{
			"p50": intervalFromValues(values, confidence, func(v simulationMetric) float64 { return float64(v.P50) }),
			"p90": intervalFromValues(values, confidence, func(v simulationMetric) float64 { return float64(v.P90) }),
			"p95": intervalFromValues(values, confidence, func(v simulationMetric) float64 { return float64(v.P95) }),
			"p99": intervalFromValues(values, confidence, func(v simulationMetric) float64 { return float64(v.P99) }),
		},
	}
}

func emptyEstimateMetrics() estimateMetrics {
	zero := estimateInterval{}
	return estimateMetrics{
		JobsAnalyzed:          zero,
		BusyHours:             zero,
		PeakConcurrency:       zero,
		PercentileConcurrency: map[string]estimateInterval{"p50": zero, "p90": zero, "p95": zero, "p99": zero},
	}
}

func intervalFromValues(values []simulationMetric, confidence int, valueFor func(simulationMetric) float64) estimateInterval {
	vals := make([]float64, 0, len(values))
	for _, value := range values {
		vals = append(vals, valueFor(value))
	}
	sort.Float64s(vals)
	alpha := float64(100-confidence) / 2.0
	lower := percentileFloat(vals, alpha)
	upper := percentileFloat(vals, 100-alpha)
	median := percentileFloat(vals, 50)
	return estimateInterval{
		Median: roundFloat(median, 2),
		Lower:  roundFloat(lower, 2),
		Upper:  roundFloat(upper, 2),
	}
}

func percentileFloat(values []float64, pct float64) float64 {
	if len(values) == 0 {
		return 0
	}
	if pct <= 0 {
		return values[0]
	}
	if pct >= 100 {
		return values[len(values)-1]
	}
	idx := int(math.Ceil((pct/100.0)*float64(len(values)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(values) {
		idx = len(values) - 1
	}
	return values[idx]
}

func runAnchor(run workflowRun) time.Time {
	for _, value := range []string{run.RunStartedAt, run.CreatedAt, run.UpdatedAt} {
		if value == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, value)
		if err == nil {
			return t
		}
	}
	return time.Time{}
}

func runStratumKey(run workflowRun) string {
	workflow := run.Name
	if run.WorkflowID != 0 {
		workflow = strconv.FormatInt(run.WorkflowID, 10)
	}
	if workflow == "" {
		workflow = "unknown-workflow"
	}
	event := run.Event
	if event == "" {
		event = "unknown-event"
	}
	return strings.ToLower(run.Repo + "|" + workflow + "|" + event)
}

func runFallbackKey(run workflowRun) string {
	event := run.Event
	if event == "" {
		event = "unknown-event"
	}
	return strings.ToLower(run.Repo + "|" + event)
}

func minViableSample(knownRuns int) int {
	if knownRuns <= 0 {
		return 0
	}
	tenPercent := int(math.Ceil(float64(knownRuns) * 0.10))
	if tenPercent < 1 {
		tenPercent = 1
	}
	return min(30, tenPercent)
}

func censusCompletenessRatio(known, estimatedTotal int, complete bool) float64 {
	if complete {
		return 1
	}
	if estimatedTotal <= 0 {
		return 0
	}
	return float64(known) / float64(estimatedTotal)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func roundFloat(value float64, places int) float64 {
	if places < 0 {
		places = 0
	}
	scale := math.Pow10(places)
	return math.Round(value*scale) / scale
}

func sortRecords(records []record) {
	sort.Slice(records, func(i, j int) bool {
		if strings.ToLower(records[i].Repo) != strings.ToLower(records[j].Repo) {
			return strings.ToLower(records[i].Repo) < strings.ToLower(records[j].Repo)
		}
		if !records[i].Start.Equal(records[j].Start) {
			return records[i].Start.Before(records[j].Start)
		}
		if !records[i].End.Equal(records[j].End) {
			return records[i].End.Before(records[j].End)
		}
		if records[i].WorkflowName != records[j].WorkflowName {
			return records[i].WorkflowName < records[j].WorkflowName
		}
		return records[i].JobName < records[j].JobName
	})
}

func computeQueueStats(records []record) *queueStats {
	var qs []float64
	for _, rec := range records {
		if rec.QueueSeconds != nil {
			qs = append(qs, *rec.QueueSeconds)
		}
	}
	if len(qs) == 0 {
		return nil
	}
	sort.Float64s(qs)
	n := len(qs)
	return &queueStats{
		Count:   n,
		MedianS: qs[n/2],
		P95S:    qs[min(n-1, int(float64(n)*0.95))],
		MaxS:    qs[n-1],
	}
}

func detectWarnings(peak int, pct map[int]int, qstats *queueStats) []string {
	var warnings []string
	if qstats != nil && qstats.P95S > 60 {
		warnings = append(warnings, fmt.Sprintf(
			"95th-percentile queue time is %.0fs. Sustained queueing means jobs waited instead of running in parallel, so true demand is likely higher than the concurrency reported here.",
			qstats.P95S,
		))
	}
	roundPeaks := map[int]bool{5: true, 10: true, 20: true, 40: true, 60: true, 180: true, 300: true}
	if roundPeaks[peak] && pct[95] == peak {
		warnings = append(warnings, fmt.Sprintf(
			"Peak (%d) sits at a round number and equals p95, which can indicate you were hitting a GitHub concurrency limit. If so, real demand exceeds this figure.",
			peak,
		))
	}
	return warnings
}

type parameters struct {
	Repos                []string `json:"repos"`
	Orgs                 []string `json:"orgs,omitempty"`
	RepoFiles            []string `json:"repo_files,omitempty"`
	OrgFiles             []string `json:"org_files,omitempty"`
	RepoType             string   `json:"repo_type,omitempty"`
	IncludeArchived      bool     `json:"include_archived"`
	RepositoryCount      int      `json:"repository_count"`
	Since                string   `json:"since"`
	Until                string   `json:"until,omitempty"`
	BaseURL              string   `json:"base_url"`
	APIWorkers           int      `json:"api_workers"`
	RunStatus            string   `json:"run_status"`
	JobFilter            string   `json:"job_filter"`
	Branch               string   `json:"branch,omitempty"`
	Event                string   `json:"event,omitempty"`
	ExcludePullRequests  bool     `json:"exclude_pull_requests"`
	Top                  int      `json:"top"`
	Mode                 string   `json:"mode"`
	EstimateMaxRequests  int      `json:"estimate_max_requests,omitempty"`
	EstimateMinRemaining int      `json:"estimate_min_remaining,omitempty"`
	EstimateSampleRuns   int      `json:"estimate_sample_runs,omitempty"`
	EstimateIterations   int      `json:"estimate_iterations,omitempty"`
	EstimateConfidence   int      `json:"estimate_confidence,omitempty"`
	EstimateSeed         int64    `json:"estimate_seed,omitempty"`
}

type report struct {
	Tool                    string                  `json:"tool"`
	Version                 string                  `json:"version"`
	GeneratedAt             string                  `json:"generated_at"`
	RuntimeSeconds          float64                 `json:"runtime_seconds"`
	Parameters              parameters              `json:"parameters"`
	Scan                    scanSummary             `json:"scan"`
	JobsAnalyzed            int                     `json:"jobs_analyzed"`
	BusyHours               float64                 `json:"busy_hours"`
	PeakConcurrency         int                     `json:"peak_concurrency"`
	PercentileConcurrency   map[string]int          `json:"percentile_concurrency"`
	RunnerPools             []runnerPool            `json:"runner_pools"`
	TopRepositories         []usageSummary          `json:"top_repositories,omitempty"`
	TopWorkflows            []usageSummary          `json:"top_workflows,omitempty"`
	TopJobs                 []usageSummary          `json:"top_jobs,omitempty"`
	BillableMinutesEstimate map[string]billableSlot `json:"billable_minutes_estimate"`
	QueueSeconds            *queueStats             `json:"queue_seconds"`
	Warnings                []string                `json:"warnings"`
	Estimate                *estimateReport         `json:"estimate,omitempty"`
}

func buildReport(records []record, cfg config, runtime time.Duration, summary scanSummary, stats requestStats) report {
	intervals := make([][2]time.Time, 0, len(records))
	for _, rec := range records {
		intervals = append(intervals, [2]time.Time{rec.Start, rec.End})
	}
	peak, profile := concurrencyProfile(intervals)
	pct := percentiles(profile, []int{50, 90, 95, 99})
	qstats := computeQueueStats(records)
	busySeconds := 0.0
	for _, seconds := range profile {
		busySeconds += seconds
	}
	runtimeS := runtimeSeconds(runtime)
	summary.APIRequests = stats.Requests
	summary.Retries = stats.Retries
	summary.RateLimitSleeps = stats.RateLimitSleeps
	summary.RateLimitSleepSeconds = stats.RateLimitSleepSeconds
	summary.RuntimeSeconds = runtimeS
	params := buildParameters(cfg)
	return report{
		Tool:                    "gh-concurrency",
		Version:                 version,
		GeneratedAt:             time.Now().UTC().Format(time.RFC3339),
		RuntimeSeconds:          runtimeS,
		Parameters:              params,
		Scan:                    summary,
		JobsAnalyzed:            len(records),
		BusyHours:               math.Round((busySeconds/3600.0)*100) / 100,
		PeakConcurrency:         peak,
		PercentileConcurrency:   map[string]int{"p50": pct[50], "p90": pct[90], "p95": pct[95], "p99": pct[99]},
		RunnerPools:             runnerPools(records),
		TopRepositories:         topUsageSummaries(records, cfg.top, func(rec record) string { return rec.Repo }),
		TopWorkflows:            topUsageSummaries(records, cfg.top, workflowSummaryName),
		TopJobs:                 topUsageSummaries(records, cfg.top, jobSummaryName),
		BillableMinutesEstimate: billableMinutes(records),
		QueueSeconds:            qstats,
		Warnings:                detectWarnings(peak, pct, qstats),
	}
}

func buildParameters(cfg config) parameters {
	params := parameters{
		Repos:               cfg.repos,
		Orgs:                cfg.orgs,
		RepoFiles:           cfg.repoFiles,
		OrgFiles:            cfg.orgFiles,
		RepoType:            cfg.repoType,
		IncludeArchived:     cfg.includeArchived,
		RepositoryCount:     len(cfg.repos),
		Since:               cfg.since,
		Until:               cfg.until,
		BaseURL:             cfg.baseURL,
		APIWorkers:          cfg.apiWorkers,
		RunStatus:           runStatus(cfg),
		JobFilter:           cfg.jobFilter,
		Branch:              cfg.branch,
		Event:               cfg.event,
		ExcludePullRequests: cfg.excludePullRequests,
		Top:                 cfg.top,
		Mode:                modeName(cfg),
	}
	if cfg.estimate {
		params.EstimateMaxRequests = cfg.estimateMaxRequests
		params.EstimateMinRemaining = cfg.estimateMinRemaining
		params.EstimateSampleRuns = cfg.estimateSampleRuns
		params.EstimateIterations = cfg.estimateIterations
		params.EstimateConfidence = cfg.estimateConfidence
		params.EstimateSeed = cfg.estimateSeed
	}
	return params
}

func modeName(cfg config) string {
	if cfg.estimate {
		return "estimate"
	}
	return "exact"
}

func runStatus(cfg config) string {
	if cfg.includeInProgress {
		return "all"
	}
	return "completed"
}

func runtimeSeconds(runtime time.Duration) float64 {
	if runtime < 0 {
		runtime = 0
	}
	return math.Round(runtime.Seconds()*1000) / 1000
}

func formatRunDuration(runtimeSeconds float64) string {
	if runtimeSeconds < 0 {
		runtimeSeconds = 0
	}
	duration := time.Duration(runtimeSeconds * float64(time.Second)).Round(100 * time.Millisecond)
	return duration.String()
}

func displayVersion(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "dev"
	}
	if strings.HasPrefix(value, "v") || value == "dev" {
		return value
	}
	return "v" + value
}

func printText(out io.Writer, rep report) {
	p := rep.Parameters
	until := p.Until
	if until == "" {
		until = "now"
	}
	fmt.Fprintf(out, "\ngh-concurrency %s\n", displayVersion(rep.Version))
	if len(p.Orgs) > 0 {
		fmt.Fprintf(out, "orgs:   %s\n", strings.Join(p.Orgs, ", "))
	}
	if len(p.RepoFiles) > 0 {
		fmt.Fprintf(out, "repo files: %s\n", strings.Join(p.RepoFiles, ", "))
	}
	if len(p.OrgFiles) > 0 {
		fmt.Fprintf(out, "org files:  %s\n", strings.Join(p.OrgFiles, ", "))
	}
	if p.IncludeArchived {
		fmt.Fprintln(out, "archived repos: included")
	}
	fmt.Fprintf(out, "repos:  %s\n", summarizeRepos(p.Repos))
	fmt.Fprintf(out, "repo count: %d\n", p.RepositoryCount)
	fmt.Fprintf(out, "window: %s -> %s   api: %s\n", p.Since, until, p.BaseURL)
	fmt.Fprintf(out, "filters: runs=%s jobs=%s workers=%d", p.RunStatus, p.JobFilter, p.APIWorkers)
	if p.Branch != "" {
		fmt.Fprintf(out, " branch=%s", p.Branch)
	}
	if p.Event != "" {
		fmt.Fprintf(out, " event=%s", p.Event)
	}
	if p.ExcludePullRequests {
		fmt.Fprint(out, " exclude_prs=true")
	}
	fmt.Fprintln(out)
	if rep.Estimate != nil {
		printEstimateText(out, rep)
		return
	}
	fmt.Fprintf(out, "\nJobs analyzed:        %d\n", rep.JobsAnalyzed)
	fmt.Fprintf(out, "Run time:             %s\n", formatRunDuration(rep.RuntimeSeconds))
	fmt.Fprintf(out, "Busy wall-clock time: %.2fh (>=1 job running)\n", rep.BusyHours)
	fmt.Fprintf(out, "Peak concurrency:     %d\n", rep.PeakConcurrency)
	for _, key := range []string{"p50", "p90", "p95", "p99"} {
		fmt.Fprintf(out, "%s concurrency:       %d\n", key, rep.PercentileConcurrency[key])
	}

	printScanSummary(out, rep.Scan)

	if len(rep.RunnerPools) > 0 {
		fmt.Fprintln(out, "\nRunner pools:")
		for _, pool := range rep.RunnerPools {
			fmt.Fprintf(
				out,
				"  %-28s peak %4d  p95 %4d  %8s jobs\n",
				pool.Name,
				pool.PeakConcurrency,
				pool.PercentileConcurrency["p95"],
				comma(pool.Jobs),
			)
		}
	}

	printUsageSummaries(out, "Top repositories by busy time:", rep.TopRepositories)
	printUsageSummaries(out, "Top workflows by busy time:", rep.TopWorkflows)
	printUsageSummaries(out, "Top jobs by busy time:", rep.TopJobs)

	if len(rep.BillableMinutesEstimate) > 0 {
		fmt.Fprintln(out, "\nBillable-minutes estimate (sanity-check vs your invoice):")
		total := 0
		for _, osName := range sortedStringKeys(rep.BillableMinutesEstimate) {
			slot := rep.BillableMinutesEstimate[osName]
			total += slot.BillableMinutes
			fmt.Fprintf(out, "  %-8s %6d jobs  %10s billable min\n", osName, slot.Jobs, comma(slot.BillableMinutes))
		}
		fmt.Fprintf(out, "  %-8s %6s       %10s billable min\n", "TOTAL", "", comma(total))
	}

	if rep.QueueSeconds != nil {
		q := rep.QueueSeconds
		fmt.Fprintf(out, "\nQueue time: median %.0fs  p95 %.0fs  max %.0fs\n", q.MedianS, q.P95S, q.MaxS)
	}

	fmt.Fprintln(out, "\nSize Buildkite toward ~p95/p99, not the absolute peak: one 2am")
	fmt.Fprintln(out, "cron fan-out should not make you pay for that slot all month.")

	for _, warning := range rep.Warnings {
		fmt.Fprintf(out, "\nWARNING: %s\n", warning)
	}
}

func printEstimateText(out io.Writer, rep report) {
	est := rep.Estimate
	fmt.Fprintf(out, "\nESTIMATE MODE: sampled %s of %s known workflow runs; %d%% simulation interval; seed %d\n",
		comma(est.SampledRuns), comma(est.KnownRuns), est.Confidence, est.Seed)
	if est.CensusCompleteness < 1 {
		fmt.Fprintf(out, "Census completeness: %.1f%% of estimated workflow runs\n", est.CensusCompleteness*100)
	}
	if est.StopReason != "" {
		fmt.Fprintf(out, "Stopped early: %s\n", est.StopReason)
	}
	fmt.Fprintf(out, "\nJobs analyzed:        median %s (%d%% range %s-%s)\n",
		formatEstimateNumber(est.Metrics.JobsAnalyzed.Median),
		est.Confidence,
		formatEstimateNumber(est.Metrics.JobsAnalyzed.Lower),
		formatEstimateNumber(est.Metrics.JobsAnalyzed.Upper))
	fmt.Fprintf(out, "Run time:             %s\n", formatRunDuration(rep.RuntimeSeconds))
	fmt.Fprintf(out, "Busy wall-clock time: median %.2fh (%d%% range %.2f-%.2fh)\n",
		est.Metrics.BusyHours.Median, est.Confidence, est.Metrics.BusyHours.Lower, est.Metrics.BusyHours.Upper)
	fmt.Fprintf(out, "Peak concurrency:     median %s (%d%% range %s-%s)\n",
		formatEstimateNumber(est.Metrics.PeakConcurrency.Median),
		est.Confidence,
		formatEstimateNumber(est.Metrics.PeakConcurrency.Lower),
		formatEstimateNumber(est.Metrics.PeakConcurrency.Upper))
	for _, key := range []string{"p50", "p90", "p95", "p99"} {
		interval := est.Metrics.PercentileConcurrency[key]
		fmt.Fprintf(out, "%s concurrency:       median %s (%d%% range %s-%s)\n",
			key,
			formatEstimateNumber(interval.Median),
			est.Confidence,
			formatEstimateNumber(interval.Lower),
			formatEstimateNumber(interval.Upper))
	}
	printScanSummary(out, rep.Scan)
	fmt.Fprintln(out, "\nEstimate notes:")
	fmt.Fprintln(out, "  These are sampled simulation intervals, not billing-grade exact measurements.")
	for _, warning := range est.Warnings {
		fmt.Fprintf(out, "  WARNING: %s\n", warning)
	}
}

func formatEstimateNumber(value float64) string {
	return comma(int(math.Round(value)))
}

func printScanSummary(out io.Writer, summary scanSummary) {
	fmt.Fprintln(out, "\nScan summary:")
	fmt.Fprintf(out, "  repositories: queued %d  scanned %d  skipped %d\n",
		summary.RepositoriesQueued, summary.RepositoriesScanned, summary.RepositoriesSkipped)
	fmt.Fprintf(out, "  workflow runs: %s  workflow jobs seen: %s  jobs used: %s\n",
		comma(summary.WorkflowRuns), comma(summary.WorkflowJobs), comma(summary.JobsUsed))
	fmt.Fprintf(out, "  API: %s requests  %s retries  %s rate-limit sleeps (%.1fs)\n",
		comma(summary.APIRequests), comma(summary.Retries), comma(summary.RateLimitSleeps), summary.RateLimitSleepSeconds)
	if len(summary.Conclusions) > 0 {
		var parts []string
		for _, key := range sortedStringKeys(summary.Conclusions) {
			parts = append(parts, key+"="+comma(summary.Conclusions[key]))
		}
		fmt.Fprintf(out, "  conclusions: %s\n", strings.Join(parts, ", "))
	}
	if len(summary.SkippedRepositories) > 0 {
		fmt.Fprintln(out, "  skipped repositories:")
		for _, skipped := range summary.SkippedRepositories {
			fmt.Fprintf(out, "    %s (%s)\n", skipped.Repo, skipped.Reason)
		}
	}
}

func printUsageSummaries(out io.Writer, title string, summaries []usageSummary) {
	if len(summaries) == 0 {
		return
	}
	fmt.Fprintf(out, "\n%s\n", title)
	for _, summary := range summaries {
		fmt.Fprintf(
			out,
			"  %-40s busy %7.2fh  peak %4d  p95 %4d  %8s jobs\n",
			truncate(summary.Name, 40),
			summary.BusyHours,
			summary.PeakConcurrency,
			summary.PercentileConcurrency["p95"],
			comma(summary.Jobs),
		)
	}
}

func truncate(value string, max int) string {
	if max < 1 || len(value) <= max {
		return value
	}
	if max <= 3 {
		return value[:max]
	}
	return value[:max-3] + "..."
}

func summarizeRepos(repos []string) string {
	if len(repos) == 0 {
		return "(none)"
	}
	if len(repos) <= 12 {
		return strings.Join(repos, ", ")
	}
	shown := append([]string{}, repos[:12]...)
	return strings.Join(shown, ", ") + fmt.Sprintf(", ... (%d total)", len(repos))
}

func sortedStringKeys[T any](m map[string]T) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func comma(n int) string {
	s := strconv.Itoa(n)
	if len(s) <= 3 {
		return s
	}
	var out []byte
	first := len(s) % 3
	if first == 0 {
		first = 3
	}
	out = append(out, s[:first]...)
	for i := first; i < len(s); i += 3 {
		out = append(out, ',')
		out = append(out, s[i:i+3]...)
	}
	return string(out)
}

func run(argv []string, stdout, stderr io.Writer) int {
	started := time.Now()
	cfg, err := parseArgs(argv, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if err := validateConfig(cfg); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 2
	}
	if cfg.showVer {
		fmt.Fprintf(stdout, "gh-concurrency %s (commit %s, built %s)\n", version, commit, date)
		return 0
	}

	token, err := resolveToken(cfg.token, cfg.baseURL)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	client := newGitHubClient(cfg.baseURL, token, cfg.maxRetries, cfg.verbose)
	client.setAPIWorkers(cfg.apiWorkers)
	client.debug = cfg.debug
	client.logWriter = stderr
	client.requestDelay = time.Duration(cfg.requestDelayMS) * time.Millisecond
	client.logf("resolving repository targets")
	targetRepos, skippedRepos, err := resolveTargetRepos(client, cfg, stderr)
	if err != nil {
		var ae authError
		if errors.As(err, &ae) {
			fmt.Fprintln(stderr, "error: 401 unauthorized. Check token scope and validity.")
			return 1
		}
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	if len(targetRepos) == 0 {
		fmt.Fprintln(stderr, "error: no repositories matched the requested targets.")
		return 1
	}
	client.logf("resolved %d repositories", len(targetRepos))
	cfg.repos = targetRepos

	if cfg.estimate {
		rep, err := runEstimate(client, cfg, skippedRepos, started, stderr)
		if err != nil {
			var ae authError
			if errors.As(err, &ae) {
				fmt.Fprintln(stderr, "error: 401 unauthorized. Check token scope and validity.")
				return 1
			}
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 1
		}
		if cfg.format == "json" {
			enc := json.NewEncoder(stdout)
			enc.SetIndent("", "  ")
			if err := enc.Encode(rep); err != nil {
				fmt.Fprintf(stderr, "error: %v\n", err)
				return 1
			}
			return 0
		}
		printText(stdout, rep)
		return 0
	}

	progress := newProgressReporter(stderr, cfg.verbose, len(cfg.repos))
	progress.Begin()
	records, summary, err := collectRepositories(client, cfg.repos, collectOptions{
		Since:               cfg.since,
		Until:               cfg.until,
		IncludeInProgress:   cfg.includeInProgress,
		JobFilter:           cfg.jobFilter,
		Branch:              cfg.branch,
		Event:               cfg.event,
		ExcludePullRequests: cfg.excludePullRequests,
		APIWorkers:          cfg.apiWorkers,
	}, progress, skippedRepos, stderr)
	progress.Complete()
	if err != nil {
		var ae authError
		if errors.As(err, &ae) {
			fmt.Fprintln(stderr, "error: 401 unauthorized. Check token scope and validity.")
			return 1
		}
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	if len(records) == 0 {
		fmt.Fprintln(stderr, "error: no completed jobs found in that window.")
		return 1
	}

	rep := buildReport(records, cfg, time.Since(started), summary, client.statsSnapshot())
	if cfg.format == "json" {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 1
		}
		return 0
	}
	printText(stdout, rep)
	return 0
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}
