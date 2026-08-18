package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	githubProvider         = "github"
	circleCIProvider       = "circleci"
	defaultProvider        = githubProvider
	defaultBaseURL         = "https://api.github.com"
	defaultCircleCIBaseURL = "https://circleci.com/api/v2"
)

type config struct {
	providedFlags        map[string]bool
	provider             string
	repos                []string
	orgs                 []string
	repoFiles            []string
	orgFiles             []string
	circleCIProjects     []string
	circleCIProjectFiles []string
	circleCIVCS          string
	circleCIJobDetails   bool
	circleCIMaxPages     int
	repoType             string
	since                string
	until                string
	baseURL              string
	token                string
	format               string
	maxRetries           int
	requestDelayMS       int
	apiWorkers           int
	includeArchived      bool
	includeInProgress    bool
	jobFilter            string
	branch               string
	event                string
	excludePullRequests  bool
	runnerInventory      bool
	top                  int
	resourceMapFile      string
	defaultVCPUs         int
	resourceRules        []resourceRule
	estimate             bool
	estimateMaxRequests  int
	estimateMinRemaining int
	estimateSampleRuns   int
	estimateIterations   int
	estimateConfidence   int
	estimateSeed         int64
	estimateRepoLimit    int
	verbose              bool
	debug                bool
	showVer              bool
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
	var orgs stringList
	var repoFiles stringList
	var orgFiles stringList
	var circleCIProjects stringList
	var circleCIProjectFiles stringList

	fs := flag.NewFlagSet("gh-concurrency", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&cfg.provider, "provider", defaultProvider, "CI provider: github or circleci")
	fs.Var(&repos, "repo", "repository in OWNER/NAME form (GitHub) or OWNER/NAME for CircleCI (mapped to VCS/OWNER/NAME using --circleci-vcs; repeatable)")
	fs.Var(&orgs, "org", "GitHub-only: organization whose accessible repositories should be pooled (repeatable)")
	fs.Var(&repoFiles, "repo-file", "file containing OWNER/NAME repositories, one per line or comma/space separated (repeatable)")
	fs.Var(&orgFiles, "org-file", "GitHub-only: file containing organization names, one per line or comma/space separated (repeatable)")
	fs.Var(&circleCIProjects, "circleci-project", "CircleCI-only: project slug in VCS/ORG/PROJECT form (repeatable; projects pool into one profile)")
	fs.Var(&circleCIProjectFiles, "circleci-project-file", "CircleCI-only: file containing project slugs, one per line or comma/space separated (repeatable)")
	fs.StringVar(&cfg.circleCIVCS, "circleci-vcs", "gh", "CircleCI-only: VCS slug used to map --repo OWNER/NAME")
	fs.BoolVar(&cfg.circleCIJobDetails, "circleci-job-details", true, "CircleCI-only: fetch per-job details for resource class, executor, queue time, and parallelism")
	fs.IntVar(&cfg.circleCIMaxPages, "circleci-max-pages", 0, "CircleCI-only: maximum pipeline pages to read per project; 0 means unlimited")
	fs.StringVar(&cfg.repoType, "repo-type", "all", "GitHub-only: organization repository type: all, public, private, forks, sources, or member")
	fs.StringVar(&cfg.since, "since", "", "lower bound on workflow-run creation date (YYYY-MM-DD)")
	fs.StringVar(&cfg.until, "until", "", "optional upper bound on workflow-run creation date (YYYY-MM-DD)")
	fs.StringVar(&cfg.baseURL, "base-url", "", "API base URL. GitHub default: GITHUB_API_URL or https://api.github.com; CircleCI default: CIRCLECI_API_URL or https://circleci.com/api/v2")
	fs.StringVar(&cfg.token, "token", "", "API token. GitHub defaults to GITHUB_TOKEN/GH_TOKEN plus gh auth fallback; CircleCI defaults to CIRCLECI_TOKEN/CIRCLE_TOKEN")
	fs.StringVar(&cfg.format, "format", "text", "output format: text or json")
	fs.IntVar(&cfg.maxRetries, "max-retries", 6, "maximum HTTP attempts (including retries)")
	fs.IntVar(&cfg.requestDelayMS, "request-delay-ms", 100, "minimum delay before each API request; helps avoid rate limits")
	fs.IntVar(&cfg.apiWorkers, "api-workers", 4, "maximum concurrent API requests")
	fs.BoolVar(&cfg.includeArchived, "include-archived", false, "GitHub-only: include archived repositories instead of skipping them during target resolution")
	fs.BoolVar(&cfg.includeInProgress, "include-in-progress", false, "GitHub-only: include non-completed workflow runs instead of querying only completed runs")
	fs.StringVar(&cfg.jobFilter, "job-filter", "all", "GitHub-only: workflow-run job filter: all or latest")
	fs.StringVar(&cfg.branch, "branch", "", "only include runs for this branch")
	fs.StringVar(&cfg.event, "event", "", "GitHub-only: only include workflow runs for this event, such as push or pull_request")
	fs.BoolVar(&cfg.excludePullRequests, "exclude-pull-requests", false, "GitHub-only: omit pull request workflow runs")
	fs.BoolVar(&cfg.runnerInventory, "runner-inventory", false, "GitHub exact-mode only: enrich larger runner pools from organization inventory (requires Administration: read)")
	fs.IntVar(&cfg.top, "top", 10, "number of top repositories, workflows, and jobs to show")
	fs.StringVar(&cfg.resourceMapFile, "resource-map", "", "exact mode: JSON file mapping job metadata to target platform, shape, and vCPUs")
	fs.IntVar(&cfg.defaultVCPUs, "default-vcpus", 0, "exact mode: target vCPUs for jobs not matched by --resource-map; 0 leaves them unresolved")
	fs.BoolVar(&cfg.estimate, "estimate", false, "GitHub-only: use sampled workflow-run jobs and simulation to estimate concurrency faster")
	fs.IntVar(&cfg.estimateMaxRequests, "estimate-max-requests", 1000, "GitHub estimate-only: maximum API requests to spend after target resolution")
	fs.IntVar(&cfg.estimateMinRemaining, "estimate-min-remaining", 500, "GitHub estimate-only: stop before primary rate-limit remaining reaches this value")
	fs.IntVar(&cfg.estimateSampleRuns, "estimate-sample-runs", 250, "GitHub estimate-only: target workflow runs to sample")
	fs.IntVar(&cfg.estimateIterations, "estimate-iterations", 1000, "GitHub estimate-only: Monte Carlo iterations")
	fs.IntVar(&cfg.estimateConfidence, "estimate-confidence", 90, "GitHub estimate-only: confidence interval percentage")
	fs.Int64Var(&cfg.estimateSeed, "estimate-seed", 0, "GitHub estimate-only: random seed; default is generated and printed")
	fs.IntVar(&cfg.estimateRepoLimit, "estimate-repo-limit", 0, "GitHub estimate-only: rank repositories first and only estimate the top N; 0 keeps all repositories")
	fs.BoolVar(&cfg.verbose, "verbose", false, "progress and rate-limit logging to stderr")
	fs.BoolVar(&cfg.verbose, "v", false, "alias for --verbose")
	fs.BoolVar(&cfg.debug, "debug", false, "HTTP request and pagination diagnostics to stderr; implies --verbose")
	fs.BoolVar(&cfg.debug, "d", false, "alias for --debug")
	fs.BoolVar(&cfg.showVer, "version", false, "print version and exit")

	if err := fs.Parse(argv); err != nil {
		return cfg, err
	}
	cfg.providedFlags = providedFlags(fs)
	cfg.provider = normalizeProvider(cfg.provider)
	cfg.repos = repos
	cfg.orgs = orgs
	cfg.repoFiles = repoFiles
	cfg.orgFiles = orgFiles
	cfg.circleCIProjects = circleCIProjects
	cfg.circleCIProjectFiles = circleCIProjectFiles
	cfg.baseURL = strings.TrimRight(firstNonEmpty(cfg.baseURL, defaultBaseURLForProvider(cfg.provider)), "/")
	if cfg.token == "" {
		cfg.token = envTokenForProvider(cfg.provider)
	}
	if cfg.maxRetries < 1 {
		cfg.maxRetries = 1
	}
	if cfg.requestDelayMS < 0 {
		cfg.requestDelayMS = 0
	}
	if cfg.apiWorkers < 1 {
		cfg.apiWorkers = 1
	}
	if cfg.apiWorkers > 32 {
		cfg.apiWorkers = 32
	}
	if cfg.top < 0 {
		cfg.top = 0
	}
	if cfg.circleCIMaxPages < 0 {
		cfg.circleCIMaxPages = 0
	}
	if cfg.estimateMaxRequests < 1 {
		cfg.estimateMaxRequests = 1
	}
	if cfg.estimateMinRemaining < 0 {
		cfg.estimateMinRemaining = 0
	}
	if cfg.estimateSampleRuns < 1 {
		cfg.estimateSampleRuns = 1
	}
	if cfg.estimateIterations < 1 {
		cfg.estimateIterations = 1
	}
	if cfg.estimateConfidence < 1 {
		cfg.estimateConfidence = 1
	}
	if cfg.estimateConfidence > 99 {
		cfg.estimateConfidence = 99
	}
	if cfg.estimateRepoLimit < 0 {
		cfg.estimateRepoLimit = 0
	}
	if cfg.debug {
		cfg.verbose = true
	}
	return cfg, nil
}

