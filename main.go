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
	repos      []string
	since      string
	until      string
	baseURL    string
	token      string
	format     string
	maxRetries int
	verbose    bool
	showVer    bool
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
	baseURL := os.Getenv("GITHUB_API_URL")
	if baseURL == "" {
		baseURL = defaultBaseURL
	}

	fs := flag.NewFlagSet("gh-concurrency", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Var(&repos, "repo", "repository in OWNER/NAME form (repeatable; repos pool into one profile)")
	fs.StringVar(&cfg.since, "since", "", "lower bound on workflow-run creation date (YYYY-MM-DD)")
	fs.StringVar(&cfg.until, "until", "", "optional upper bound on workflow-run creation date (YYYY-MM-DD)")
	fs.StringVar(&cfg.baseURL, "base-url", baseURL, "API base URL. GHES: https://HOST/api/v3 (env: GITHUB_API_URL)")
	fs.StringVar(&cfg.token, "token", envToken(), "GitHub token (default: GITHUB_TOKEN or GH_TOKEN; gh auth fallback when available)")
	fs.StringVar(&cfg.format, "format", "text", "output format: text or json")
	fs.IntVar(&cfg.maxRetries, "max-retries", 6, "maximum HTTP retry attempts")
	fs.BoolVar(&cfg.verbose, "verbose", false, "progress and rate-limit logging to stderr")
	fs.BoolVar(&cfg.showVer, "version", false, "print version and exit")

	if err := fs.Parse(argv); err != nil {
		return cfg, err
	}
	cfg.repos = repos
	cfg.baseURL = strings.TrimRight(cfg.baseURL, "/")
	if cfg.maxRetries < 1 {
		cfg.maxRetries = 1
	}
	return cfg, nil
}

