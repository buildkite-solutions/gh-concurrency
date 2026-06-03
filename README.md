# gha-concurrency

Estimate how many GitHub Actions jobs run **at the same time** so you can
compare GitHub's per-minute billing against a concurrency-based model like
Buildkite's.

GitHub bills Actions by minutes — the *area* under your usage curve. Buildkite
bills by concurrency — the *height* of that curve. GitHub's billing and metrics
views never report concurrency, so this tool reconstructs it from job
start/finish timestamps and reports peak plus time-weighted percentiles.

It runs entirely from your machine, makes only authenticated `GET` requests to
GitHub, never logs or transmits your token, and has **no third-party
dependencies** (standard-library Python only).

## Quickstart (GitHub cloud)

```bash
docker run --rm -e GITHUB_TOKEN \
  YOURDOCKERHUBUSER/gha-concurrency:latest \
  --repo owner/name --since 2025-05-01
```

`-e GITHUB_TOKEN` passes the token from your shell environment into the
container without it ever appearing in your command history. Set it first:

```bash
export GITHUB_TOKEN=ghp_xxx
```

Pool several repos into one organization-wide profile by repeating `--repo`:

```bash
docker run --rm -e GITHUB_TOKEN YOURDOCKERHUBUSER/gha-concurrency:latest \
  --repo owner/api --repo owner/web --repo owner/infra --since 2025-05-01
```

## GitHub Enterprise Server (self-hosted GitHub)

Point the tool at your instance's API base — either flag or env var:

```bash
docker run --rm -e GITHUB_TOKEN \
  -e GITHUB_API_URL=https://ghes.example.com/api/v3 \
  YOURDOCKERHUBUSER/gha-concurrency:latest \
  --repo owner/name --since 2025-05-01
# equivalently: --base-url https://ghes.example.com/api/v3
```

The default is `https://api.github.com` (cloud). For GHES the base path is
your hostname followed by `/api/v3`.

## Token scopes (least privilege)

Use a **fine-grained personal access token** with read-only access:

- `Actions: read`
- `Metadata: read`

Or a classic token with `repo` scope. The tool only reads; it never writes,
deletes, or changes anything.

## Reading the output

```
Peak concurrency:     42      <- your single busiest instant
p95 concurrency:      18      <- size Buildkite around here, not the peak
Billable-minutes estimate ... <- compare this to your real GitHub invoice
```

- **Percentiles are time-weighted over busy time** (when ≥1 job was running),
  so they answer "when I'm actually building, how parallel is it?"
- **Size toward p95/p99, not peak.** A single 2 a.m. cron fan-out shouldn't
  make you pay for that capacity all month.
- **The billable-minutes estimate is your trust check.** It re-derives minutes
  (rounding each job up to the minute, applying the Linux ×1 / Windows ×2 /
  macOS ×10 multipliers, treating self-hosted as free). If that total matches
  your GitHub bill, the concurrency number is built on the same data that just
  reproduced your invoice.
- **Heed the warnings.** Sustained queue times or a peak pinned to a round
  number suggest you were hitting a GitHub concurrency cap — meaning real
  demand is *higher* than reported. The measured concurrency is a floor, not a
  ceiling.

Add `--format json` for machine-readable output (the JSON is self-describing:
it includes the tool version, window, and repos). Add `--verbose` for progress
and rate-limit logging on stderr.

## Reliability

- Retries with exponential backoff + jitter on `5xx` and rate-limit responses.
- Honors `Retry-After` and pre-empts the primary rate-limit window using the
  `X-RateLimit-Remaining` / `-Reset` headers (one jobs call per run, so large
  orgs generate many calls against the ~5,000/hour limit).
- A missing or inaccessible repo is skipped with a warning rather than aborting
  the whole run.

## Building and publishing

```bash
make test                                  # run the unit suite
make build  IMAGE=myorg/gha-concurrency    # local single-arch image
make buildx IMAGE=myorg/gha-concurrency    # multi-arch (amd64+arm64), push
```

`buildx` depends on `test`, so a failing test blocks publishing. CI in
`.github/workflows/publish.yml` does the same and pushes on a `v*` tag with
provenance + SBOM attestations.

### Reproducibility

The image installs nothing — just copies one stdlib script onto a pinned Python
base — so builds are deterministic given a pinned base. For byte-level
reproducibility, pin the base by digest in the `Dockerfile`:

```dockerfile
FROM python:3.12.7-slim-bookworm@sha256:<digest>
```

Resolve the current digest with `docker buildx imagetools inspect
python:3.12.7-slim-bookworm`.

## Security model (what to tell a customer's security team)

- Read-only: only authenticated `GET` requests, only to the configured GitHub
  API host.
- The token is read from the environment, never written to disk, never logged,
  never sent anywhere except as the `Authorization` header to GitHub.
- No outbound calls to any third party — no telemetry, no "phone home."
- Runs as a non-root user (UID 10001).
- The entire tool is a single ~300-line dependency-free script; diff it against
  this repo and audit it in one sitting.

Customers run it themselves and send you the JSON output — never their token.

## License

MIT.
