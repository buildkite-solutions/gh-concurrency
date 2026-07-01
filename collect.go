package main

import (
	"errors"
	"fmt"
	"io"
	"sync"
)

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
