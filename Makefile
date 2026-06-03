# Override IMAGE to your own Docker Hub namespace, e.g.:
#   make buildx IMAGE=myorg/gha-concurrency
IMAGE      ?= YOURDOCKERHUBUSER/gha-concurrency
VERSION    ?= $(shell sed -n 's/^VERSION = "\(.*\)"/\1/p' gha_concurrency.py)
VCS_REF    ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
PLATFORMS  ?= linux/amd64,linux/arm64
ARGS       ?= --help

BUILD_ARGS = --build-arg VERSION=$(VERSION) \
             --build-arg VCS_REF=$(VCS_REF) \
             --build-arg BUILD_DATE=$(BUILD_DATE)

.PHONY: test build buildx run lint clean

test:                       ## Run the unit suite (no network, no deps)
	python3 -m unittest -v

build: test                 ## Build a local single-arch image
	docker build $(BUILD_ARGS) -t $(IMAGE):$(VERSION) -t $(IMAGE):latest .

buildx: test                ## Build + push multi-arch (requires `docker login`)
	docker buildx build --platform $(PLATFORMS) $(BUILD_ARGS) \
		--provenance=true --sbom=true \
		-t $(IMAGE):$(VERSION) -t $(IMAGE):latest --push .

run: build                  ## Run locally; pass flags via ARGS="--repo o/r --since 2025-05-01"
	docker run --rm -e GITHUB_TOKEN -e GITHUB_API_URL $(IMAGE):$(VERSION) $(ARGS)

lint:                       ## Byte-compile check
	python3 -m py_compile gha_concurrency.py test_gha_concurrency.py

clean:
	docker image rm $(IMAGE):$(VERSION) $(IMAGE):latest 2>/dev/null || true
