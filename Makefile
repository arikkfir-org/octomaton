# The version is the commit's short SHA, as the release pipeline tags it.
VERSION        ?= $(or $(shell git rev-parse HEAD 2>/dev/null | cut -c1-7),dev)
KO_DOCKER_REPO ?= me-west1-docker.pkg.dev/arikkfir/images/octomaton
GO             ?= go
KO             ?= $(GO) run github.com/google/ko@v0.19.1

.PHONY: all test vet lint build image tidy fmt clean

all: test build

## test: go vet and the race-enabled test suite
test: vet
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

## lint: validate this repository's own .octomaton.yaml and PipelineRuns
lint:
	$(GO) run ./cmd/octomaton-lint .

## build: static binaries in bin/: octomaton (the server) and octomaton-lint
build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "-s -w -X octomaton.dev/internal/system/buildinfo.version=$(VERSION)" -o bin/ ./cmd/...

## image: build and push the image of HEAD with ko, tagged with its short SHA (needs registry credentials)
image:
	@git diff --quiet HEAD || { echo "Commit your changes first: the image is tagged with the commit" >&2; exit 1; }
	VERSION=$(VERSION) KO_DOCKER_REPO=$(KO_DOCKER_REPO) $(KO) build --bare --tags=$(VERSION) ./cmd/octomaton

tidy:
	$(GO) mod tidy

fmt:
	gofmt -w .

clean:
	rm -rf bin
