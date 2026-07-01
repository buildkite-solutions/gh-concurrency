package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

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
