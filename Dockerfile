# syntax=docker/dockerfile:1

FROM golang:1.25-bookworm AS build

ARG VERSION=dev
ARG COMMIT=unknown
ARG DATE=unknown

WORKDIR /src
COPY go.mod ./
COPY main.go ./

RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${DATE}" \
    -o /out/gh-concurrency .

FROM scratch

ARG VERSION=dev
ARG COMMIT=unknown
ARG DATE=unknown

LABEL org.opencontainers.image.title="gh-concurrency" \
      org.opencontainers.image.description="Estimate GitHub Actions job concurrency for Buildkite sizing" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.created="${DATE}" \
      org.opencontainers.image.source="https://github.com/buildkite-solutions/gh-concurrency" \
      org.opencontainers.image.licenses="MIT"

COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/gh-concurrency /gh-concurrency

USER 10001:10001
ENTRYPOINT ["/gh-concurrency"]
CMD ["--help"]
