# syntax=docker/dockerfile:1

# Pinned to a specific patch release for reproducibility. For a fully
# byte-reproducible base, pin by digest instead (see README):
#   FROM python:3.12.7-slim-bookworm@sha256:<digest>
FROM python:3.12.7-slim-bookworm AS runtime

# Build-time metadata. Override via --build-arg (the Makefile/CI does this).
ARG VERSION=dev
ARG VCS_REF=unknown
ARG BUILD_DATE=unknown
LABEL org.opencontainers.image.title="gha-concurrency" \
      org.opencontainers.image.description="Estimate GitHub Actions job concurrency for Buildkite cost comparison" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${VCS_REF}" \
      org.opencontainers.image.created="${BUILD_DATE}" \
      org.opencontainers.image.licenses="MIT"

# The tool is stdlib-only: nothing to pip install, no wheel cache, no apt
# packages. That keeps the image tiny, the attack surface minimal, and the
# whole thing auditable by a customer security team in one read.

# Run as an unprivileged, fixed UID rather than root.
RUN useradd --create-home --uid 10001 app
WORKDIR /home/app

COPY --chown=app:app gha_concurrency.py /app/gha_concurrency.py
USER app

# Stream logs in real time; don't litter .pyc files into a read-only layer.
ENV PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1

# Args after the image name flow straight to the script:
#   docker run --rm -e GITHUB_TOKEN img --repo o/r --since 2025-05-01
ENTRYPOINT ["python3", "/app/gha_concurrency.py"]
CMD ["--help"]
