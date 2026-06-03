# gh-concurrency

Estimate how many GitHub Actions jobs run at the same time, so you can compare
GitHub's per-minute billing model with a concurrency-based model like
Buildkite's.

GitHub Actions bills for the area under your usage curve. Buildkite plans are
sized around the height of that curve. GitHub's billing and metrics views do
not report concurrency directly, so `gh-concurrency` reconstructs it from job
start and finish timestamps, then reports peak concurrency plus time-weighted
percentiles.

The tool is a dependency-free Go binary. It makes authenticated `GET` requests
only, never logs your token, and works either as a GitHub CLI extension or as a
container image.

## For Users: Run The Estimator

This section is for people who want to install `gh-concurrency`, point it at
their GitHub repositories or organizations, and read the resulting concurrency
profile.

### Install

#### GitHub CLI extension

```bash
gh extension install buildkite-solutions/gh-concurrency
gh concurrency --org owner --since 2025-05-01
```

The extension uses your existing `gh auth login` session when no token is set.
You can also provide `GITHUB_TOKEN`, `GH_TOKEN`, or `--token`.

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
narrower profile.

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

Use a fine-grained personal access token with read-only access:

- `Actions: read`
- `Metadata: read`

Or use a classic token with `repo` scope. The tool only reads; it never writes,
deletes, or changes anything.

For organization-wide scans, the token must be able to list the organization's
repositories and read Actions metadata for each repository you want included.
Repositories that are not found or not readable are skipped with a warning.

### Reading The Output

```text
Peak concurrency:     42
p95 concurrency:      18
```

- Percentiles are time-weighted over busy time, when at least one job was
  running.
- Size toward p95/p99, not the absolute peak. One nightly fan-out should not
  make you pay for that slot all month.
- The billable-minutes estimate re-derives GitHub-hosted Actions minutes by
  rounding each job up to the minute, then applying Linux x1, Windows x2, and
  macOS x10 multipliers. Self-hosted jobs are treated as free.
- Queue-time warnings mean measured concurrency is probably a floor. If jobs
  waited in GitHub's queue, true demand was higher than observed concurrency.

Use `--format json` for machine-readable output. Progress and diagnostics are
written to stderr so they do not corrupt JSON:

```bash
# Show target resolution and a repo-by-repo progress bar.
gh concurrency --org owner --since 2025-05-01 --verbose

# Add exact GitHub API GET/page/rate-limit diagnostics.
gh concurrency --org owner --since 2025-05-01 --debug
```

`-v` is an alias for `--verbose`; `-d` is an alias for `--debug`. Debug mode
implies verbose mode. The tool runs requests sequentially, sleeps briefly
before each request by default (`--request-delay-ms 100`), honors `Retry-After`,
waits for primary rate-limit reset windows, and backs off for secondary
rate-limit responses.

### Security Model

- Read-only: authenticated `GET` requests only, scoped to the configured GitHub
  API host.
- The token is read from env, `--token`, or `gh auth token`; it is never written
  to disk or logged.
- No telemetry and no third-party API calls.
- The Docker image runs a static Go binary as UID `10001`.
- The core implementation is small enough to audit in one sitting.

Customers can run it themselves and share the JSON output. They never need to
share their GitHub token.

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
