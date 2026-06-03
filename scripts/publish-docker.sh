#!/usr/bin/env bash
set -euo pipefail

version="${VERSION:-${BUILDKITE_TAG:-}}"
if [[ -z "${version}" ]]; then
  echo "VERSION or BUILDKITE_TAG is required" >&2
  exit 2
fi

image="${IMAGE:-ghcr.io/buildkite-solutions/gh-concurrency}"
username="${GHCR_USERNAME:-}"
token="${GHCR_TOKEN:-}"

if [[ -z "${token}" ]]; then
  echo "GHCR_TOKEN is required to publish to GHCR" >&2
  exit 2
fi

if [[ -z "${username}" ]]; then
  echo "GHCR_USERNAME is required to publish to GHCR" >&2
  exit 2
fi

echo "${token}" | docker login ghcr.io -u "${username}" --password-stdin
make docker-publish IMAGE="${image}" VERSION="${version}"
