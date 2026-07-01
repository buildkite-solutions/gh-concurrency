package main

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

type circleCIPipeline struct {
	ID          string `json:"id"`
	ProjectSlug string `json:"project_slug"`
	Number      int    `json:"number"`
	State       string `json:"state"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
	VCS         struct {
		Branch string `json:"branch"`
		Tag    string `json:"tag"`
	} `json:"vcs"`
	Trigger struct {
		Type string `json:"type"`
	} `json:"trigger"`
}

type circleCIWorkflow struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
	StoppedAt string `json:"stopped_at"`
}

type circleCIJob struct {
	ID           string `json:"id"`
	JobNumber    int    `json:"job_number"`
	Number       int    `json:"number"`
	StartedAt    string `json:"started_at"`
	StoppedAt    string `json:"stopped_at"`
	QueuedAt     string `json:"queued_at"`
	CreatedAt    string `json:"created_at"`
	Name         string `json:"name"`
	Status       string `json:"status"`
	Type         string `json:"type"`
	ProjectSlug  string `json:"project_slug"`
	Parallelism  int    `json:"parallelism"`
	ParallelRuns []struct {
		Index  int    `json:"index"`
		Status string `json:"status"`
	} `json:"parallel_runs"`
	Executor struct {
		ResourceClass string `json:"resource_class"`
		Type          string `json:"type"`
	} `json:"executor"`
	LatestWorkflow struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"latest_workflow"`
}

type circleCIItemPage[T any] struct {
	Items         []T    `json:"items"`
	NextPageToken string `json:"next_page_token"`
}

func collectCircleCIProjects(client *circleCIClient, projects []string, opts collectOptions, progress *progressReporter, stderr io.Writer) ([]record, scanSummary, error) {
	summary := scanSummary{
		RepositoriesQueued: len(projects),
		Conclusions:        map[string]int{},
	}
	workers := boundedWorkerCount(opts.APIWorkers, len(projects))
	projectCh := make(chan string)
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
	addSkipped := func(project, reason string) {
		mu.Lock()
		defer mu.Unlock()
		summary.SkippedRepositories = append(summary.SkippedRepositories, skippedRepository{Repo: project, Reason: reason})
	}
	addResult := func(result repoScanResult) {
		mu.Lock()
		defer mu.Unlock()
		summary.RepositoriesScanned++
		summary.Pipelines += result.Pipelines
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
			for project := range projectCh {
				if hasFatal() {
					continue
				}
				progress.Start(project)
				result, err := collectCircleCIProjectJobs(client, project, opts)
				if err != nil {
					var nf notFoundError
					var ae authError
					switch {
					case errors.As(err, &nf):
						fmt.Fprintf(stderr, "warning: %s not found or no CircleCI project access; skipping.\n", project)
						addSkipped(project, "not found or no CircleCI project access")
						progress.Skip(project)
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
				progress.Done(project, len(result.Records))
			}
		}()
	}

	for _, project := range projects {
		if hasFatal() {
			break
		}
		projectCh <- project
	}
	close(projectCh)
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

func collectCircleCIProjectJobs(client *circleCIClient, project string, opts collectOptions) (repoScanResult, error) {
	result := repoScanResult{Repo: project}
	client.logf("%s: listing CircleCI pipelines", project)
	pipelines, err := listCircleCIPipelines(client, project, opts)
	if err != nil {
		return result, err
	}
	result.Pipelines = len(pipelines)
	if len(pipelines) == 0 {
		client.logf("%s: 0 pipelines, 0 workflows, 0 workflow jobs, 0 jobs used", project)
		return result, nil
	}

	var workflows []circleCIWorkflow
	for _, pipeline := range pipelines {
		pipelineWorkflows, err := listCircleCIWorkflows(client, pipeline.ID)
		if err != nil {
			return result, err
		}
		workflows = append(workflows, pipelineWorkflows...)
	}
	result.WorkflowRuns = len(workflows)
	if len(workflows) == 0 {
		client.logf("%s: %d pipelines, 0 workflows, 0 workflow jobs, 0 jobs used", project, result.Pipelines)
		return result, nil
	}

	workers := boundedWorkerCount(opts.APIWorkers, len(workflows))
	workflowCh := make(chan circleCIWorkflow)
	resultCh := make(chan repoScanResult, workers)
	errCh := make(chan error, workers)
	var wg sync.WaitGroup

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for workflow := range workflowCh {
				workflowResult, err := collectCircleCIWorkflowJobs(client, project, workflow, opts)
				if err != nil {
					errCh <- err
					continue
				}
				resultCh <- workflowResult
			}
		}()
	}

	go func() {
		for _, workflow := range workflows {
			workflowCh <- workflow
		}
		close(workflowCh)
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
		case workflowResult, ok := <-resultCh:
			if !ok {
				resultCh = nil
				continue
			}
			result.WorkflowJobs += workflowResult.WorkflowJobs
			result.JobsUsed += workflowResult.JobsUsed
			result.Records = append(result.Records, workflowResult.Records...)
		}
	}
	if firstErr != nil {
		return result, firstErr
	}

	sortRecords(result.Records)
	client.logf("%s: %d pipelines, %d workflows, %d workflow jobs, %d jobs used", project, result.Pipelines, result.WorkflowRuns, result.WorkflowJobs, result.JobsUsed)
	return result, nil
}

