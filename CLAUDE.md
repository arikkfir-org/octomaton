# Octomaton: rules for agents

## Principles (inviolable)

- Application-agnostic: Octomaton knows nothing about what a repository builds, its language, layout, CI or CD. No
  conventional directories (`.tekton/` is only a path users choose), no defaults that assume a layout, no knowledge of
  specific repositories.
- The only repository files Octomaton reads are the root `.octomaton.yaml` and the PipelineRun files it references.
- Configuration lives only in `.octomaton.yaml`, never in Tekton labels or annotations. Labels and annotations under
  `octomaton.dev/` (and `app.kubernetes.io/managed-by`) are bookkeeping Octomaton writes on its own objects.
- PipelineRun files stay plain Tekton YAML: no templating inside them. Context goes in only through `params` (Go
  templates over the documented context) and the optional `githubToken` workspace.
- The hub reference (`arikkfir-org/docs`, `hub/reference.md` → Octomaton) is the contract with `delivery`: server
  config, `.octomaton.yaml` schema, template context, names, mount paths, endpoints, App permissions and RBAC. Change
  it there first; keep README.md's schema blocks identical to it.

## Code

- Go 1.27; module `octomaton.dev` (a vanity path: `https://octomaton.dev` serves the `go-import` tag pointing at
  `github.com/arikkfir-org/octomaton`).
- Layout: `cmd/octomaton` (the server's launcher), `cmd/octomaton-lint`; `internal/` packages `config`, `telemetry`,
  `http`, `leader`, `kube`, `buildinfo`, `repoconfig`, `tmpl`, `githubapp` (+ `githubtest` fake API), `webhook`,
  `trigger`, `tekton`, `reporter`, `checkrun`, `relay`, `lint`, `metrics` (+ `metricstest`), `e2e`.
- `cmd/octomaton` only coordinates startup and shutdown: signals, telemetry, configuration, wiring. Logic goes into an
  `internal/` package; keep functions short.
- The server is configured by environment variables only, read with `github.com/kelseyhightower/envconfig`
  (`internal/config`; `internal/telemetry` reads `OCTOMATON_LOG_LEVEL`): no flags, no configuration files. Configuration
  errors never print secret values.
- Tekton objects are `unstructured.Unstructured` with the dynamic client; do not import `github.com/tektoncd/pipeline`.
- `.octomaton.yaml` is parsed with `go.yaml.in/yaml/v3` (YAML 1.2, `KnownFields(true)`); never with a YAML 1.1 parser
  (an unquoted `on` would become `true`). PipelineRun files use the Kubernetes YAML reader.
- GitHub access goes through `githubapp.Client`/`Provider`; Kubernetes through small interfaces (`trigger.Runs`,
  `reporter.Runs`) implemented by `tekton.Client`. Keep packages small and dependency-injected.
- Runs are created held, then check run, task checks, token Secret, then released per concurrency policy; any failure
  in between cancels the run and fails its check. Keep every step idempotent (Resume replays them).
- Logs: `log/slog`, set up by `internal/telemetry`: JSON with the fields Cloud Logging reads on GKE, text elsewhere.
  Messages start with a capital letter; errors are logged once, where handled, with the request's context so they link
  to its trace. Metrics (`internal/metrics`) and traces go through OpenTelemetry to Cloud Monitoring and Cloud Trace
  (the Telemetry API), on GKE only.

## Tests

- Every change needs table-driven tests; run `go vet ./... && go test -race ./...` (or `make test`) before finishing.
- Use `githubtest.Server` for GitHub and client-go fakes for Kubernetes; `internal/e2e` covers the webhook-to-check path.
- `make lint` validates this repository's own `.octomaton.yaml` and `.tekton/` files.

## Commands

- `make test`, `make lint`, `make build`, `make image` (ko; needs registry credentials).
- `go run ./cmd/octomaton-lint -render .` prints the PipelineRuns as Octomaton would create them.
- Do not add GitHub Actions workflows; CI runs through Octomaton itself (`.octomaton.yaml`).
