package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type circleCIClient struct {
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
	verbose          bool
	debug            bool
	logWriter        io.Writer
	httpClient       *http.Client
	sleep            func(time.Duration)
	now              func() time.Time
}

func newCircleCIClient(baseURL, token string, maxRetries int, verbose bool) *circleCIClient {
	return &circleCIClient{
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

func (c *circleCIClient) setAPIWorkers(workers int) {
	if workers < 1 {
		workers = 1
	}
	if workers > 32 {
		workers = 32
	}
	c.apiSlots = make(chan struct{}, workers)
}

func (c *circleCIClient) statsSnapshot() requestStats {
	c.statsMu.Lock()
	defer c.statsMu.Unlock()
	out := c.stats
	out.RateLimitSleepSeconds = math.Round(out.RateLimitSleepSeconds*1000) / 1000
	return out
}

func (c *circleCIClient) recordRequest() {
	c.statsMu.Lock()
	c.stats.Requests++
	c.statsMu.Unlock()
}

func (c *circleCIClient) recordRetry() {
	c.statsMu.Lock()
	c.stats.Retries++
	c.statsMu.Unlock()
}

func (c *circleCIClient) recordRateLimitSleep(delay time.Duration) {
	c.statsMu.Lock()
	c.stats.RateLimitSleeps++
	c.stats.RateLimitSleepSeconds += delay.Seconds()
	c.statsMu.Unlock()
}

func (c *circleCIClient) acquireAPISlot() func() {
	if c.apiSlots == nil {
		return func() {}
	}
	c.apiSlots <- struct{}{}
	return func() {
		<-c.apiSlots
	}
}

func (c *circleCIClient) waitForRequestStart() {
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

func (c *circleCIClient) pauseRequests(delay time.Duration, reason string) {
	if delay < time.Second {
		delay = time.Second
	}
	c.throttleMu.Lock()
	defer c.throttleMu.Unlock()
	c.recordRateLimitSleep(delay)
	c.logf("%s: pausing CircleCI API requests for %.0fs", reason, delay.Seconds())
	c.sleep(delay)
	c.lastRequestStart = c.currentTime()
}

func (c *circleCIClient) currentTime() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

func (c *circleCIClient) logf(format string, args ...interface{}) {
	if c.verbose || c.debug {
		fmt.Fprintf(c.logOutput(), "[gh-concurrency] "+format+"\n", args...)
	}
}

func (c *circleCIClient) debugf(format string, args ...interface{}) {
	if c.debug {
		fmt.Fprintf(c.logOutput(), "[gh-concurrency debug] "+format+"\n", args...)
	}
}

func (c *circleCIClient) logOutput() io.Writer {
	if c.logWriter != nil {
		return c.logWriter
	}
	return io.Discard
}

func (c *circleCIClient) getJSON(path string, params url.Values, into any) error {
	rawURL := c.baseURL + path
	if len(params) > 0 {
		rawURL += "?" + params.Encode()
	}
	body, err := c.request(rawURL)
	if err != nil {
		return err
	}
	return json.Unmarshal(body, into)
}

func paginateCircleCIItems[T any](client *circleCIClient, path string, params url.Values, handle func(T) error) error {
	pageToken := ""
	page := 1
	for {
		currentParams := cloneValues(params)
		if pageToken != "" {
			currentParams.Set("page-token", pageToken)
		}
		var resp circleCIItemPage[T]
		if err := client.getJSON(path, currentParams, &resp); err != nil {
			return err
		}
		client.debugf("page %d %s returned %d CircleCI items", page, circleCIPathForLog(path, currentParams), len(resp.Items))
		for _, item := range resp.Items {
			if err := handle(item); err != nil {
				return err
			}
		}
		if resp.NextPageToken == "" {
			break
		}
		pageToken = resp.NextPageToken
		page++
	}
	return nil
}

func (c *circleCIClient) request(rawURL string) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < c.maxRetries; attempt++ {
		if attempt > 0 {
			c.recordRetry()
		}
		req, err := http.NewRequest(http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+c.token)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "gh-concurrency/"+version)

		c.debugf("GET %s (attempt %d/%d)", requestURLForLog(rawURL), attempt+1, c.maxRetries)
		release := c.acquireAPISlot()
		c.waitForRequestStart()
		c.recordRequest()
		resp, err := c.httpClient.Do(req)
		if err != nil {
			release()
			lastErr = err
			c.logf("CircleCI network error: %v; retrying", err)
			if attempt == c.maxRetries-1 {
				break
			}
			c.backoff(attempt)
			continue
		}
		c.debugResponse(rawURL, resp)

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
			return body, nil
		}

		err = c.httpError(resp, body)
		switch {
		case resp.StatusCode == http.StatusNotFound:
			return nil, notFoundError{URL: rawURL}
		case resp.StatusCode == http.StatusUnauthorized:
			return nil, authError{}
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusForbidden:
			if retryAfter := resp.Header.Get("Retry-After"); retryAfter != "" {
				delay, parseErr := strconv.Atoi(retryAfter)
				if parseErr == nil && attempt < c.maxRetries-1 {
					c.logf("%d: honoring CircleCI Retry-After=%ds", resp.StatusCode, delay)
					c.pauseRequests(time.Duration(delay+1)*time.Second, "CircleCI Retry-After")
					continue
				}
			}
			if attempt < c.maxRetries-1 && resp.StatusCode == http.StatusTooManyRequests {
				c.backoff(attempt)
				continue
			}
			return nil, err
		case resp.StatusCode >= 500:
			lastErr = err
			if attempt == c.maxRetries-1 {
				break
			}
			c.backoff(attempt)
			continue
		default:
			return nil, err
		}
	}
	if lastErr == nil {
		lastErr = errors.New("unknown request failure")
	}
	return nil, fmt.Errorf("exhausted %d retries for %s: %w", c.maxRetries, rawURL, lastErr)
}

func (c *circleCIClient) debugResponse(rawURL string, resp *http.Response) {
	if !c.debug {
		return
	}
	var details []string
	if retryAfter := resp.Header.Get("Retry-After"); retryAfter != "" {
		details = append(details, "retry-after="+retryAfter+"s")
	}
	suffix := ""
	if len(details) > 0 {
		suffix = " (" + strings.Join(details, ", ") + ")"
	}
	c.debugf("%s -> %s%s", requestURLForLog(rawURL), resp.Status, suffix)
}

func (c *circleCIClient) httpError(resp *http.Response, body []byte) error {
	msg := strings.TrimSpace(string(bytes.TrimSpace(body)))
	if len(msg) > 500 {
		msg = msg[:500] + "..."
	}
	if msg == "" {
		msg = resp.Status
	}
	return fmt.Errorf("CircleCI API %s: %s", resp.Status, msg)
}

func (c *circleCIClient) backoff(attempt int) {
	base := time.Duration(1<<uint(min(attempt, 6))) * time.Second
	jitter := time.Duration(cryptoIntn(1000)) * time.Millisecond
	delay := base + jitter
	c.logf("CircleCI backing off %.1fs (attempt %d)", delay.Seconds(), attempt+1)
	c.sleep(delay)
}
