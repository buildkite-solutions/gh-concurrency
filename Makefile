BINARY     ?= gh-concurrency
GO         ?= go
IMAGE      ?= ghcr.io/buildkite-solutions/gh-concurrency
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT     ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE       ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
PLATFORMS  ?= linux/amd64,linux/arm64
ARGS       ?= --help

LD_FLAGS = -ldflags "-s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)"
BUILD_ARGS = --build-arg VERSION=$(VERSION) \
             --build-arg COMMIT=$(COMMIT) \
             --build-arg DATE=$(DATE)

.PHONY: test fmt build run docker-build docker-publish release-binaries clean

test:
	$(GO) test ./...

fmt:
	$(GO) fmt ./...

build: test
	$(GO) build -trimpath $(LD_FLAGS) -o $(BINARY) .

run: build
	./$(BINARY) $(ARGS)

docker-build: test
	docker build $(BUILD_ARGS) -t $(IMAGE):$(VERSION) -t $(IMAGE):latest .

docker-publish: test
	docker buildx build --platform $(PLATFORMS) $(BUILD_ARGS) \
		-t $(IMAGE):$(VERSION) -t $(IMAGE):latest --push .

release-binaries: test
	GO=$(GO) scripts/build-release.sh $(VERSION)

clean:
	rm -rf dist $(BINARY)