func providedFlags(fs *flag.FlagSet) map[string]bool {
	out := map[string]bool{}
	fs.Visit(func(f *flag.Flag) {
		out[canonicalFlagName(f.Name)] = true
	})
	return out
}

func canonicalFlagName(name string) string {
	switch name {
	case "v":
		return "verbose"
	case "d":
		return "debug"
	default:
		return name
	}
}

func flagProvided(cfg config, name string) bool {
	return cfg.providedFlags != nil && cfg.providedFlags[canonicalFlagName(name)]
}

func validateConfig(cfg config) error {
	if cfg.showVer {
		return nil
	}
	cfg.provider = normalizeProvider(cfg.provider)
	switch cfg.provider {
	case githubProvider:
	case circleCIProvider:
	default:
		return fmt.Errorf("invalid --provider %q; expected github or circleci", cfg.provider)
	}
	if err := validateFlagCompatibility(cfg); err != nil {
		return err
	}
	if cfg.defaultVCPUs < 0 {
		return errors.New("--default-vcpus must be 0 or greater")
	}
	if cfg.provider == githubProvider && len(cfg.repos) == 0 && len(cfg.orgs) == 0 && len(cfg.repoFiles) == 0 && len(cfg.orgFiles) == 0 {
		return errors.New("at least one --repo, --org, --repo-file, or --org-file is required")
	}
	if cfg.provider == circleCIProvider && len(cfg.repos) == 0 && len(cfg.repoFiles) == 0 && len(cfg.circleCIProjects) == 0 && len(cfg.circleCIProjectFiles) == 0 {
		return errors.New("at least one --repo, --repo-file, --circleci-project, or --circleci-project-file is required")
	}
	for _, repo := range cfg.repos {
		if err := validateRepo(repo); err != nil {
			return err
		}
	}
	for _, org := range cfg.orgs {
		if err := validateOrg(org); err != nil {
			return err
		}
	}
	for _, project := range cfg.circleCIProjects {
		if err := validateCircleCIProject(project); err != nil {
			return err
		}
	}
	switch cfg.repoType {
	case "all", "public", "private", "forks", "sources", "member":
	default:
		return fmt.Errorf("invalid --repo-type %q; expected all, public, private, forks, sources, or member", cfg.repoType)
	}
	for _, path := range append(append(append([]string{}, cfg.repoFiles...), cfg.orgFiles...), cfg.circleCIProjectFiles...) {
		if strings.TrimSpace(path) == "" {
			return errors.New("target file path cannot be empty")
		}
	}
	if (cfg.provider == circleCIProvider || flagProvided(cfg, "circleci-vcs")) && (strings.TrimSpace(cfg.circleCIVCS) == "" || strings.Contains(cfg.circleCIVCS, "/") || strings.ContainsAny(cfg.circleCIVCS, " \t\r\n")) {
		return fmt.Errorf("invalid --circleci-vcs %q; expected a CircleCI VCS slug such as gh, bb, or circleci", cfg.circleCIVCS)
	}
	if cfg.since == "" {
		return errors.New("--since YYYY-MM-DD is required")
	}
	since, err := time.Parse("2006-01-02", cfg.since)
	if err != nil {
		return fmt.Errorf("invalid --since %q; expected YYYY-MM-DD", cfg.since)
	}
	if cfg.until != "" {
		until, err := time.Parse("2006-01-02", cfg.until)
		if err != nil {
			return fmt.Errorf("invalid --until %q; expected YYYY-MM-DD", cfg.until)
		}
		if until.Before(since) {
			return fmt.Errorf("invalid --until %q; must be on or after --since %s", cfg.until, cfg.since)
		}
	}
	if cfg.format != "text" && cfg.format != "json" {
		return fmt.Errorf("invalid --format %q; expected text or json", cfg.format)
	}
	if cfg.jobFilter != "all" && cfg.jobFilter != "latest" {
		return fmt.Errorf("invalid --job-filter %q; expected all or latest", cfg.jobFilter)
	}
	u, err := url.Parse(cfg.baseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("invalid --base-url %q", cfg.baseURL)
	}
	return nil
}

