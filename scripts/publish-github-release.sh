#!/usr/bin/env bash
set -euo pipefail

version="${VERSION:-${BUILDKITE_TAG:-}}"
if [[ -z "${version}" ]]; then
  echo "VERSION or BUILDKITE_TAG is required" >&2
  exit 2
fi

VERSION="${version}" go run ./scripts/github-release.go
