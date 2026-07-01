package main

import (
	"bytes"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRequestBudgetStopsBeforeMaxRequests(t *testing.T) {
	client := newGitHubClient("https://api.github.com", "tok", 1, false)
	client.enableRequestBudget(1, 0)
	client.httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return fakeHTTPResponse(http.StatusOK, "200 OK", `{"ok":true}`, nil), nil
	})}
	client.sleep = func(time.Duration) {}

	if _, _, err := client.request("https://api.github.com/one"); err != nil {
		t.Fatal(err)
	}
	_, _, err := client.request("https://api.github.com/two")
	var stopErr requestBudgetStopError
	if !errors.As(err, &stopErr) {
		t.Fatalf("err = %v, want requestBudgetStopError", err)
	}
	if !strings.Contains(stopErr.Reason, "budget") {
		t.Fatalf("stop reason = %q, want budget", stopErr.Reason)
	}
}

func TestSecondaryRateLimitBackoffRetries(t *testing.T) {
	calls := 0
	var sleeps []time.Duration
	client := newGitHubClient("https://api.github.com", "tok", 2, false)
	client.setAPIWorkers(2)
	client.httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return fakeHTTPResponse(http.StatusForbidden, "403 Forbidden", `{"message":"You have exceeded a secondary rate limit"}`, nil), nil
		}
		return fakeHTTPResponse(http.StatusOK, "200 OK", `{"ok":true}`, nil), nil
	})}
	client.sleep = func(d time.Duration) {
		sleeps = append(sleeps, d)
	}

	body, _, err := client.request("https://api.github.com/x")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(body)) != `{"ok":true}` {
		t.Fatalf("body = %s", body)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
	if len(sleeps) != 1 || sleeps[0] < time.Minute {
		t.Fatalf("sleeps = %v, want one secondary backoff >= 1m", sleeps)
	}
	stats := client.statsSnapshot()
	if stats.Requests != 2 || stats.Retries != 1 || stats.RateLimitSleeps != 1 {
		t.Fatalf("stats = %#v, want 2 requests, 1 retry, 1 rate-limit sleep", stats)
	}
}

func TestRetryAfterPausesSharedRequestGate(t *testing.T) {
	headers := make(http.Header)
	headers.Set("Retry-After", "2")

	var mu sync.Mutex
	calls := 0
	client := newGitHubClient("https://api.github.com", "tok", 2, false)
	client.setAPIWorkers(2)
	client.httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		mu.Lock()
		calls++
		call := calls
		mu.Unlock()
		if call == 1 {
			return fakeHTTPResponse(http.StatusForbidden, "403 Forbidden", `{"message":"slow down"}`, headers), nil
		}
		return fakeHTTPResponse(http.StatusOK, "200 OK", `{"ok":true}`, nil), nil
	})}

	sleepStarted := make(chan struct{})
	releaseSleep := make(chan struct{})
	var sleepOnce sync.Once
	client.sleep = func(d time.Duration) {
		if d >= 3*time.Second {
			sleepOnce.Do(func() { close(sleepStarted) })
			<-releaseSleep
		}
	}

	firstDone := make(chan error, 1)
	go func() {
		_, _, err := client.request("https://api.github.com/first")
		firstDone <- err
	}()
	<-sleepStarted

	secondDone := make(chan error, 1)
	go func() {
		_, _, err := client.request("https://api.github.com/second")
		secondDone <- err
	}()

	select {
	case err := <-secondDone:
		t.Fatalf("second request completed during shared cooldown: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(releaseSleep)

	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
	stats := client.statsSnapshot()
	if stats.RateLimitSleeps != 1 || stats.RateLimitSleepSeconds != 3 {
		t.Fatalf("stats = %#v, want one 3s rate-limit sleep", stats)
	}
}

func TestClientLimitsConcurrentRequests(t *testing.T) {
	var mu sync.Mutex
	inFlight := 0
	maxInFlight := 0
	client := newGitHubClient("https://api.github.com", "tok", 1, false)
	client.setAPIWorkers(2)
	client.httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		mu.Lock()
		inFlight++
		if inFlight > maxInFlight {
			maxInFlight = inFlight
		}
		mu.Unlock()

		time.Sleep(20 * time.Millisecond)

		mu.Lock()
		inFlight--
		mu.Unlock()
		return fakeHTTPResponse(http.StatusOK, "200 OK", `{"ok":true}`, nil), nil
	})}

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, err := client.request("https://api.github.com/x/" + strconv.Itoa(i))
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if maxInFlight > 2 {
		t.Fatalf("max in-flight requests = %d, want <= 2", maxInFlight)
	}
}

func TestDebugRequestLogging(t *testing.T) {
	var logs bytes.Buffer
	headers := make(http.Header)
	headers.Set("X-RateLimit-Remaining", "4999")
	headers.Set("X-RateLimit-Reset", "1746122400")

	client := newGitHubClient("https://api.github.com", "tok", 1, false)
	client.debug = true
	client.logWriter = &logs
	client.httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return fakeHTTPResponse(http.StatusOK, "200 OK", `{"ok":true}`, headers), nil
	})}
	client.sleep = func(time.Duration) {}

	if _, _, err := client.request("https://api.github.com/repos/o/r/actions/runs?per_page=100"); err != nil {
		t.Fatal(err)
	}
	out := logs.String()
	for _, want := range []string{
		"GET https://api.github.com/repos/o/r/actions/runs?per_page=100",
		"200 OK",
		"remaining=4999",
		"reset=2025-05-01T18:00:00Z",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("debug log missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "tok") {
		t.Fatalf("debug log leaked token:\n%s", out)
	}
}
