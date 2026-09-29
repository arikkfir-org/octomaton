VERSION        ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
KO_DOCKER_REPO ?= me-west1-docker.pkg.dev/arikkfir/images/octomaton
GO             ?= go

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
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "-s -w -X octomaton.dev/internal/buildinfo.version=$(VERSION)" -o bin/ ./cmd/...

## image: build and push the image with ko (needs registry credentials)
image:
	VERSION=$(VERSION) KO_DOCKER_REPO=$(KO_DOCKER_REPO) ko build --bare --tags=$(VERSION) ./cmd/octomaton

tidy:
	$(GO) mod tidy

fmt:
	gofmt -w .

clean:
	rm -rf bin
