# Phase 2: Octomaton

**Goal**: replace GitHub Actions. A GitHub App delivers webhooks to Octomaton in GKE; Octomaton runs the Tekton
`PipelineRun`s that the repository's root `.octomaton.yaml` maps to those events and reports each one as a GitHub
check run. Code: [arikkfir-org/octomaton](https://github.com/arikkfir-org/octomaton). Names and wiring:
[reference](../../hub/reference.md#octomaton). Domain, webhook host and Go module path: [octomaton.dev](octomaton-dev.md).

## Principles

1. **Application-agnostic.** Octomaton knows nothing about what a repository builds, its language, layout, CI or CD.
2. **One file.** The only repository file Octomaton reads is the root `.octomaton.yaml`, plus the `PipelineRun`
   files it points to. There are no conventional directories and no configuration in Tekton labels or annotations.
3. **Plain Tekton.** `PipelineRun` files are ordinary Tekton YAML. Octomaton injects event context only through
   `params` (Go templates over a documented context) and an optional token workspace.

Octomaton is derived from an earlier, application-aware implementation; these principles are what changed.

## Flow

```mermaid
sequenceDiagram
  autonumber
  participant GH as GitHub
  participant SB as Octomaton
  participant K8s as Kubernetes / Tekton
  GH->>SB: webhook (push, pull_request, merge_group, check_run, check_suite)
  SB->>SB: verify HMAC signature, dedupe delivery, 202
  SB->>GH: read .octomaton.yaml at the commit under test
  SB->>SB: ignore forks, then match events, branches, tags and paths
  alt paths don't match
    SB->>GH: check run "completed / skipped"
  else run
    SB->>GH: read the PipelineRun file at the same commit
    SB->>K8s: create the PipelineRun held (PipelineRunPending) in the repository's ci- namespace
    SB->>GH: check run "queued" (links to Tekton Dashboard)
    SB->>K8s: token Secret owned by the run (optional)
    SB->>K8s: release the run per its concurrency policy
    loop reconciler (leader only)
      K8s-->>SB: PipelineRun status changes
      SB->>GH: check run in_progress / completed + TaskRun table + failed-step log tail
    end
  end
```

## Configuration

```yaml
apiVersion: octomaton.dev/v1
pipelines:
  - name: ci                          # the pipeline's ID
    displayName: Continuous Integration # the check's name on GitHub
    pipelineRun: .tekton/ci.yaml      # any path; Octomaton assumes no layout
    on:
      pull_request: {branches: [main]}
      merge_group: {}
      push: {branches: [main], tags: ["v*"], paths: ["src/**"]}
    params:
      repo-url: "{{ .Repository.CloneURL }}"
      revision: "{{ .Revision }}"
    githubToken: {workspace: github-token}
    timeout: 1h
    concurrency: {group: "pr-{{ .PullRequest.Number }}", policy: supersede}
```

The full schema and template context are in the [reference](../../hub/reference.md#repository-configuration-octomatonyaml)
and the Octomaton README.

## Behaviour

| Situation | Behaviour |
| --- | --- |
| No `.octomaton.yaml` | Nothing happens |
| Invalid `.octomaton.yaml` | One failed check run named `octomaton` explaining the error |
| Event matches, paths don't | Check run reported as `skipped`, which satisfies required checks |
| Changed files can't be determined | Treated as matching (runs rather than silently skipping) |
| Pull request from a fork (whoever opened it), or any event from a repository that is a fork | Ignored: no check run and no run; comment commands on it get no reaction or reply, and its stored reports are never re-run |
| Repository namespace missing | Failed check: "repository not onboarded" |
| Newer commit on the same pull request or branch | Concurrency groups decide: `supersede` cancels older runs (the default for pull requests), `queue` runs one at a time, `latest` keeps only the newest waiting run |
| `/command` comment on a pull request | Pipelines with a matching `on.comment.pattern` run (definitions from the default branch, code from the pull request); the commenter needs write access; 👀 when started, a reply with the result when done |
| Cron schedule | Pipelines with `on.schedule` run at the default branch head, once per slot |
| Long run | The installation token is refreshed while the run lives; a live task table and per-task checks (`taskChecks`) show progress |
| Octomaton restarts mid-dispatch | Runs are created held (`PipelineRunPending`) and released only once their check and token exist; held runs are resumed |
| Merge group destroyed | Its runs are cancelled |
| "Re-run" in GitHub | The pipeline re-runs with the original context, stored in the check run itself, so it works after pruning |

## Tenancy and identity

```mermaid
flowchart LR
  SB[octomaton/octomaton] -- creates PipelineRuns, token Secrets --> NS1[ci-docs]
  SB --> NS2[ci-octomaton]
  SB --> NS3[ci-...]
  NS1 -- KSA docs-publisher, main only --> B1[(arikkfir-docs: object user on its layer)]
  NS2 -- KSA ci-octomaton-release, main only --> B2[(Artifact Registry images: writer)]
```

- Each repository runs in its own namespace, `ci-<repository>`, created in `delivery` (onboarding a repository is a
  small change there, plus IAM in `infra` if it needs cloud access).
- Runs use the namespace's `pipeline` service account unless they name another. GCP permissions attach only to ServiceAccounts a pipeline names, never to `pipeline`, through Workload Identity, so one repository's pipelines can never use another's permissions ([CI ServiceAccounts](../../hub/designs/ci-service-accounts.md)).
- Octomaton's own identity can create `PipelineRun`s and token Secrets only in tenant namespaces
  (`ClusterRole octomaton-tenant`, bound per namespace), and watch runs cluster-wide.
- Tekton's default pod template schedules runs onto the `ci` node pool.

## Process

`cmd/octomaton` is a launcher: it starts the parts in dependency order and stops them in reverse; the logic lives in
the packages under `internal/`. Every replica serves webhooks; only the leader writes to GitHub from the reporter.

```mermaid
flowchart TD
  SIG["signals<br/>SIGTERM, SIGINT"] --> TEL["telemetry<br/>Google Cloud on GKE, local elsewhere"]
  TEL --> CFG["configuration<br/>OCTOMATON_* variables"]
  CFG --> CLI["clients<br/>Kubernetes, GitHub App"]
  CLI --> HTTP["HTTP server<br/>/github/hooks, /healthz, /readyz"]
  CLI --> ELEC["Lease election"]
  HTTP --> POOL["webhook workers"]
  ELEC -->|leader| JOBS["reporter, scheduler,<br/>token refresh, PVC retention"]
```

| On SIGTERM, in order | Bound |
| --- | --- |
| `/readyz` fails; the HTTP server stops accepting connections and finishes the requests in flight. Meanwhile the leader's jobs stop and it releases the Lease | 10 s |
| Queued deliveries are processed and relayed | 15 s |
| Telemetry exporters flush | 5 s |

Telemetry follows where the process runs. On GKE (a pod with a GCP metadata server), everything goes to Google Cloud,
linked by trace ID; anywhere else, logs are text and nothing is exported. Variables and grants:
[reference](../../hub/reference.md#server-configuration).

```mermaid
flowchart LR
  OCT["Octomaton pod<br/>KSA octomaton/octomaton"] -->|"stdout, JSON"| AGENT["GKE logging agent"]
  AGENT --> CL["Cloud Logging"]
  OCT -->|"OTLP over gRPC<br/>Workload Identity"| TAPI["telemetry.googleapis.com"]
  TAPI --> CM["Cloud Monitoring<br/>octomaton.* metrics"]
  TAPI --> CT["Cloud Trace<br/>webhook spans"]
  CL -.->|"logging.googleapis.com/trace"| CT
```

## Decisions

| Decision | Why | Rejected |
| --- | --- | --- |
| Check runs (Checks API) rather than commit statuses | Rich output, re-run buttons, required-check integration | Commit statuses |
| A check is named by its pipeline's `displayName` (`Continuous Integration`); `name` (`ci`) stays the pipeline's ID | GitHub shows the check name, and a two-letter link is hard to click; run names, labels and concurrency groups keep the short ID | Renaming the pipelines (spaces are not valid in run names or labels) |
| Ignore forks entirely, even members' | The repositories are public: anyone can fork one and open a pull request, and its code must never run in the cluster, nor be one click away from it | An "Approve and run" button for fork pull requests; trusting forks of owners, members and collaborators |
| Params and a token workspace instead of templating inside PipelineRun files | Files stay valid Tekton; no templating collisions with scripts | Pipelines-as-Code style `{{ }}` substitution in YAML |
| Runs are created held, then released | The check run and token Secret exist before anything executes; a restart mid-dispatch is resumed; concurrency queues need held runs anyway | Creating Secrets first, `generateName` |
| Deterministic run names with attempts (`<repo>-<pipeline>-<sha7>-<n>`) | Redeliveries and duplicate events find the existing run; re-runs are new attempts | Random names (duplicates on redelivery) |
| Skipped checks for path-filtered pipelines | Required checks can't deadlock on unrelated changes | Not reporting (blocks merges) |
| Namespace per repository | Isolation of credentials and permissions | Shared CI namespace |
| Unstructured objects + dynamic client | Avoids the heavy Tekton Go module; only a few status fields are read | Tekton typed clients |
| Leader election for the reconciler only | Any replica can take webhooks; one writer updates check runs | Single replica without election |
| Configuration from environment variables only (`envconfig`) | One mechanism for settings and secrets: the ConfigMap and the Secret map straight to variables, nothing is mounted, and every problem is reported at startup | A YAML file with mounted secret files; flags |
| Telemetry to Google Cloud on GKE: logs through stdout to Cloud Logging, metrics and traces over OTLP to the Telemetry API; nothing exported elsewhere | One place for logs, metrics and traces, linked by trace ID. OTLP is Google's recommended path (its own Cloud Monitoring and Cloud Trace exporters are deprecated) and needs no collector | A Prometheus endpoint; an OpenTelemetry Collector; Google's deprecated exporters |
| The linter is its own command, `octomaton-lint` | The server takes no arguments; the linter is what people install | Subcommands of one binary |

## Security

- Webhooks are verified (HMAC-SHA256) before anything else; the webhook route is the only public path.
- Forks are never acted on: pull requests from them, comment commands on those, re-runs of their reports and every
  event from a repository that is a fork are ignored, without a check run or a reply.
- Installation tokens are minted per run with least permissions (contents read), expire within an hour and are
  garbage-collected with the run.
- Pull requests can change their own pipeline files, so they can name any ServiceAccount in their namespace. Namespaces
  separate repositories, and within a namespace a ServiceAccount annotated `octomaton.dev/branches` is refused to every
  other branch ([CI ServiceAccounts](../../hub/designs/ci-service-accounts.md)).

## Rollout

1. Create the GitHub App (permissions and events in the [reference](../../hub/reference.md#octomaton)); store its ID, key
   and webhook secret in Secret Manager.
2. Build the first image locally with `ko` (see the [bootstrap runbook](../../hub/runbooks/bootstrap.md)); afterwards
   Octomaton builds itself: every push to `main` publishes an image tagged with the commit's short SHA.
3. Argo CD deploys every commit on `main` with its own image ([deployment](deployment.md)).
4. Apply the GitHub rulesets once `Continuous Integration` checks appear on pull requests.
