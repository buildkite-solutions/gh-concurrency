#!/usr/bin/env bash
set -euo pipefail

version="${VERSION:-${BUILDKITE_TAG:-}}"
if [[ -z "${version}" ]]; then
  echo "VERSION or BUILDKITE_TAG is required" >&2
  exit 2
fi

token="${GITHUB_TOKEN:-${GH_TOKEN:-}}"
if [[ -z "${token}" ]]; then
  echo "GITHUB_TOKEN or GH_TOKEN is required to publish a GitHub release" >&2
  exit 2
fi
export GH_TOKEN="${token}"

if ! command -v gh >/dev/null 2>&1; then
  echo "gh is required to publish a GitHub release" >&2
  exit 2
fi

notes="Install with:

\`\`\`bash
gh extension install buildkite-solutions/gh-concurrency
\`\`\`

Docker image:

\`\`\`bash
docker pull ghcr.io/buildkite-solutions/gh-concurrency:${version}
\`\`\`"

if gh release view "${version}" >/dev/null 2>&1; then
  gh release upload "${version}" dist/* --clobber
else
  gh release create "${version}" dist/* --title "${version}" --notes "${notes}"
fi