func validateFlagCompatibility(cfg config) error {
	if cfg.provider == circleCIProvider {
		if flagProvided(cfg, "include-in-progress") {
			return errors.New("--include-in-progress is not supported for CircleCI because CircleCI concurrency requires stopped jobs")
		}
		if flagProvided(cfg, "estimate") {
			return errors.New("--estimate is only supported with --provider github")
		}
		if err := rejectProvidedFlags(cfg, githubOnlyFlags(), "only applies with --provider github"); err != nil {
			return err
		}
		if err := rejectProvidedFlags(cfg, estimateKnobFlags(), "only applies with --provider github and --estimate"); err != nil {
			return err
		}
		if cfg.estimate {
			return errors.New("--estimate is only supported with --provider github")
		}
		return nil
	}

	if err := rejectProvidedFlags(cfg, circleCIOnlyFlags(), "only applies with --provider circleci"); err != nil {
		return err
	}
	if !cfg.estimate {
		if err := rejectProvidedFlags(cfg, estimateKnobFlags(), "requires --estimate"); err != nil {
			return err
		}
	} else if cfg.runnerInventory {
		return errors.New("--runner-inventory requires exact mode; remove --estimate")
	}
	if cfg.estimate {
		if flagProvided(cfg, "resource-map") {
			return errors.New("--resource-map is not yet supported with --estimate; run exact mode")
		}
		if flagProvided(cfg, "default-vcpus") {
			return errors.New("--default-vcpus is not yet supported with --estimate; run exact mode")
		}
	}
	return nil
}

