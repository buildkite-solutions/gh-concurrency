# gh-concurrency

Estimate how many CI jobs run at the same time, so you can compare
GitHub Actions or CircleCI usage with a concurrency-based model like
Buildkite's.

GitHub Actions bills for the area under your usage curve. CircleCI exposes job
history, but not a single cross-project concurrency profile. Buildkite plans
are sized around the height of that curve, so `gh-concurrency` reconstructs it
from job start and finish timestamps, then reports peak concurrency plus
time-weighted percentiles.

The tool is a dependency-free Go binary. It makes authenticated `GET` requests
only, never logs your token, and works either as a GitHub CLI extension or as a
container image.

## For Users: Run The Estimator

This section is for people who want to install `gh-concurrency`, point it at
their GitHub repositories, GitHub organizations, or CircleCI projects, and read
the resulting concurrency profile.

### Install

#### GitHub CLI extension

```bash
gh extension install buildkite-solutions/gh-concurrency
gh concurrency --org owner --since 2025-05-01
```

The extension uses your existing `gh auth login` session when no token is set.
You can also provide `GITHUB_TOKEN`, `GH_TOKEN`, or `--token`.

To upgrade after a new release:

```bash
gh extension upgrade concurrency

# Or upgrade all installed gh extensions.
gh extension upgrade --all
```

`gh` also checks for newer extension versions periodically when the extension
runs and prints an upgrade notice when one is available.

#### Docker

```bash
export GITHUB_TOKEN=ghp_xxx

docker run --rm -e GITHUB_TOKEN \
  ghcr.io/buildkite-solutions/gh-concurrency:latest \
  --org owner --since 2025-05-01
```

### Choose Targets

Scan one repo, one org, several orgs, or a curated cross-org repo list. Every
target is pooled into one concurrency profile:

```bash
# One repository.
gh concurrency --repo owner/name --since 2025-05-01

# Every accessible repository in one organization.
gh concurrency --org owner --since 2025-05-01

# Multiple organizations.
gh concurrency --org owner-a --org owner-b --since 2025-05-01

# Organizations from a file.
gh concurrency --org-file orgs.txt --since 2025-05-01

# A curated list of repositories across organizations.
gh concurrency --repo-file repos.txt --since 2025-05-01

# Mix explicit repos, orgs, and files.
gh concurrency --org owner-a --repo owner-b/special --repo-file repos.txt --since 2025-05-01
```

`repos.txt` and `orgs.txt` accept one value per line, or comma/space-separated
values. `#` starts a comment:

```text
owner-a/api
owner-a/web, owner-b/mobile

# experimental migration candidates
owner-c/infra
```

Organization scans use `--repo-type all` by default. Use `--repo-type sources`
to exclude forks, or `public`, `private`, `forks`, or `member` when you need a
narrower profile. Archived repositories are excluded by default so old projects
do not add noise or workflow API requests; pass `--include-archived` if you
intentionally want them in the profile.

Use the workflow-run filters when a full organization scan is too broad:

```bash
# Only runs on main from push events.
gh concurrency --org owner --since 2025-05-01 --branch main --event push

# Omit pull request runs from the profile.
gh concurrency --org owner --since 2025-05-01 --exclude-pull-requests
```

By default, the tool queries completed workflow runs and fetches all workflow
run job attempts (`--job-filter all`). Use `--include-in-progress` to include
queued/running workflow runs in the scan, or `--job-filter latest` if you only
want the latest attempt for each workflow run.

### CircleCI Projects

CircleCI scans are project-scoped. Use a personal API token through
`CIRCLECI_TOKEN`, `CIRCLE_TOKEN`, or `--token`, then pass
`--provider circleci`:

```bash
# Full CircleCI project slug.
gh concurrency \
  --provider circleci \
  --circleci-project gh/owner/name \
  --since 2025-05-01

# GitHub-backed CircleCI projects can use --repo as a shorthand for gh/owner/name.
gh concurrency \
  --provider circleci \
  --repo owner/name \
  --since 2025-05-01

# Bitbucket-backed shorthand.
gh concurrency \
  --provider circleci \
  --circleci-vcs bb \
  --repo workspace/repo \
  --since 2025-05-01
```

