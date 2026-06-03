#!/usr/bin/env bash
set -euo pipefail

version="${1:-${BUILDKITE_TAG:-}}"
if [[ -z "${version}" ]]; then
  echo "usage: scripts/build-release.sh v1.0.0" >&2
  exit 2
fi

commit="${COMMIT:-$(git rev-parse --short HEAD 2>/dev/null || echo unknown)}"
date="${DATE:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}"
go_cmd="${GO:-go}"

rm -rf dist
mkdir -p dist

targets=(
  "darwin/amd64"
  "darwin/arm64"
  "linux/amd64"
  "linux/arm64"
  "windows/amd64"
)

for target in "${targets[@]}"; do
  goos="${target%/*}"
  goarch="${target#*/}"
  ext=""
  if [[ "${goos}" == "windows" ]]; then
    ext=".exe"
  fi

  output="dist/gh-concurrency_${version}_${goos}-${goarch}${ext}"
  echo "building ${output}"
  CGO_ENABLED=0 GOOS="${goos}" GOARCH="${goarch}" "${go_cmd}" build -trimpath \
    -ldflags "-s -w -X main.version=${version} -X main.commit=${commit} -X main.date=${date}" \
    -o "${output}" .

  if [[ "${goos}" != "windows" ]]; then
    chmod +x "${output}"
  fi
done

if command -v sha256sum >/dev/null 2>&1; then
  (cd dist && sha256sum * > checksums.txt)
else
  (cd dist && shasum -a 256 * > checksums.txt)
fi
