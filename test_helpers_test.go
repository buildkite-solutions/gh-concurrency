package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
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

func rec(seconds int, osName string, selfHosted bool) record {
	return record{
		Repo:       "x/y",
		Start:      dt("10:00:00"),
		End:        dt("10:00:00").Add(time.Duration(seconds) * time.Second),
		OS:         osName,
		SelfHosted: selfHosted,
	}
}

func workflowRunIDs(runs []workflowRun) string {
	var ids []string
	for _, run := range runs {
		ids = append(ids, strconv.FormatInt(run.ID, 10))
	}
	return strings.Join(ids, ",")
}

func recordSignature(records []record) string {
	var parts []string
	for _, rec := range records {
		parts = append(parts, strings.Join([]string{
			rec.Repo,
			rec.WorkflowName,
			rec.JobName,
			rec.Start.Format(time.RFC3339),
			rec.End.Format(time.RFC3339),
		}, "|"))
	}
	return strings.Join(parts, "\n")
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
