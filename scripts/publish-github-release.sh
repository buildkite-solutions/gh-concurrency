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

VERSION="${version}" go run ./scripts/github-release.go
