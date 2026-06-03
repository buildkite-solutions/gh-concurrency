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

## Install

### GitHub CLI extension

```bash
gh extension install buildkite-solutions/gh-concurrency
gh concurrency --repo owner/name --since 2025-05-01
```

The extension uses your existing `gh auth login` session when no token is set.
You can also provide `GITHUB_TOKEN`, `GH_TOKEN`, or `--token`.

### Docker

```bash
export GITHUB_TOKEN=ghp_xxx

docker run --rm -e GITHUB_TOKEN \
  ghcr.io/buildkite-solutions/gh-concurrency:latest \
  --repo owner/name --since 2025-05-01
```

Pool several repos into one organization-wide profile by repeating `--repo`:

```bash
gh concurrency \
  --repo owner/api \
  --repo owner/web \
  --repo owner/infra \
  --since 2025-05-01
```

## GitHub Enterprise Server

Point the tool at your instance's API base:

```bash
gh auth login --hostname ghes.example.com

gh concurrency \
  --base-url https://ghes.example.com/api/v3 \
  --repo owner/name \
  --since 2025-05-01
```

For Docker:

```bash
docker run --rm \
  -e GITHUB_TOKEN \
  -e GITHUB_API_URL=https://ghes.example.com/api/v3 \
  ghcr.io/buildkite-solutions/gh-concurrency:latest \
  --repo owner/name --since 2025-05-01
```

## Token Scopes

Use a fine-grained personal access token with read-only access:

- `Actions: read`
- `Metadata: read`

Or use a classic token with `repo` scope. The tool only reads; it never writes,
deletes, or changes anything.

## Reading The Output

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

Use `--format json` for machine-readable output and `--verbose` for progress
and rate-limit logging on stderr.

## Build

Requirements:

- Go 1.25+
- Docker with Buildx for image publishing
- `GITHUB_TOKEN` or `GH_TOKEN` for GitHub Release publishing

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

## Release

Releases are handled by Buildkite, not GitHub Actions.

1. Configure the Buildkite pipeline command as:

   ```bash
   buildkite-agent pipeline upload
   ```

2. Add secrets to the Buildkite pipeline or agent environment:

   - `GITHUB_TOKEN` or `GH_TOKEN` with repository release permissions.
   - `GHCR_TOKEN` or `GITHUB_TOKEN` with permission to publish packages.
   - `GHCR_USERNAME` if the package publisher should not default to
     `buildkite-solutions`.

3. Push a semver tag:

   ```bash
   git tag v1.0.0
   git push origin v1.0.0
   ```

On `v*` tags, Buildkite installs Go 1.25.3 with the `setup-go` plugin, runs
tests, builds precompiled gh extension binaries, uploads them to the GitHub
Release, and publishes a multi-arch image to GHCR. The pipeline uses a hosted
agent cache volume at `.buildkite/cache-volume` to keep toolchain and Go caches
warm between builds.

## Security Model

- Read-only: authenticated `GET` requests only, scoped to the configured GitHub
  API host.
- The token is read from env, `--token`, or `gh auth token`; it is never written
  to disk or logged.
- No telemetry and no third-party API calls.
- The Docker image runs a static Go binary as UID `10001`.
- The core implementation is small enough to audit in one sitting.

Customers can run it themselves and share the JSON output. They never need to
share their GitHub token.

## License

MIT.
