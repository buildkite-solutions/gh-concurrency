#!/usr/bin/env bash
set -euo pipefail

version="${VERSION:-${RELEASE_TAG:-${BUILDKITE_TAG:-}}}"
check_auth=false
for arg in "$@"; do
  if [[ "${arg}" == "--check-auth" ]]; then
    check_auth=true
  fi
done

if [[ "${check_auth}" != "true" && -z "${version}" ]]; then
  echo "VERSION, RELEASE_TAG, or BUILDKITE_TAG is required" >&2
  exit 2
fi

if [[ -n "${version}" ]]; then
  export VERSION="${version}"
fi

go run ./scripts/github-release.go "$@"
