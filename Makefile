# The version is the commit's short SHA, as the release pipeline tags it.
VERSION        ?= $(or $(shell git rev-parse HEAD 2>/dev/null | cut -c1-7),dev)
KO_DOCKER_REPO ?= me-west1-docker.pkg.dev/arikkfir/images/octomaton
GO             ?= go
KO             ?= $(GO) run github.com/google/ko@v0.19.1
K8S_IMAGE      ?= docker.io/alpine/k8s:1.37.1

.PHONY: all test vet lint manifests build image tidy fmt clean

all: test build

## test: go vet and the race-enabled test suite
test: vet
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

## lint: validate this repository's own .octomaton.yaml and PipelineRuns
lint:
	$(GO) run ./cmd/octomaton-lint .

## manifests: render deploy/ as Argo CD deploys it, as written and as delivery's main overrides it, and validate it
manifests:
	rm -rf .ci/manifests
	docker run --rm -v "$(CURDIR):/src" -w /src -u "$$(id -u):$$(id -g)" -e HOME=/tmp -e REVISION=$(VERSION) \
		--entrypoint sh $(K8S_IMAGE) deploy/manifests.sh .ci/manifests

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
	rm -rf bin .ci