Use `--circleci-project` for GitHub App, GitLab, or standalone projects whose
slug uses `circleci/<org-id>/<project-id>`. You can repeat
`--circleci-project`, mix it with `--repo`, or load slugs from
`--circleci-project-file`.

CircleCI project pipeline listing supports a branch filter, so `--branch main`
works for CircleCI too. The project pipeline endpoint does not provide a
server-side date range filter, so the tool pages through project pipelines,
filters by `--since`/`--until` locally, and stops once it reaches older
pipelines. Add `--circleci-max-pages N` to cap API spend per project when you
only need a quick first pass.

By default, CircleCI scans fetch per-job details so resource class, executor,
queue time, and `parallelism` are reflected in runner pools and concurrency.
Pass `--circleci-job-details=false` for fewer API requests; parallel jobs may
then be undercounted because the workflow jobs list does not include all detail
fields.

### Flag Compatibility

The CLI fails fast when a flag does not apply to the selected provider or mode.
This includes explicit no-op defaults, such as `--job-filter all` with
`--provider circleci` or `--circleci-vcs gh` with GitHub. Estimate tuning flags
such as `--estimate-sample-runs` require `--estimate`, and estimate mode is
GitHub-only.

`--branch` is shared by GitHub and CircleCI. `--include-in-progress` is
GitHub-only because CircleCI concurrency is measured from jobs with both
`started_at` and `stopped_at`.

### Fast Estimated Mode

If an exact GitHub organization scan is too expensive for the current GitHub API
budget, use `--estimate` for a fast first pass. Estimated mode is GitHub-only;
CircleCI scans use the exact project workflow/job endpoints.

```bash
gh concurrency \
  --org owner \
  --since 2025-05-01 \
  --repo-type sources \
  --estimate
```

Estimated mode still resolves repositories exactly, then builds a repository
landscape before sampling. The landscape ranks repositories primarily by
Actions workflow-run activity in the selected window, with repository metadata
such as size and recent pushes as tie-breakers and fallback signals. Without
extra flags, all repositories remain eligible, but the estimate scans them in
ranked order so request-budget stops spend API calls on the busiest repos first.
When the request budget is tight, the landscape is marked partial and ranking
falls back to the metadata already available.

Set `--estimate-repo-limit N` to focus the estimate on only the top-ranked
repositories:

```bash
gh concurrency \
  --org owner \
  --since 2025-05-01 \
  --estimate \
  --estimate-repo-limit 50
```

After ranking repositories, estimate mode lists workflow runs and samples
workflow-run jobs under the request budget. It builds reusable job shapes from
sampled runs and runs a Monte Carlo simulation to produce intervals:

```text
ESTIMATE MODE: sampled 250 of 3,412 known workflow runs; 90% simulation interval; seed 12345

Repository landscape: ranked 1,000 repos, selected 50, limit 50, probes complete
  #1 owner/api                            runs   14,230  size  180,301  pushed 2025-05-01  selected

Jobs analyzed:        median 12,340 (90% range 10,900-15,100)
Peak concurrency:     median 146 (90% range 105-230)
p95 concurrency:      median 30 (90% range 22-48)
```

Tune the request budget and reproducibility when needed:

```bash
gh concurrency \
  --org owner \
  --since 2025-05-01 \
  --estimate \
  --estimate-max-requests 750 \
  --estimate-min-remaining 500 \
  --estimate-sample-runs 200 \
  --estimate-repo-limit 50 \
  --estimate-seed 12345
```

Estimated output is intentionally labeled as sampled simulation data. It is
useful for sizing conversations and deciding whether a full exact scan is worth
spending API quota on, but run exact mode before final commitments. Absolute
peak concurrency is especially sensitive to rare unsampled fan-out.
JSON output includes `estimate.repository_landscape` with each repo's rank,
workflow-run count when known, metadata signals, and selected/not-selected
status.