func validateConfig(cfg config) error {
	if cfg.showVer {
		return nil
	}
	if len(cfg.repos) == 0 {
		return errors.New("at least one --repo OWNER/NAME is required")
	}
	for _, repo := range cfg.repos {
		if strings.Count(repo, "/") != 1 {
			return fmt.Errorf("invalid repo %q; expected OWNER/NAME", repo)
		}
		parts := strings.Split(repo, "/")
		if parts[0] == "" || parts[1] == "" {
			return fmt.Errorf("invalid repo %q; expected OWNER/NAME", repo)
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
	u, err := url.Parse(cfg.baseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("invalid --base-url %q", cfg.baseURL)
	}
	return nil
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
	baseURL    string
	token      string
	maxRetries int
	timeout    time.Duration
	verbose    bool
	httpClient *http.Client
	sleep      func(time.Duration)
	now        func() time.Time
}

func newGitHubClient(baseURL, token string, maxRetries int, verbose bool) *githubClient {
	return &githubClient{
		baseURL:    strings.TrimRight(baseURL, "/"),
		token:      token,
		maxRetries: maxRetries,
		timeout:    30 * time.Second,
		verbose:    verbose,
		httpClient: &http.Client{Timeout: 30 * time.Second},
		sleep:      time.Sleep,
		now:        time.Now,
	}
}

func (c *githubClient) logf(format string, args ...interface{}) {
	if c.verbose {
		fmt.Fprintf(os.Stderr, "[gh-concurrency] "+format+"\n", args...)
	}
}

func (c *githubClient) request(rawURL string) ([]byte, string, error) {
	var lastErr error
	for attempt := 0; attempt < c.maxRetries; attempt++ {
		req, err := http.NewRequest(http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, "", err
		}
		req.Header.Set("Authorization", "Bearer "+c.token)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		req.Header.Set("User-Agent", "gh-concurrency/"+version)

		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = err
			c.logf("network error: %v; retrying", err)
			if attempt == c.maxRetries-1 {
				break
			}
			c.backoff(attempt)
			continue
		}

		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
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
					c.logf("%d: honoring Retry-After=%ds", resp.StatusCode, delay)
					c.sleep(time.Duration(delay+1) * time.Second)
					continue
				}
			}
			if resp.Header.Get("X-RateLimit-Remaining") == "0" {
				if reset := resp.Header.Get("X-RateLimit-Reset"); reset != "" && attempt < c.maxRetries-1 {
					if epoch, parseErr := strconv.ParseInt(reset, 10, 64); parseErr == nil {
						c.sleepUntil(epoch, "primary rate limit")
						continue
					}
				}
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

func (c *githubClient) sleepUntil(resetEpoch int64, reason string) {
	wait := time.Until(time.Unix(resetEpoch, 0)) + time.Second
	if c.now != nil {
		wait = time.Unix(resetEpoch, 0).Sub(c.now()) + time.Second
	}
	if wait < 0 {
		wait = time.Second
	}
	c.logf("%s: sleeping %.0fs until window resets", reason, wait.Seconds())
	c.sleep(wait)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func (c *githubClient) paginate(path string, params url.Values, itemsKey string, handle func(json.RawMessage) error) error {
	params = cloneValues(params)
	params.Set("per_page", "100")
	nextURL := c.baseURL + path + "?" + params.Encode()
	for nextURL != "" {
		body, link, err := c.request(nextURL)
		if err != nil {
			return err
		}
		items, err := extractItems(body, itemsKey)
		if err != nil {
			return err
		}
		for _, item := range items {
			if err := handle(item); err != nil {
				return err
			}
		}
		nextURL = nextLink(link)
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
	ID int64 `json:"id"`
}

type workflowJob struct {
	StartedAt   string   `json:"started_at"`
	CompletedAt string   `json:"completed_at"`
	CreatedAt   string   `json:"created_at"`
	Labels      []string `json:"labels"`
}

type record struct {
	Repo         string
	Start        time.Time
	End          time.Time
	QueueSeconds *float64
	OS           string
	SelfHosted   bool
}

func collectJobs(client *githubClient, repo, since, until string) ([]record, error) {
	created := ">=" + since
	if until != "" {
		created = since + ".." + until
	}

	var out []record
	params := url.Values{"created": []string{created}}
	err := client.paginate("/repos/"+repo+"/actions/runs", params, "workflow_runs", func(raw json.RawMessage) error {
		var run workflowRun
		if err := json.Unmarshal(raw, &run); err != nil {
			return err
		}
		return client.paginate("/repos/"+repo+"/actions/runs/"+strconv.FormatInt(run.ID, 10)+"/jobs", nil, "jobs", func(rawJob json.RawMessage) error {
			var job workflowJob
			if err := json.Unmarshal(rawJob, &job); err != nil {
				return err
			}
			rec, err := normalizeJob(job, repo)
			if err != nil {
				return err
			}
			if rec != nil {
				out = append(out, *rec)
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
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
		Repo:         repo,
		Start:        start,
		End:          end,
		QueueSeconds: queueSeconds,
		OS:           inferOS(job.Labels),
		SelfHosted:   isSelfHosted(job.Labels),
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

type queueStats struct {
	Count   int     `json:"count"`
	MedianS float64 `json:"median_s"`
	P95S    float64 `json:"p95_s"`
	MaxS    float64 `json:"max_s"`
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
	Repos   []string `json:"repos"`
	Since   string   `json:"since"`
	Until   string   `json:"until,omitempty"`
	BaseURL string   `json:"base_url"`
}

type report struct {
	Tool                    string                  `json:"tool"`
	Version                 string                  `json:"version"`
	GeneratedAt             string                  `json:"generated_at"`
	Parameters              parameters              `json:"parameters"`
	JobsAnalyzed            int                     `json:"jobs_analyzed"`
	BusyHours               float64                 `json:"busy_hours"`
	PeakConcurrency         int                     `json:"peak_concurrency"`
	PercentileConcurrency   map[string]int          `json:"percentile_concurrency"`
	BillableMinutesEstimate map[string]billableSlot `json:"billable_minutes_estimate"`
	QueueSeconds            *queueStats             `json:"queue_seconds"`
	Warnings                []string                `json:"warnings"`
}

func buildReport(records []record, cfg config) report {
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
	return report{
		Tool:        "gh-concurrency",
		Version:     version,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Parameters: parameters{
			Repos:   cfg.repos,
			Since:   cfg.since,
			Until:   cfg.until,
			BaseURL: cfg.baseURL,
		},
		JobsAnalyzed:            len(records),
		BusyHours:               math.Round((busySeconds/3600.0)*100) / 100,
		PeakConcurrency:         peak,
		PercentileConcurrency:   map[string]int{"p50": pct[50], "p90": pct[90], "p95": pct[95], "p99": pct[99]},
		BillableMinutesEstimate: billableMinutes(records),
		QueueSeconds:            qstats,
		Warnings:                detectWarnings(peak, pct, qstats),
	}
}

func printText(out io.Writer, rep report) {
	p := rep.Parameters
	until := p.Until
	if until == "" {
		until = "now"
	}
	fmt.Fprintf(out, "\ngh-concurrency v%s\n", rep.Version)
	fmt.Fprintf(out, "repos:  %s\n", strings.Join(p.Repos, ", "))
	fmt.Fprintf(out, "window: %s -> %s   api: %s\n", p.Since, until, p.BaseURL)
	fmt.Fprintf(out, "\nJobs analyzed:        %d\n", rep.JobsAnalyzed)
	fmt.Fprintf(out, "Busy wall-clock time: %.2fh (>=1 job running)\n", rep.BusyHours)
	fmt.Fprintf(out, "Peak concurrency:     %d\n", rep.PeakConcurrency)
	for _, key := range []string{"p50", "p90", "p95", "p99"} {
		fmt.Fprintf(out, "%s concurrency:       %d\n", key, rep.PercentileConcurrency[key])
	}

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
	var records []record
	for _, repo := range cfg.repos {
		client.logf("fetching %s since %s", repo, cfg.since)
		repoRecords, err := collectJobs(client, repo, cfg.since, cfg.until)
		if err != nil {
			var nf notFoundError
			var ae authError
			switch {
			case errors.As(err, &nf):
				fmt.Fprintf(stderr, "warning: %s not found or no Actions access; skipping.\n", repo)
				continue
			case errors.As(err, &ae):
				fmt.Fprintln(stderr, "error: 401 unauthorized. Check token scope and validity.")
				return 1
			default:
				fmt.Fprintf(stderr, "error: %v\n", err)
				return 1
			}
		}
		records = append(records, repoRecords...)
	}

	if len(records) == 0 {
		fmt.Fprintln(stderr, "error: no completed jobs found in that window.")
		return 1
	}

	rep := buildReport(records, cfg)
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
