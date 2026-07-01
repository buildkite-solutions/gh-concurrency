package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"
)

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

	token, err := resolveToken(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	if cfg.provider == circleCIProvider {
		client := newCircleCIClient(cfg.baseURL, token, cfg.maxRetries, cfg.verbose)
		client.setAPIWorkers(cfg.apiWorkers)
		client.debug = cfg.debug
		client.logWriter = stderr
		client.requestDelay = time.Duration(cfg.requestDelayMS) * time.Millisecond
		client.logf("resolving CircleCI project targets")
		targetProjects, err := resolveCircleCIProjects(cfg)
		if err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 1
		}
		if len(targetProjects) == 0 {
			fmt.Fprintln(stderr, "error: no CircleCI projects matched the requested targets.")
			return 1
		}
		client.logf("resolved %d CircleCI projects", len(targetProjects))
		cfg.circleCIProjects = targetProjects
		cfg.repos = targetProjects

		progress := newProgressReporterForProvider(stderr, cfg.verbose, len(cfg.circleCIProjects), circleCIProvider)
		progress.Begin()
		records, summary, err := collectCircleCIProjects(client, cfg.circleCIProjects, collectOptions{
			Since:              cfg.since,
			Until:              cfg.until,
			IncludeInProgress:  cfg.includeInProgress,
			Branch:             cfg.branch,
			APIWorkers:         cfg.apiWorkers,
			CircleCIJobDetails: cfg.circleCIJobDetails,
			CircleCIMaxPages:   cfg.circleCIMaxPages,
		}, progress, stderr)
		progress.Complete()
		if err != nil {
			var ae authError
			if errors.As(err, &ae) {
				fmt.Fprintln(stderr, "error: 401 unauthorized. Check CircleCI token scope and validity.")
				return 1
			}
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 1
		}
		if len(records) == 0 {
			fmt.Fprintln(stderr, "error: no completed CircleCI jobs found in that window.")
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

	client := newGitHubClient(cfg.baseURL, token, cfg.maxRetries, cfg.verbose)
	client.setAPIWorkers(cfg.apiWorkers)
	client.debug = cfg.debug
	client.logWriter = stderr
	client.requestDelay = time.Duration(cfg.requestDelayMS) * time.Millisecond
	client.logf("resolving repository targets")
	targetRepos, repoInfos, skippedRepos, err := resolveTargetReposWithInfo(client, cfg, stderr)
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
		rep, err := runEstimate(client, cfg, repoInfos, skippedRepos, started, stderr)
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
