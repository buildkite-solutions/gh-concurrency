package main

import (
	"encoding/json"
	"io"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func dt(hms string) time.Time {
	parts := strings.Split(hms, ":")
	hour := atoi(parts[0])
	minute := atoi(parts[1])
	second := atoi(parts[2])
	return time.Date(2025, 5, 1, hour, minute, second, 0, time.UTC)
}

func atoi(value string) int {
	n := 0
	for _, ch := range value {
		n = n*10 + int(ch-'0')
	}
	return n
}

func TestConcurrencyProfileEmpty(t *testing.T) {
	peak, profile := concurrencyProfile(nil)
	if peak != 0 {
		t.Fatalf("peak = %d, want 0", peak)
	}
	if len(profile) != 0 {
		t.Fatalf("profile = %v, want empty", profile)
	}
}

func TestConcurrencyProfileSingleJob(t *testing.T) {
	peak, _ := concurrencyProfile([][2]time.Time{{dt("10:00:00"), dt("10:10:00")}})
	if peak != 1 {
		t.Fatalf("peak = %d, want 1", peak)
	}
}

func TestConcurrencyProfileFullOverlap(t *testing.T) {
	peak, _ := concurrencyProfile([][2]time.Time{
		{dt("10:00:00"), dt("10:00:10")},
		{dt("10:00:05"), dt("10:00:15")},
	})
	if peak != 2 {
		t.Fatalf("peak = %d, want 2", peak)
	}
}

func TestConcurrencyProfileHandoffNotDoubleCounted(t *testing.T) {
	peak, _ := concurrencyProfile([][2]time.Time{
		{dt("10:00:00"), dt("10:00:01")},
		{dt("10:00:01"), dt("10:00:02")},
	})
	if peak != 1 {
		t.Fatalf("peak = %d, want 1", peak)
	}
}

func TestConcurrencyProfileZeroDurationIgnored(t *testing.T) {
	peak, profile := concurrencyProfile([][2]time.Time{{dt("10:00:00"), dt("10:00:00")}})
	if peak != 0 || len(profile) != 0 {
		t.Fatalf("peak/profile = %d/%v, want 0/empty", peak, profile)
	}
}

func TestConcurrencyProfileNestedIntervals(t *testing.T) {
	peak, _ := concurrencyProfile([][2]time.Time{
		{dt("10:00:00"), dt("10:00:30")},
		{dt("10:00:05"), dt("10:00:20")},
		{dt("10:00:10"), dt("10:00:25")},
	})
	if peak != 3 {
		t.Fatalf("peak = %d, want 3", peak)
	}
}

func TestConcurrencyProfileTimeAtLevel(t *testing.T) {
	_, profile := concurrencyProfile([][2]time.Time{
		{dt("10:00:00"), dt("10:00:10")},
		{dt("10:00:05"), dt("10:00:15")},
	})
	if profile[1] != 10 {
		t.Fatalf("profile[1] = %v, want 10", profile[1])
	}
	if profile[2] != 5 {
		t.Fatalf("profile[2] = %v, want 5", profile[2])
	}
}

func TestConcurrencyProfileMatchesBruteforceGrid(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	var intervals [][2]time.Time
	for i := 0; i < 50; i++ {
		start := rng.Intn(600)
		duration := rng.Intn(120) + 1
		intervals = append(intervals, [2]time.Time{
			dt("10:00:00").Add(time.Duration(start) * time.Second),
			dt("10:00:00").Add(time.Duration(start+duration) * time.Second),
		})
	}
	peak, _ := concurrencyProfile(intervals)
	gridPeak := 0
	for sec := 0; sec < 800; sec++ {
		tick := dt("10:00:00").Add(time.Duration(sec) * time.Second)
		count := 0
		for _, interval := range intervals {
			if (tick.Equal(interval[0]) || tick.After(interval[0])) && tick.Before(interval[1]) {
				count++
			}
		}
		if count > gridPeak {
			gridPeak = count
		}
	}
	if peak != gridPeak {
		t.Fatalf("peak = %d, want %d", peak, gridPeak)
	}
}

func TestPercentilesEmpty(t *testing.T) {
	got := percentiles(nil, []int{50, 90, 95, 99})
	for _, p := range []int{50, 90, 95, 99} {
		if got[p] != 0 {
			t.Fatalf("p%d = %d, want 0", p, got[p])
		}
	}
}

func TestPercentilesWeighted(t *testing.T) {
	got := percentiles(map[int]float64{1: 90, 5: 10}, []int{50, 95})
	if got[50] != 1 {
		t.Fatalf("p50 = %d, want 1", got[50])
	}
	if got[95] != 5 {
		t.Fatalf("p95 = %d, want 5", got[95])
	}
}

func rec(seconds int, osName string, selfHosted bool) record {
	return record{
		Repo:       "x/y",
		Start:      dt("10:00:00"),
		End:        dt("10:00:00").Add(time.Duration(seconds) * time.Second),
		OS:         osName,
		SelfHosted: selfHosted,
	}
}

func TestBillableMinutesRoundsUp(t *testing.T) {
	got := billableMinutes([]record{rec(61, "linux", false)})
	if got["linux"].BillableMinutes != 2 {
		t.Fatalf("billable minutes = %d, want 2", got["linux"].BillableMinutes)
	}
}

func TestBillableMinutesMacOSMultiplier(t *testing.T) {
	got := billableMinutes([]record{rec(60, "macos", false)})
	if got["macos"].BillableMinutes != 10 {
		t.Fatalf("billable minutes = %d, want 10", got["macos"].BillableMinutes)
	}
}

func TestBillableMinutesSelfHostedIsFree(t *testing.T) {
	got := billableMinutes([]record{rec(600, "linux", true)})
	if len(got) != 0 {
		t.Fatalf("billable = %v, want empty", got)
	}
}

func TestNextLinkPresent(t *testing.T) {
	header := `<https://api.github.com/x?page=2>; rel="next", <https://api.github.com/x?page=9>; rel="last"`
	got := nextLink(header)
	if got != "https://api.github.com/x?page=2" {
		t.Fatalf("nextLink = %q", got)
	}
}

func TestNextLinkMissing(t *testing.T) {
	if got := nextLink(`<https://api.github.com/x?page=1>; rel="prev"`); got != "" {
		t.Fatalf("nextLink = %q, want empty", got)
	}
}

func TestNextLinkEmpty(t *testing.T) {
	if got := nextLink(""); got != "" {
		t.Fatalf("nextLink = %q, want empty", got)
	}
}

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
		"/orgs/acme/repos": {
			body: []map[string]any{
				{"full_name": "acme/api"},
				{"full_name": "acme/web"},
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

	got, err := resolveTargetRepos(client, config{
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
}

func TestPaginationAndCollectionOfflineReplay(t *testing.T) {
	responses := map[string]fakeResponse{
		"/repos/o/r/actions/runs": {
			body: map[string]any{"workflow_runs": []map[string]any{{"id": 1}, {"id": 2}}},
		},
		"/repos/o/r/actions/runs/1/jobs": {
			body: map[string]any{"jobs": []map[string]any{
				{
					"started_at":   "2025-05-01T10:00:00Z",
					"completed_at": "2025-05-01T10:05:00Z",
					"created_at":   "2025-05-01T09:59:00Z",
					"labels":       []string{"ubuntu-latest"},
				},
			}},
		},
		"/repos/o/r/actions/runs/2/jobs": {
			body: map[string]any{"jobs": []map[string]any{
				{
					"started_at":   "2025-05-01T10:02:00Z",
					"completed_at": "2025-05-01T10:08:00Z",
					"created_at":   "2025-05-01T10:02:00Z",
					"labels":       []string{"windows-latest"},
				},
				{
					"started_at":   nil,
					"completed_at": nil,
					"labels":       []string{},
				},
			}},
		},
	}
	client := newGitHubClient("https://api.github.com", "tok", 1, false)
	client.httpClient = &http.Client{Transport: fakeTransport{responses: responses}}
	client.sleep = func(time.Duration) {}

	records, err := collectJobs(client, "o/r", "2025-05-01", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("records = %d, want 2", len(records))
	}
	oses := map[string]bool{}
	for _, rec := range records {
		oses[rec.OS] = true
	}
	if !oses["linux"] || !oses["windows"] {
		t.Fatalf("OSes = %v, want linux and windows", oses)
	}
	peak, _ := concurrencyProfile([][2]time.Time{
		{records[0].Start, records[0].End},
		{records[1].Start, records[1].End},
	})
	if peak != 2 {
		t.Fatalf("peak = %d, want 2", peak)
	}
}

func TestSecondaryRateLimitBackoffRetries(t *testing.T) {
	calls := 0
	var sleeps []time.Duration
	client := newGitHubClient("https://api.github.com", "tok", 2, false)
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
}

type fakeResponse struct {
	body any
	link string
}

type fakeTransport struct {
	responses map[string]fakeResponse
}

func (t fakeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, ok := t.responses[req.URL.Path]
	if !ok {
		return fakeHTTPResponse(http.StatusNotFound, "404 Not Found", `{"message":"missing"}`, nil), nil
	}
	data, err := json.Marshal(resp.body)
	if err != nil {
		return nil, err
	}
	headers := make(http.Header)
	if resp.link != "" {
		headers.Set("Link", resp.link)
	}
	return fakeHTTPResponse(http.StatusOK, "200 OK", string(data), headers), nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func fakeHTTPResponse(statusCode int, status string, body string, headers http.Header) *http.Response {
	if headers == nil {
		headers = make(http.Header)
	}
	return &http.Response{
		StatusCode: statusCode,
		Status:     status,
		Header:     headers,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}
