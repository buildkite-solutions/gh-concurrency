package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

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
		Provider:        githubProvider,
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