### GitHub Enterprise Server

Point the tool at your instance's API base:

```bash
gh auth login --hostname ghes.example.com

gh concurrency \
  --base-url https://ghes.example.com/api/v3 \
  --org owner \
  --since 2025-05-01
```

For Docker:

```bash
docker run --rm \
  -e GITHUB_TOKEN \
  -e GITHUB_API_URL=https://ghes.example.com/api/v3 \
  ghcr.io/buildkite-solutions/gh-concurrency:latest \
  --org owner --since 2025-05-01
```

### Token Scopes

For GitHub scans:

Use a fine-grained personal access token with read-only access:

- `Actions: read`
- `Metadata: read`

Or use a classic token with `repo` scope. The tool only reads; it never writes,
deletes, or changes anything.

For organization-wide scans, the token must be able to list the organization's
repositories and read Actions metadata for each repository you want included.
Repositories that are not found or not readable are skipped with a warning.

For CircleCI scans, use a CircleCI personal API token. API v2 project tokens are
not supported by CircleCI; set `CIRCLECI_TOKEN`, `CIRCLE_TOKEN`, or pass
`--token`. The token must be able to read each CircleCI project you include.

### Reading The Output

```text
Jobs analyzed:        12,345
Run time:             1m23.4s
Peak concurrency:     42
p95 concurrency:      18

Scan summary:
  repositories: queued 74  scanned 70  skipped 4
  workflow runs: 3,210  workflow jobs seen: 12,800  jobs used: 12,345
  API: 6,530 requests  2 retries  1 rate-limit sleeps (61.0s)

Runner pools:
  self-hosted/blacksmith        peak   48  p95   30     4,120 jobs
  GitHub-hosted/linux           peak   12  p95    8       930 jobs
  self-hosted/arc               peak    9  p95    6       310 jobs

Top repositories by busy time:
  owner/api                                busy  123.45h  peak   12  p95    8     2,400 jobs
```

- Percentiles are time-weighted over busy time, when at least one job was
  running.
- Run time is measured by the tool itself, so you do not need to wrap the
  command in `time`.
- Size toward p95/p99, not the absolute peak. One nightly fan-out should not
  make you pay for that slot all month.
- Runner pools are derived from GitHub's workflow-job metadata. GitHub-hosted
  jobs are grouped by OS; self-hosted and third-party runner platforms such as
  Blacksmith, RunsOn, or ARC are grouped by runner group when GitHub reports
  one, with a label-based fallback for common third-party runner labels.
- CircleCI runner pools are grouped by resource class when per-job details are
  enabled. Jobs with `parallelism` greater than one are expanded into multiple
  concurrent slots for concurrency math.
- The billable-minutes estimate re-derives GitHub-hosted Actions minutes by
  rounding each job up to the minute, then applying Linux x1, Windows x2, and
  macOS x10 multipliers. Self-hosted jobs are treated as free. This section is
  omitted for CircleCI scans.
- Queue-time warnings mean measured concurrency is probably a floor. If jobs
  waited in GitHub's queue, true demand was higher than observed concurrency.
- The scan summary explains how much data was collected, which repositories
  were skipped, and whether rate limits affected the run.
- Top repositories, workflows, and jobs point at the biggest contributors to
  busy time, so you can find the useful migration-sizing conversations faster.

Use `--format json` for machine-readable output. Progress and diagnostics are
written to stderr so they do not corrupt JSON:

```bash
# Show target resolution and a repo-by-repo progress bar.
gh concurrency --org owner --since 2025-05-01 --verbose

# Add exact API GET/page/rate-limit diagnostics.
gh concurrency --org owner --since 2025-05-01 --debug
```

`-v` is an alias for `--verbose`; `-d` is an alias for `--debug`. Debug mode
implies verbose mode.

