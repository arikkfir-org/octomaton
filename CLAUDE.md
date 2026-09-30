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
- The hub reference (`arikkfir-org/docs`, `hub/reference.md` → Octomaton) is the contract for the hub's deployment
  (`deploy/`) and with `delivery`: server config, `.octomaton.yaml` schema, template context, names, mount paths,
  endpoints, App permissions and RBAC. Change it there first; keep README.md's schema blocks identical to it.

## Code

- Go 1.27; module `octomaton.dev` (a vanity path: `https://octomaton.dev` serves the `go-import` tag pointing at
  `github.com/arikkfir-org/octomaton`).
- Layers (see README.md → Architecture): `internal/services` hold the CI logic in Octomaton's own terms;
  `internal/services/ci` defines the vocabulary and the ports (`CodeHost`, `Runner`). `internal/adapters` implement the
  ports (`github`, `tekton`) and the plumbing (`http`, `kube`, `leader`, `relay`). `internal/system` holds `config`,
  `telemetry`, `metrics` and `buildinfo`.
- Dependencies point inward, and `internal/architecture` fails the build otherwise. `services/ci` imports only the
  standard library. Services never import adapters, `k8s.io/*`, go-github or Tekton. Adapters import `services/ci`,
  `services/pipelines`, `adapters/kube` and `system/*`, never another service.
- `cmd/octomaton` only coordinates startup and shutdown: signals, telemetry, configuration, wiring. Logic goes into an
  `internal/` package; keep functions short.
- The server is configured by environment variables only, read with `github.com/kelseyhightower/envconfig`
  (`internal/system/config`; `internal/system/telemetry` reads `OCTOMATON_LOG_LEVEL`): no flags, no configuration
  files. Configuration errors never print secret values.
- Tekton objects are `unstructured.Unstructured` with the dynamic client; do not import `github.com/tektoncd/pipeline`.
- `.octomaton.yaml` is parsed with `go.yaml.in/yaml/v3` (YAML 1.2, `KnownFields(true)`); never with a YAML 1.1 parser
  (an unquoted `on` would become `true`). PipelineRun files use the Kubernetes YAML reader.
- Services reach GitHub and Tekton only through the ports. GitHub payloads, check-run shapes and markers stay in
  `adapters/github`; Tekton labels, annotations and status stay in `adapters/tekton`. Keep packages small and
  dependency-injected.
- Runs are created held, then their report, task reports and token, then released per concurrency policy; any failure
  in between cancels the run and fails its report. Keep every step idempotent (Resume replays them).
- Logs: `log/slog`, set up by `internal/system/telemetry`: JSON with the fields Cloud Logging reads on GKE, text
  elsewhere. Messages start with a capital letter; errors are logged once, where handled, with the request's context so
  they link to its trace. Metrics (`internal/system/metrics`) and traces go through OpenTelemetry to Cloud Monitoring
  and Cloud Trace (the Telemetry API), on GKE only.

## Deployment

- `deploy/` is the hub's deployment of Octomaton (Kustomize). The Argo CD Application `octomaton` in
  `arikkfir-org/delivery` applies it from `main` and tags the image with that commit's short SHA, so every merge to
  `main` deploys itself. The code never reads `deploy/`.
- Follow `delivery`'s rules there: pin every other image; secrets only as `ExternalSecret`s on the `ClusterSecretStore`
  `gcp-secret-manager`; a NetworkPolicy admitting only the `traefik` namespace to every served pod. Leave the
  `octomaton` image untagged: Argo CD sets the tag.
- `release` must publish an image for every push to `main`, so it never gets a `paths` filter: Argo CD runs the image of
  whatever commit `main` is at.
- CI renders `deploy/` and validates it with kubeconform; run `kubectl kustomize deploy` before pushing a change there.

## Tests

- Every change needs table-driven tests; run `go vet ./... && go test -race ./...` (or `make test`) before finishing.
- Test services against the in-memory fakes in `services/ci/citest`. Test adapters against `githubtest.Server` and
  client-go fakes. `internal/e2e` covers the webhook-to-check path through the real adapters.
- `make lint` validates this repository's own `.octomaton.yaml` and `.tekton/` files.

## Commands

- `make test`, `make lint`, `make build`, `make image` (ko; needs registry credentials).
- Releases have no version tags: every push to `main` publishes an image tagged with the commit's short SHA, which is
  also the version the binary reports.
- `go run ./cmd/octomaton-lint -render .` prints the PipelineRuns as Octomaton would create them.
- Do not add GitHub Actions workflows; CI runs through Octomaton itself (`.octomaton.yaml`).