func listCircleCIPipelines(client *circleCIClient, project string, opts collectOptions) ([]circleCIPipeline, error) {
	start, end, hasEnd, err := dateWindow(opts.Since, opts.Until)
	if err != nil {
		return nil, err
	}
	params := url.Values{}
	if opts.Branch != "" {
		params.Set("branch", opts.Branch)
	}
	path := circleCIProjectPath(project) + "/pipeline"
	pageToken := ""
	page := 0
	var pipelines []circleCIPipeline
	for {
		page++
		currentParams := cloneValues(params)
		if pageToken != "" {
			currentParams.Set("page-token", pageToken)
		}
		var resp circleCIItemPage[circleCIPipeline]
		if err := client.getJSON(path, currentParams, &resp); err != nil {
			return nil, err
		}
		client.debugf("page %d %s returned %d CircleCI pipelines", page, circleCIPathForLog(path, currentParams), len(resp.Items))

		allBeforeSince := len(resp.Items) > 0
		for _, pipeline := range resp.Items {
			if pipeline.ProjectSlug == "" {
				pipeline.ProjectSlug = project
			}
			created, ok, err := parseOptionalTime(pipeline.CreatedAt, "CircleCI pipeline created_at")
			if err != nil {
				return nil, err
			}
			if !ok {
				allBeforeSince = false
				pipelines = append(pipelines, pipeline)
				continue
			}
			if !created.Before(start) {
				allBeforeSince = false
			}
			if created.Before(start) {
				continue
			}
			if hasEnd && !created.Before(end) {
				continue
			}
			pipelines = append(pipelines, pipeline)
		}

		if resp.NextPageToken == "" {
			break
		}
		if opts.CircleCIMaxPages > 0 && page >= opts.CircleCIMaxPages {
			break
		}
		if allBeforeSince {
			break
		}
		pageToken = resp.NextPageToken
	}
	return pipelines, nil
}

func listCircleCIWorkflows(client *circleCIClient, pipelineID string) ([]circleCIWorkflow, error) {
	if strings.TrimSpace(pipelineID) == "" {
		return nil, errors.New("CircleCI pipeline did not include an id")
	}
	path := "/pipeline/" + url.PathEscape(pipelineID) + "/workflow"
	var workflows []circleCIWorkflow
	err := paginateCircleCIItems(client, path, url.Values{}, func(workflow circleCIWorkflow) error {
		workflows = append(workflows, workflow)
		return nil
	})
	return workflows, err
}

func collectCircleCIWorkflowJobs(client *circleCIClient, project string, workflow circleCIWorkflow, opts collectOptions) (repoScanResult, error) {
	result := repoScanResult{Repo: project}
	if strings.TrimSpace(workflow.ID) == "" {
		return result, errors.New("CircleCI workflow did not include an id")
	}
	path := "/workflow/" + url.PathEscape(workflow.ID) + "/job"
	err := paginateCircleCIItems(client, path, url.Values{}, func(job circleCIJob) error {
		result.WorkflowJobs++
		if job.ProjectSlug == "" {
			job.ProjectSlug = project
		}
		if opts.CircleCIJobDetails {
			number := circleCIJobNumber(job)
			if number > 0 {
				details, err := getCircleCIJobDetails(client, job.ProjectSlug, number)
				if err != nil {
					var nf notFoundError
					if !errors.As(err, &nf) {
						return err
					}
				} else {
					job = mergeCircleCIJob(job, details)
				}
			}
		}
		records, err := normalizeCircleCIJob(job, project, workflow.Name)
		if err != nil {
			return err
		}
		result.JobsUsed += len(records)
		result.Records = append(result.Records, records...)
		return nil
	})
	return result, err
}

func getCircleCIJobDetails(client *circleCIClient, project string, jobNumber int) (circleCIJob, error) {
	var job circleCIJob
	if strings.TrimSpace(project) == "" {
		return job, errors.New("CircleCI job did not include a project slug")
	}
	path := circleCIProjectPath(project) + "/job/" + strconv.Itoa(jobNumber)
	if err := client.getJSON(path, url.Values{}, &job); err != nil {
		return job, err
	}
	if job.ProjectSlug == "" {
		job.ProjectSlug = project
	}
	return job, nil
}