The tool uses a conservative shared API worker pool by default
(`--api-workers 4`). Every request goes through one global request-start delay
(`--request-delay-ms 100`), honors `Retry-After`, waits for primary rate-limit
reset windows, and backs off for secondary rate-limit responses. Increase
`--api-workers` carefully for faster scans, or lower it to `1` for fully serial
API access.

### Security Model

- Read-only: authenticated `GET` requests only, scoped to the configured API
  host.
- The token is read from env, `--token`, or GitHub's `gh auth token` fallback;
  it is never written to disk or logged.
- No telemetry and no third-party API calls.
- The Docker image runs a static Go binary as UID `10001`.
- The core implementation is small enough to audit in one sitting.

Customers can run it themselves and share the JSON output. They never need to
share their GitHub or CircleCI token.

## For Maintainers: Develop, Build, And Release

This section is for people working on this repository: local development,
packaging, Docker images, GitHub Release assets, and Buildkite release
automation.

### Build

Requirements:

- Go 1.25+
- Docker with Buildx for image publishing
- GitHub App credentials for GitHub Release publishing

```bash
make test
make build
./gh-concurrency --version
```

Local Docker image:

```bash
make docker-build
docker run --rm ghcr.io/buildkite-solutions/gh-concurrency:dev --help
```

Build release binaries for the gh extension:

```bash
make release-binaries VERSION=v1.0.0
ls dist/
```

### Live Smoke Test Before Release

Before pushing a new `v*` tag, test the local binary against a real test
organization. Pick a small recent window first so failures are fast and you do
not burn through API quota while iterating:

```bash
export TEST_GITHUB_ORG=your-test-org
export TEST_SINCE=2025-05-01
export TEST_UNTIL=2025-05-08

make build

./gh-concurrency \
  --org "$TEST_GITHUB_ORG" \
  --since "$TEST_SINCE" \
  --until "$TEST_UNTIL" \
  --repo-type sources \
  --api-workers 4 \
  --verbose
```

For a release-candidate check, capture JSON and compare serial vs parallel
collection. The headline results should match; the scan API counters and run
time will differ:

```bash
./gh-concurrency \
  --org "$TEST_GITHUB_ORG" \
  --since "$TEST_SINCE" \
  --until "$TEST_UNTIL" \
  --repo-type sources \
  --api-workers 1 \
  --format json \
  > /tmp/gh-concurrency-serial.json

./gh-concurrency \
  --org "$TEST_GITHUB_ORG" \
  --since "$TEST_SINCE" \
  --until "$TEST_UNTIL" \
  --repo-type sources \
  --api-workers 4 \
  --format json \
  > /tmp/gh-concurrency-parallel.json
```

If `jq` is available, this is a quick sanity check:

```bash
jq '{jobs_analyzed, peak_concurrency, percentile_concurrency, scan}' \
  /tmp/gh-concurrency-serial.json
jq '{jobs_analyzed, peak_concurrency, percentile_concurrency, scan}' \
  /tmp/gh-concurrency-parallel.json
```

To test the command exactly as a local `gh` extension, build the executable
first, then install this checkout. GitHub CLI links local extensions to the
root executable, so `make build` must happen before `gh extension install .`:

```bash
make build
gh extension install . --force

gh concurrency \
  --org "$TEST_GITHUB_ORG" \
  --since "$TEST_SINCE" \
  --until "$TEST_UNTIL" \
  --repo-type sources \
  --api-workers 4 \
  --verbose
```

If you replaced an installed release with the local checkout, restore the
published extension after testing:

```bash
gh extension remove concurrency
gh extension install buildkite-solutions/gh-concurrency
```

### Release

Releases are handled by Buildkite, not GitHub Actions.

1. Configure the Buildkite pipeline command as:

   ```bash
   buildkite-agent pipeline upload
   ```

