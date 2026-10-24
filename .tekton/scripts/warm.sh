#!/bin/sh
# Builds what ci compiles (.tekton/ci.task.yaml), as it builds it, into the Go caches go-cache-save.sh keeps
# (docs/octomaton/designs/ci-caches.md): lint's binary, what vet checks against, the tests with the race detector
# (linked, never run) and the static binaries. A warm-up that fails costs later runs time, never this run its result.
set -eu
go run ./cmd/octomaton-lint -version >/dev/null || echo "Couldn't warm lint"
go vet ./... || echo "Couldn't warm vet"
go test -race -count=1 -run '^$' -exec /bin/true ./... >/dev/null || echo "Couldn't warm the tests"
CGO_ENABLED=0 go build -trimpath -o /tmp/bin/ ./cmd/... || echo "Couldn't warm the build"