func mergeCircleCIJob(base, details circleCIJob) circleCIJob {
	if details.ID == "" {
		details.ID = base.ID
	}
	if details.JobNumber == 0 {
		details.JobNumber = base.JobNumber
	}
	if details.Number == 0 {
		details.Number = base.Number
	}
	if details.Name == "" {
		details.Name = base.Name
	}
	if details.Status == "" {
		details.Status = base.Status
	}
	if details.Type == "" {
		details.Type = base.Type
	}
	if details.ProjectSlug == "" {
		details.ProjectSlug = base.ProjectSlug
	}
	if details.StartedAt == "" {
		details.StartedAt = base.StartedAt
	}
	if details.StoppedAt == "" {
		details.StoppedAt = base.StoppedAt
	}
	if details.QueuedAt == "" {
		details.QueuedAt = base.QueuedAt
	}
	if details.CreatedAt == "" {
		details.CreatedAt = base.CreatedAt
	}
	if details.Parallelism == 0 {
		details.Parallelism = base.Parallelism
	}
	if len(details.ParallelRuns) == 0 {
		details.ParallelRuns = base.ParallelRuns
	}
	if details.Executor.ResourceClass == "" {
		details.Executor.ResourceClass = base.Executor.ResourceClass
	}
	if details.Executor.Type == "" {
		details.Executor.Type = base.Executor.Type
	}
	if details.LatestWorkflow.Name == "" {
		details.LatestWorkflow.Name = base.LatestWorkflow.Name
	}
	return details
}

func circleCIJobNumber(job circleCIJob) int {
	if job.JobNumber > 0 {
		return job.JobNumber
	}
	return job.Number
}

func normalizeCircleCIJob(job circleCIJob, project, workflowName string) ([]record, error) {
	if job.StartedAt == "" || job.StoppedAt == "" {
		return nil, nil
	}
	start, err := time.Parse(time.RFC3339, job.StartedAt)
	if err != nil {
		return nil, fmt.Errorf("parse CircleCI started_at %q: %w", job.StartedAt, err)
	}
	end, err := time.Parse(time.RFC3339, job.StoppedAt)
	if err != nil {
		return nil, fmt.Errorf("parse CircleCI stopped_at %q: %w", job.StoppedAt, err)
	}
	if !end.After(start) {
		return nil, nil
	}

	var queueSeconds *float64
	for _, value := range []string{job.QueuedAt, job.CreatedAt} {
		queued, ok, err := parseOptionalTime(value, "CircleCI queued_at")
		if err != nil {
			return nil, err
		}
		if ok {
			q := start.Sub(queued).Seconds()
			if q < 0 {
				q = 0
			}
			queueSeconds = &q
			break
		}
	}

	parallelism := job.Parallelism
	if parallelism < 1 && len(job.ParallelRuns) > 0 {
		parallelism = len(job.ParallelRuns)
	}
	if parallelism < 1 {
		parallelism = 1
	}
	resourceClass := strings.TrimSpace(job.Executor.ResourceClass)
	if resourceClass == "" {
		resourceClass = "unknown"
	}
	executor := strings.TrimSpace(job.Executor.Type)
	workflow := firstNonEmpty(workflowName, job.LatestWorkflow.Name, "unknown workflow")
	jobName := firstNonEmpty(job.Name, "unknown job")
	status := strings.TrimSpace(job.Status)
	osName := inferCircleCIOS(resourceClass, executor)

	records := make([]record, 0, parallelism)
	for i := 0; i < parallelism; i++ {
		records = append(records, record{
			Provider:        circleCIProvider,
			Repo:            project,
			WorkflowName:    workflow,
			JobName:         jobName,
			Conclusion:      status,
			Start:           start,
			End:             end,
			QueueSeconds:    queueSeconds,
			OS:              osName,
			SelfHosted:      false,
			RunnerGroupName: resourceClass,
			ResourceClass:   resourceClass,
			Executor:        executor,
			Parallelism:     parallelism,
		})
	}
	return records, nil
}

func inferCircleCIOS(resourceClass, executor string) string {
	joined := strings.ToLower(resourceClass + " " + executor)
	switch {
	case strings.Contains(joined, "windows"):
		return "windows"
	case strings.Contains(joined, "macos"), strings.Contains(joined, "mac-"):
		return "macos"
	case strings.Contains(joined, "arm"):
		return "linux"
	default:
		return "linux"
	}
}

func dateWindow(since, until string) (time.Time, time.Time, bool, error) {
	start, err := time.Parse("2006-01-02", since)
	if err != nil {
		return time.Time{}, time.Time{}, false, err
	}
	if until == "" {
		return start, time.Time{}, false, nil
	}
	endDay, err := time.Parse("2006-01-02", until)
	if err != nil {
		return time.Time{}, time.Time{}, false, err
	}
	return start, endDay.Add(24 * time.Hour), true, nil
}

func parseOptionalTime(value, label string) (time.Time, bool, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false, nil
	}
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("parse %s %q: %w", label, value, err)
	}
	return t, true, nil
}

func circleCIProjectPath(project string) string {
	parts := strings.Split(project, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return "/project/" + strings.Join(parts, "/")
}

func circleCIPathForLog(path string, params url.Values) string {
	if len(params) == 0 {
		return path
	}
	return path + "?" + params.Encode()
}