2. Create a GitHub App for release publishing:

   Create the app from the organization, not from your personal settings and
   not from the repository settings:

   - Go to `https://github.com/organizations/buildkite-solutions/settings/apps`
     or GitHub profile menu -> Your organizations -> `buildkite-solutions` ->
     Settings -> Developer settings -> GitHub Apps.
   - Click New GitHub App.
   - Use a short unique name, for example `bk-gh-concurrency-release`.
   - Set Homepage URL to `https://github.com/buildkite-solutions/gh-concurrency`.
   - Leave Callback URL and Setup URL blank.
   - In Webhook, uncheck Active. No webhook URL is needed.
   - Under Repository permissions, set Contents to Read and write. Leave every
     other permission as No access. Metadata will remain read-only because
     GitHub Apps always get it.
   - Do not subscribe to any events.
   - Under "Where can this GitHub App be installed?", choose Only on this
     account.
   - Click Create GitHub App.

   If you do not see organization Developer settings, you need an organization
   owner or GitHub App manager to create the app.

3. Install the app only on this repository:

   - On the app settings page, click Install App in the left sidebar.
   - Click Install next to `buildkite-solutions`.
   - Choose Only select repositories.
   - Select `gh-concurrency`.
   - Click Install.

4. Create the app secret values:

   - On the app settings page, copy the Client ID.
   - Under Private keys, click Generate a private key. GitHub downloads a `.pem`
     file.
   - Base64-encode the PEM into one line:

     ```bash
     openssl base64 -A -in path/to/github-app-private-key.pem
     ```

5. Add Buildkite secrets:

   In Buildkite, add secrets for the pipeline's agent cluster. The release step
   expects:

   - `GITHUB_APP_CLIENT_ID`: the GitHub App Client ID.
   - `GITHUB_APP_PRIVATE_KEY_B64`: the one-line base64 value from the private
     key PEM.

   Create a narrowly scoped classic PAT for GHCR package publishing and store:

   - `GHCR_USERNAME`: the GitHub username that owns the PAT.
   - `GHCR_TOKEN`: a classic PAT with `write:packages` for GHCR publishing
     (`read:packages` is also commonly granted; add `repo` only if publishing a
     private package requires it).

6. Push a semver tag:

   ```bash
   git tag v1.0.0
   git push origin v1.0.0
   ```

7. Trigger the Buildkite build manually:

   Keep the Buildkite pipeline's branch filter as `main pb/*`. For a manual
   release build, use `main` as the branch so the branch filter passes, and use
   the pushed tag as the commit/ref to check out.

   In the Buildkite UI, click New Build and set:

   - Branch: `main`
   - Commit: `v1.0.0`
   - Message: `Release v1.0.0`
   - Environment variable: `RELEASE_TAG=v1.0.0`

   Or with the Buildkite CLI:

   ```bash
   bk build create \
     --pipeline buildkite-solutions/gh-concurrency \
     --branch main \
     --commit v1.0.0 \
     --message "Release v1.0.0" \
     --env RELEASE_TAG=v1.0.0
   ```

   Do not set the Buildkite branch to `v1.0.0` unless you also add `v*` to the
   pipeline branch filter. This release path intentionally keeps the branch as
   `main` and uses `RELEASE_TAG` as the manual release gate.

   Do not set `BUILDKITE_GIT_FETCH_FLAGS` in `.buildkite/pipeline.yml` to force
   tag fetching. `BUILDKITE_*` variables are protected runtime variables, so
   Buildkite will ignore pipeline-level overrides. For manual releases, use the
   pushed tag as the Buildkite build commit/ref as shown above.

On `v*` tags, Buildkite installs Go 1.25.3 with the `setup-go` plugin, runs
tests, validates the GitHub App release credentials, builds precompiled gh
extension binaries, mints a one-hour GitHub App installation token scoped to
`buildkite-solutions/gh-concurrency` with `contents: write`, uploads the GitHub
Release assets, and publishes a multi-arch image to GHCR with `GHCR_TOKEN`. For
manually triggered builds, `RELEASE_TAG` can be used instead of Buildkite's
native `build.tag` value. The pipeline uses a hosted agent cache volume at
`.buildkite/cache-volume` to keep toolchain and Go caches warm between builds.

## License

MIT.
