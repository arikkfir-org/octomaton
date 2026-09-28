VERSION        ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
KO_DOCKER_REPO ?= me-west1-docker.pkg.dev/arikkfir/images/switchboard
GO             ?= go

.PHONY: all test vet lint build image tidy fmt clean

all: test build

## test: go vet and the race-enabled test suite
test: vet
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

## lint: validate this repository's own .switchboard.yaml and PipelineRuns
lint:
	$(GO) run ./cmd/switchboard lint .

## build: a static binary in bin/
build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o bin/switchboard ./cmd/switchboard

## image: build and push the image with ko (needs registry credentials)
image:
	VERSION=$(VERSION) KO_DOCKER_REPO=$(KO_DOCKER_REPO) ko build --bare --tags=$(VERSION) ./cmd/switchboard

tidy:
	$(GO) mod tidy

fmt:
	gofmt -w .

clean:
	rm -rf bin