func rejectProvidedFlags(cfg config, flags []string, reason string) error {
	for _, name := range flags {
		if flagProvided(cfg, name) {
			return fmt.Errorf("--%s %s", name, reason)
		}
	}
	return nil
}

func githubOnlyFlags() []string {
	return []string{
		"org",
		"org-file",
		"repo-type",
		"include-archived",
		"include-in-progress",
		"job-filter",
		"event",
		"exclude-pull-requests",
		"runner-inventory",
	}
}

func circleCIOnlyFlags() []string {
	return []string{
		"circleci-project",
		"circleci-project-file",
		"circleci-vcs",
		"circleci-job-details",
		"circleci-max-pages",
	}
}

func estimateKnobFlags() []string {
	return []string{
		"estimate-max-requests",
		"estimate-min-remaining",
		"estimate-sample-runs",
		"estimate-iterations",
		"estimate-confidence",
		"estimate-seed",
		"estimate-repo-limit",
	}
}

func validateRepo(repo string) error {
	owner, name, err := splitRepoName(repo)
	if err != nil {
		return err
	}
	if owner == "" || name == "" {
		return fmt.Errorf("invalid repo %q; expected OWNER/NAME", repo)
	}
	return nil
}

func validateOrg(org string) error {
	org = strings.TrimSpace(org)
	if org == "" || strings.Contains(org, "/") || strings.ContainsAny(org, " \t\r\n") {
		return fmt.Errorf("invalid org %q; expected organization slug", org)
	}
	return nil
}

func validateCircleCIProject(project string) error {
	project = strings.TrimSpace(project)
	if project == "" || strings.ContainsAny(project, " \t\r\n") {
		return fmt.Errorf("invalid CircleCI project %q; expected VCS/ORG/PROJECT", project)
	}
	parts := strings.Split(project, "/")
	if len(parts) != 3 {
		return fmt.Errorf("invalid CircleCI project %q; expected VCS/ORG/PROJECT", project)
	}
	for _, part := range parts {
		if part == "" {
			return fmt.Errorf("invalid CircleCI project %q; expected VCS/ORG/PROJECT", project)
		}
	}
	return nil
}

func normalizeProvider(provider string) string {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		return defaultProvider
	}
	return provider
}

func defaultBaseURLForProvider(provider string) string {
	switch provider {
	case circleCIProvider:
		if baseURL := strings.TrimSpace(os.Getenv("CIRCLECI_API_URL")); baseURL != "" {
			return baseURL
		}
		return defaultCircleCIBaseURL
	default:
		if baseURL := strings.TrimSpace(os.Getenv("GITHUB_API_URL")); baseURL != "" {
			return baseURL
		}
		return defaultBaseURL
	}
}

func envTokenForProvider(provider string) string {
	switch provider {
	case circleCIProvider:
		for _, key := range []string{"CIRCLECI_TOKEN", "CIRCLE_TOKEN"} {
			if token := strings.TrimSpace(os.Getenv(key)); token != "" {
				return token
			}
		}
		return ""
	default:
		return envToken()
	}
}
