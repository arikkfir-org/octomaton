# Octomaton architecture: services, adapters, system

**Decision**: Octomaton's code splits into three layers, wired together by `cmd/octomaton`:

- **`services`** hold the CI logic and speak only Octomaton's own terms: "start this pipeline for that trigger", "report
  this run", "tell me when a run changes".
- **`adapters`** turn those terms into GitHub, Tekton, Kubernetes and HTTP calls. Callers never see which technology is
  underneath.
- **`system`** configures the process: configuration, telemetry, metrics, the version.

Dependencies point inward: adapters use the services' vocabulary; services never import an adapter or a provider's
SDK. Behaviour, configuration and every external name stay as they are. This reshapes
[octomaton#1](https://github.com/arikkfir-org/octomaton/pull/1) before its review; it changes neither Octomaton's
[configuration](https://github.com/arikkfir-org/octomaton#server-configuration) nor its [deployment](deployment.md).
What Octomaton does is in the [Phase 2 design](phase-2-octomaton.md).

## Why

The core logic (`trigger` and `reporter`, 3,300 lines) is written against the technologies it drives:

| Coupling | Where | Cost |
| --- | --- | --- |
| Tekton objects are the core's data model: `*unstructured.Unstructured`, about 25 label and annotation keys as its state, Tekton status conditions | `trigger`, `reporter` | Every rule (concurrency, supersede, resume, reporting) is also a Kubernetes patch; testing a rule needs a fake API server |
| go-github payloads and check-run structs | `trigger/events.go`, `reporter` | GitHub's shapes leak into run logic |
| client-go informer and workqueue | `reporter` | The reporter is half controller, half report writer |
| The trigger context, GitHub check-run markers and Tekton Dashboard links in one package | `checkrun` | Domain and presentation are mixed |

## Layers

```mermaid
flowchart TB
  CMD["cmd/octomaton<br/>starts and stops everything, wires the layers"]
  subgraph SVC["services"]
    direction TB
    RUNS["runs"] & REP["reports"] & SCH["schedules"] & UPK["upkeep"] --> CI["ci<br/>vocabulary and ports"]
    RUNS & SCH --> PIPE["pipelines<br/>.octomaton.yaml"]
  end
  subgraph ADP["adapters"]
    direction TB
    GH["github"] ~~~ HTTP["http"] ~~~ REL["relay"]
    TEK["tekton"] --> KUBE["kube"]
    LEAD["leader"] --> KUBE
  end
  subgraph SYS["system"]
    CFG["config"] ~~~ TEL["telemetry"] ~~~ MET["metrics"]
  end
  CMD --> SVC & ADP & SYS
  ADP -- "implement the ports,<br/>speak the vocabulary" --> CI
  SVC -. "record" .-> MET
  ADP --> EXT[("GitHub<br/>Kubernetes, Tekton")]
  TEL --> GCP[("Google Cloud")]
```

Arrows are imports. `services/ci` declares the ports (Go interfaces); adapters implement them, and `cmd/octomaton`
hands the implementations to the services. `tekton` and `leader` share the Kubernetes clients of `kube`.

## Rules

| Package | May import | Never imports |
| --- | --- | --- |
| `internal/services/ci` | the standard library | anything else, in or outside the module |
| `internal/services/*` | `services/ci`, `services/pipelines`, other services, `system/metrics`, the standard library, libraries that aren't provider SDKs (YAML, globs, cron) | `internal/adapters/*`, `k8s.io/*`, go-github, Tekton |
| `internal/adapters/*` | `services/ci`, `services/pipelines` (to parse it), `adapters/kube`, provider SDKs, `system/*` | the other services |
| `internal/system/*` | the standard library, OpenTelemetry, envconfig | `services`, `adapters` |
| `cmd/octomaton` | everything | |

A test (`internal/architecture`) walks the imports of every package, tests included, and fails on any violation, so
the layering can't erode. A new package without a rule fails it too.

## Packages

```text
cmd/octomaton/            launcher: signals, system, adapters, services, run
cmd/octomaton-lint/       the linter's launcher
internal/system/          config, telemetry, metrics (+ metricstest), buildinfo
internal/services/
  ci/                     vocabulary (Repository, Trigger, Event, Run, Outcome, Report) and ports (CodeHost, Runner)
    citest/               in-memory CodeHost and Runner for the services' tests
  pipelines/              .octomaton.yaml: schema, event matching, templates
  runs/                   events to runs: evaluation, start, concurrency, re-runs, comment commands
  reports/                run state to reports: titles, task tables, failure logs, task checks
  schedules/              cron triggers to runs
  upkeep/                 token refresh, retention of finished runs' resources
  lint/                   validating a repository's configuration, rendering through a Renderer
internal/adapters/
  github/                 CodeHost: App auth, REST calls, check-run markers (+ githubtest); webhook payloads to ci.Event
  tekton/                 Runner: PipelineRun rendering, bookkeeping labels and annotations, status to ci.Run, Watch;
                          Renderer for the linter; the repository-to-namespace mapping
  kube/                   Kubernetes clients
  leader/                 Lease election; cmd/octomaton runs the leader's jobs through it
  http/                   server, readiness, the webhook endpoint (signature, deduplication, worker pool)
  relay/                  forwarding verified deliveries
internal/architecture/    the import rules, as a test
internal/e2e/             signed webhooks through the real adapters over fakes
```

| Today | Becomes |
| --- | --- |
| `config`, `telemetry`, `metrics`, `buildinfo` | `system/*`, as they are |
| `repoconfig`, `tmpl` | `services/pipelines` |
| `checkrun.Context` | `ci.Trigger`; the marker encoding moves to `adapters/github` |
| `trigger` (evaluate, start, concurrency, rerun, comments) | `services/runs` |
| `trigger` (schedules), `trigger` (maintenance) | `services/schedules`, `services/upkeep` |
| `reporter` (titles, task table, failure logs, task checks) | `services/reports` |
| `reporter` (informer, workqueue) | `adapters/tekton`, behind `Runner.Watch` |
| `tekton` | `adapters/tekton` |
| `githubapp` (+ `githubtest`), `trigger/events.go` | `adapters/github` |
| `webhook`, `http` | `adapters/http` |
| `kube`, `leader`, `relay` | `adapters/*` |
| `lint` | `services/lint`, rendering through the Tekton adapter |

## Vocabulary and ports

| Type (`services/ci`) | Meaning | Today |
| --- | --- | --- |
| `Repository` | Owner, name, IDs, default branch | `checkrun.Repository` |
| `Trigger` | Why pipelines are considered: event, revision, ref, pull request, comment, schedule. Stored with every run and report, so a re-run replays it | `checkrun.Context` |
| `Event` | A decoded delivery: push, pull request, merge group, comment, re-run request, approval | go-github payloads |
| `Run` | One pipeline run: ID (tenant and name), trigger, pipeline, attempt, concurrency, phase (held, running, finished), outcome, tasks, results, what was reported | `*unstructured.Unstructured` and its annotations |
| `Outcome` | How a finished run ended: success, failure, cancelled, timed out or skipped, with a reason | `tekton.Outcome` |
| `Report` | The status shown on the code host: name, status, conclusion, title, summary, link, trigger | GitHub check-run options |

The ports, as built. The deviations from the first sketch are listed under [As built](#as-built).

```go
// CodeHost is GitHub: the App's installations, repository tokens and the permissions they can carry.
type CodeHost interface {
	Accounts(ctx context.Context) ([]Account, error) // installations of the served owners
	Installation(id int64) Installation
	RepositoryToken(ctx context.Context, installationID, repositoryID int64, permissions map[string]string) (Token, error)
	CheckPermissions(permissions map[string]string) error // githubToken.permissions GitHub can grant
}

type Installation interface {
	Repositories(ctx context.Context) ([]Repository, error)
	ReadFile(ctx context.Context, repo Repository, path, ref string) ([]byte, error) // ErrNotFound
	PullRequestFiles(ctx context.Context, repo Repository, number int) (ChangedFiles, error)
	CompareFiles(ctx context.Context, repo Repository, base, head string) (ChangedFiles, error)
	PullRequest(ctx context.Context, repo Repository, number int) (PullRequestState, error)
	BranchHead(ctx context.Context, repo Repository, branch string) (string, error)
	Permission(ctx context.Context, repo Repository, user string) (Permission, error)
	OpenReport(ctx context.Context, repo Repository, r Report) (ReportID, error)
	UpdateReport(ctx context.Context, repo Repository, id ReportID, r Report) error
	FindReport(ctx context.Context, repo Repository, revision, name, externalID string) (ReportID, error)
	ReportTrigger(ctx context.Context, repo Repository, id ReportID) (*Trigger, error)
	SuiteReports(ctx context.Context, repo Repository, suiteID int64) ([]ReportRef, error)
	React(ctx context.Context, repo Repository, commentID int64, reaction string) error
	Comment(ctx context.Context, repo Repository, number int, body string) error
}

// Runner is the CI system that executes pipelines: Tekton on Kubernetes.
type Runner interface {
	Expand(ctx context.Context, spec RunSpec, read FileReader) ([]byte, error) // the definition with the files it references inlined
	Check(ctx context.Context, spec RunSpec) error                     // a *Refusal before anything exists
	Create(ctx context.Context, spec RunSpec, attempt int) (Run, error) // held; ErrExists when the attempt exists
	Get(ctx context.Context, id RunID) (Run, error)
	List(ctx context.Context, q RunQuery) ([]Run, error)
	Release(ctx context.Context, id RunID) error
	Cancel(ctx context.Context, id RunID, why Cancellation) error
	Record(ctx context.Context, id RunID, r Record) error // what was reported, report IDs, progress, …
	SetToken(ctx context.Context, run Run, t Token) error
	TokenExpiry(ctx context.Context, id RunID) (expires time.Time, ok bool, err error)
	Details(ctx context.Context, id RunID) (Details, error) // tasks, results, failed steps
	StepLogs(ctx context.Context, id RunID, step Step, tailLines, limitBytes int64) (string, error)
	Link(id RunID) RunLink               // where people watch a run
	TaskURL(id RunID, task string) string // and one of its tasks
	FreeResources(ctx context.Context, finishedBefore time.Time) (int, error)
	Watch(ctx context.Context, w Watcher) error // until ctx ends
}

// Watcher is told about runs that changed; services/reports implements it.
type Watcher interface {
	Reconcile(ctx context.Context, run Run) (again time.Duration, err error)
	Deleted(ctx context.Context, run Run) error // deleted before it was reported
}
```

## Flows

An event becomes a run. Every call below the dashed line goes through a port:

```mermaid
sequenceDiagram
  participant G as GitHub
  participant H as adapters/http
  participant A as adapters/github
  participant R as services/runs
  participant C as CodeHost (adapters/github)
  participant T as Runner (adapters/tekton)
  G->>H: POST /github/hooks
  H->>A: verify signature, decode
  A-->>H: ci.Event
  H->>R: Handle(event), on a worker
  Note over R,T: ports only
  R->>C: ReadFile(.octomaton.yaml, revision)
  R->>R: pipelines: match triggers, render params
  R->>C: ReadFile(pipelineRun file)
  R->>T: Check, then Create (held)
  R->>C: OpenReport (queued)
  R->>C: RepositoryToken, then T: SetToken
  R->>T: Release, or wait per the concurrency policy
```

A run's changes become reports. Only the leader watches:

```mermaid
sequenceDiagram
  participant T as Runner (adapters/tekton)
  participant P as services/reports
  participant R as services/runs
  participant C as CodeHost (adapters/github)
  T->>P: Watch: Reconcile(run)
  P->>C: UpdateReport (in progress, task table, conclusion)
  P->>T: Record(reported)
  P->>R: finished: ReleaseNext, held too long: Resume
  R->>T: Release the next held run of its group, or finish starting this one
```

`cmd/octomaton` hands `runs.Service.ReleaseNext` and `Resume` to `reports.Service`, so neither service holds the
other.

## Behaviour and contract

Unchanged: environment variables, endpoints, check names and markers (written only by `adapters/github`), labels and
annotations (written only by `adapters/tekton`), metrics, RBAC and IAM: neither the README nor the
[deployment](deployment.md) changes. The few behaviour changes are listed under [As built](#as-built).

## Decisions

| Decision | Why | Rejected |
| --- | --- | --- |
| One vocabulary package, `services/ci`, holding the types and the ports | Services and adapters share one language. The ports sit with the core, so adapters depend on it, never the reverse | Interfaces declared in each service (adapters would import several services); a separate `domain` layer (a fourth layer for about 300 lines) |
| Tekton labels and annotations only in `adapters/tekton`; the core reads and writes `ci.Run` fields | Concurrency, supersede and reporting rules become testable with an in-memory runner | Keeping `unstructured` objects in the core |
| The informer and workqueue behind `Runner.Watch` | "Tell me when a run changes" is the core's need; how Kubernetes delivers it is the adapter's | The reporter owning the controller |
| GitHub payloads decoded in `adapters/github` | The core sees `ci.Event`, never go-github types | Decoding in the core |
| In-memory `Runner` and `CodeHost` for the services' tests; `githubtest` and client-go fakes for the adapters' and the end-to-end tests | Rule tests are fast and need no Kubernetes; adapters are still tested against realistic fakes | End-to-end tests only |
| An architecture test on imports | Keeps the layering from eroding | Convention only |

## Migration

Every step keeps `go vet`, `go test -race` and the end-to-end test green. All of it lands in octomaton#1.

1. Move `config`, `telemetry`, `metrics` and `buildinfo` under `system`, and `kube`, `leader`, `http`, `relay` and
   `webhook` under `adapters`. This is mechanical.
2. Add `services/ci` (types and ports) and `services/pipelines` (from `repoconfig` and `tmpl`).
3. `adapters/github` implements `CodeHost` and decodes webhook payloads; the check-run markers move there.
4. `adapters/tekton` implements `Runner`: bookkeeping, the status mapping, and `Watch` (the reporter's informer).
5. Rewrite `trigger` as `services/runs`, `schedules` and `upkeep`, and `reporter` as `services/reports`, against the
   ports, tested with in-memory fakes.
6. Rewire `cmd/octomaton`, point the end-to-end test at the real adapters over fakes, add the architecture test, and
   update the README and CLAUDE.md.

## As built

All six steps landed in octomaton#1. Where the code differs from the design above:

| Design | As built | Why |
| --- | --- | --- |
| A `Leadership` port in `services/ci` | No port. `cmd/octomaton` runs the leader's jobs through `adapters/leader` | No service asks who leads; only the launcher decides what runs on the leader |
| `CodeHost.Installations` | `Accounts`: installation IDs with their owners, filtered by `OCTOMATON_GITHUB_ALLOWED_OWNERS` in `adapters/github` | Which owners are served is GitHub's concern |
| — | `CodeHost.CheckPermissions` | `pipelines` checks `githubToken.permissions` against what GitHub grants, in the server and in `octomaton-lint` |
| `Installation.ChangedFiles(trigger)` | `PullRequestFiles` and `CompareFiles` | Which range counts as changed for a trigger is a rule of the core |
| `Installation.Report` | `ReportTrigger` and `SuiteReports` | A re-run needs only a report's stored trigger, or a suite's reports |
| `Runner.Prepare`, then `Create(prepared)` | `Check(spec)`, then `Create(spec, attempt)` | The runner renders at creation; `Check` refuses (as a `ci.Refusal`) before anything exists |
| `Runner.TenantExists` | Part of `Check` | A missing namespace is one of the refusals |
| `Runner.TaskLogs(task)` | `Details` and `StepLogs(step)` | Reports show the log tails of failed steps |
| `Runner.Watch(on func)` | `Watch(ctx, Watcher)`, with `Reconcile` and `Deleted` | A run deleted before it was reported still gets its report concluded |
| Namespaces mapped in `system/config` | `adapters/tekton.Namespaces`, from the settings `system/config` reads | A tenant is the runner's notion |
| `services/lint` renders through the Tekton adapter | Through `lint.Renderer`, which `adapters/tekton.Renderer` implements | Services don't import adapters |
| `adapters/http` decodes through `adapters/github` | Through its `Decoder` and `EventHandler` interfaces, filled by `cmd/octomaton` with the GitHub App and the runs service | Adapters don't import other adapters (except `kube`) or services |

Behaviour changes:

| Change | Effect |
| --- | --- |
| The comment and schedule-slot labels of a run come from its trigger | Re-runs of scheduled runs keep the slot label |
| The runner's checks (namespace, definition) run after the pipelineRun file is read and its params rendered | With several problems, another one may be reported first |
| Log `component` values are `runs`, `reports`, `schedules`, `upkeep` and `runner` | Log queries on `trigger` or `reporter` need the new names |
