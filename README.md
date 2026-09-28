# Switchboard

Switchboard replaces GitHub Actions for the `arikkfir-org` organization. A GitHub App sends every webhook to
Switchboard (running in GKE); Switchboard reads the repository's root `.switchboard.yaml`, creates the Tekton
`PipelineRun`s it maps the event to, and reports each run back to GitHub as a check run.

Switchboard is **application-agnostic**: it knows nothing about what a repository builds, its language or layout. The
only repository files it reads are `.switchboard.yaml` and the PipelineRun files that file points to. PipelineRun files
are plain Tekton YAML; Switchboard injects event context only through `params` and an optional GitHub token workspace.

## How it works

```mermaid
sequenceDiagram
  autonumber
  participant GH as GitHub
  participant SB as Switchboard
  participant K8s as Kubernetes / Tekton
  GH->>SB: webhook (push, pull_request, merge_group, issue_comment, check_run, check_suite)
  SB->>SB: verify signature, drop duplicate deliveries, answer 202
  SB->>GH: read .switchboard.yaml (and the PipelineRun file)
  SB->>SB: match events, branches, tags and paths; apply trust rules
  SB->>K8s: create the PipelineRun held (spec.status: PipelineRunPending)
  SB->>GH: create the check run (queued, linked to the Tekton Dashboard)
  SB->>K8s: create the token Secret (optional), then release the run per its concurrency policy
  loop reporter (elected leader only)
    K8s-->>SB: PipelineRun and TaskRun changes
    SB->>GH: check run in_progress, task table, completed with conclusion and failed-step logs
  end
```

1. Every webhook is verified (HMAC-SHA256) before anything else, then processed asynchronously on a bounded worker
   pool. A full queue answers 503, so GitHub records a failed delivery that can be redelivered.
2. The event is matched against each pipeline's triggers. A matching pipeline whose path filters do not match gets a
   check run concluded `skipped` (required checks do not block); a pull request from an untrusted fork gets an
   `action_required` check with an **Approve and run** button.
3. Runs are named `<repo>-<pipeline>-<sha7>-<attempt>`. A redelivery, or a second event for the same commit, pipeline and
   branch, finds the existing run; re-runs create the next attempt.
4. Runs are created held, then their check run and token Secret are created, then they are released per their
   concurrency group. If anything fails in between, the run is cancelled and its check says why. A run held longer than
   five minutes is resumed by the leader (which also serves as the poll for queued runs).
5. The elected leader watches the runs and keeps their checks up to date, refreshes GitHub tokens of long runs, fires
   cron schedules and deletes PVCs of finished runs.

## Repository configuration (`.switchboard.yaml`)

The only file Switchboard reads from a repository, always at the root. It is parsed as YAML 1.2, so the `on` key needs
no quoting, and strictly: unknown fields, duplicate keys and type mismatches are errors.

```yaml
apiVersion: switchboard.kfirs.com/v1
pipelines:
  - name: ci                           # check-run name; unique; [a-z0-9][a-z0-9-]*
    pipelineRun: .tekton/ci.yaml       # repository-relative file holding exactly one tekton.dev/v1 PipelineRun
    on:
      pull_request:
        branches: [main]               # base-branch globs; omitted = all
        types: [opened, reopened, synchronize, ready_for_review]  # default
        drafts: true                   # default; false skips draft pull requests
        paths: ["**"]                  # optional globs; no match = check reported as skipped
        pathsIgnore: []                # optional
      merge_group:                     # merge queue; optional base-branch globs
        branches: [main]
      push:
        branches: [main]               # branch globs
        tags: ["v*"]                   # tag globs
        paths: []                      # optional
      comment:                         # pull request comment command, e.g. "/deploy staging"
        pattern: "^/deploy\\b"         # regexp on the comment's first line; commenter needs write access
        branches: [main]               # optional pull request base-branch globs
      schedule:                        # cron, 5 fields, UTC; runs at the default branch head
        - cron: "0 3 * * *"
    params:                            # set/override PipelineRun spec.params; values are Go templates
      repo-url: "{{ .Repository.CloneURL }}"
      revision: "{{ .Revision }}"
    githubToken:                       # optional installation token for this repository, refreshed while the run lives
      workspace: github-token          # bound as a Secret workspace (key: token)
      permissions: {contents: read}    # default
    timeout: 1h                        # optional, sets spec.timeouts.pipeline
    concurrency:                       # optional; default for pull_request: group "pr-<number>", policy supersede
      group: "publish"                 # Go template, scoped to the repository
      policy: latest                   # supersede | queue | latest
    taskChecks: false                  # optional: also report each pipeline task as "<name> / <task>"
```

### Triggers

| Trigger | Matches | Notes |
| --- | --- | --- |
| `pull_request` | the listed actions (`types`) on pull requests whose base branch matches `branches` | Default types: `opened`, `reopened`, `synchronize`, `ready_for_review`. `drafts: false` skips draft pull requests. |
| `merge_group` | `checks_requested` for merge groups whose base branch matches `branches` | Also accepts `paths`/`pathsIgnore`. A destroyed merge group cancels its runs. |
| `push` | pushes of branches matching `branches` and tags matching `tags` | As in GitHub Actions: with neither set every push matches; with only `branches`, tag pushes are ignored; with only `tags`, branch pushes are ignored. Deleted refs and merge queue branches never match. |
| `comment` | a pull request comment whose first line matches `pattern` (which must start with `^/`) | Only new comments on open, non-draft pull requests into `branches`, by users with write access. |
| `schedule` | each `cron` slot (5 fields, UTC) | Runs at the head of the default branch, once per slot, up to 10 minutes late. |

Globs use [doublestar](https://github.com/bmatcuk/doublestar) syntax: `*` stays within a path segment, `**` crosses
segments. **Path filters** (`paths`, `pathsIgnore`) apply to the files the event changes (pull request files; the
comparison `before...after` for pushes; `base...head` for merge groups). A file is relevant when it matches `paths`
(when set) and does not match `pathsIgnore`. With no relevant file the pipeline's check is concluded `skipped`. When the
changed files cannot be determined (a new branch or tag, an API error, more files than GitHub lists), the filters are
ignored and the pipeline runs.

**Where definitions are read:** pull requests, merge groups and pushes read `.switchboard.yaml` and the PipelineRun file
at the commit under test; comment commands and schedules read them from the default branch (comment commands still run
against the pull request's head commit, so a pull request cannot change what its own commands run).

### Pipeline settings

| Field | Meaning |
| --- | --- |
| `name` | Check-run name, unique, `[a-z0-9][a-z0-9-]*`, at most 63 characters. `switchboard` is reserved. |
| `pipelineRun` | Repository-relative path of a file holding exactly one `tekton.dev/v1` `PipelineRun`. Its `metadata.name`/`generateName` are replaced; a `metadata.namespace` other than the repository's namespace is refused. |
| `params` | Sets `spec.params` entries by name (existing entries are overridden, new ones appended). Values are Go `text/template`s over the context below; a missing value fails the check with the rendering error. |
| `githubToken` | Mints an installation token restricted to this repository with `permissions` (default `contents: read`), stores it in Secret `<run>-github-token` (key `token`, owned by the run) and binds it to `workspace`. The token is refreshed while the run lives; read it from the file each time you need it. |
| `timeout` | Go duration, sets `spec.timeouts.pipeline`. |
| `concurrency` | Limits runs sharing a group (below). |
| `taskChecks` | Also reports every task of the PipelineRun's own `spec.pipelineSpec` as a check named `<name> / <task>`. |

**Concurrency.** Groups are rendered from the `group` template and scoped to the repository, so two pipelines naming
the same group share it (include `{{ .Pipeline }}` to keep them apart).

| Policy | Behaviour |
| --- | --- |
| `supersede` | The newest commit wins: older live runs in the group are cancelled and their checks concluded `skipped` ("Superseded"). A run whose commit is no longer the head of its branch or pull request stands itself down. |
| `queue` (default) | One run at a time, oldest first. |
| `latest` | One run at a time; only the newest waiting run survives, older waiting runs are cancelled. |

Without `concurrency`, pull request runs of the same pipeline and pull request share the group `pr-<number>` (per
pipeline) with policy `supersede`; other runs are unconstrained.

### Template context

`.Event` (`push`, `pull_request`, `merge_group`, `comment`, `schedule`), `.Action`, `.Repository` (`Owner`, `Name`,
`FullName`, `CloneURL`, `HTMLURL`, `DefaultBranch`, `Private`), `.Revision` (SHA under test), `.Ref`, `.Branch`, `.Tag`,
`.Sender`, `.Pipeline`, `.Push` (`Before`, `After`), `.PullRequest` (`Number`, `HeadRef`, `HeadSHA`, `BaseRef`,
`BaseSHA`), `.MergeGroup` (`HeadRef`, `HeadSHA`, `BaseRef`, `BaseSHA`), `.Comment` (`ID`, `Author`, `Command`,
`Arguments`), `.Schedule` (`Cron`, `Slot`). Event-specific objects are nil for other events; referencing a missing
value fails the check with the rendering error. Guard optional objects with `{{ if .PullRequest }}…{{ end }}`.

| Event | `.Revision` | `.Ref` | `.Branch` | Objects set |
| --- | --- | --- | --- | --- |
| `push` | pushed commit (the tagged commit for annotated tags) | `refs/heads/<b>` or `refs/tags/<t>` | pushed branch (`.Tag` for tags) | `.Push` |
| `pull_request` | head commit | `refs/pull/<n>/head` | head branch | `.PullRequest` |
| `merge_group` | merge group head | merge group ref | merge group branch | `.MergeGroup` (`HeadRef`/`BaseRef` are full refs) |
| `comment` | pull request head commit | `refs/pull/<n>/head` | head branch | `.PullRequest`, `.Comment` (`Command` is the first word, `Arguments` the rest of the first line) |
| `schedule` | head of the default branch | `refs/heads/<default>` | default branch | `.Schedule` (`Slot` is RFC 3339, UTC) |

Values such as `.Comment.Arguments`, branch names and `.Sender` come from users: pass params to scripts through
environment variables, never by interpolating `$(params.…)` into a script.

### Examples

```yaml
apiVersion: switchboard.kfirs.com/v1
pipelines:
  - name: ci                                 # required check on pull requests and in the merge queue
    pipelineRun: .tekton/ci.yaml
    on:
      pull_request: {branches: [main], pathsIgnore: ["docs/**", "**/*.md"]}
      merge_group: {}
    params:
      repo-url: "{{ .Repository.CloneURL }}"
      revision: "{{ .Revision }}"
  - name: publish                            # one publication at a time, only the newest waiting one
    pipelineRun: .tekton/publish.yaml
    on:
      push: {branches: [main]}
    params:
      revision: "{{ .Revision }}"
    githubToken: {workspace: github-token, permissions: {contents: read}}
    concurrency: {group: "publish", policy: latest}
  - name: deploy                             # "/deploy staging" on a pull request into main
    pipelineRun: .tekton/deploy.yaml
    on:
      comment: {pattern: "^/deploy\\b", branches: [main]}
    params:
      revision: "{{ .Revision }}"
      environment: "{{ .Comment.Arguments }}"
  - name: nightly
    pipelineRun: .tekton/nightly.yaml
    on:
      schedule: [{cron: "0 3 * * *"}]
    params:
      slot: "{{ .Schedule.Slot }}"
```

Validate a configuration and the PipelineRun files it references with `switchboard lint [--render] PATH...` (PATH is
the file or its directory); it renders every param for every triggering event with placeholder values, so a template
that only works for one event is reported. Exit code 0 means clean, 1 problems, 2 usage.

## Check runs

- Each run reports on a check named after its pipeline (`queued` → `in_progress` → `completed`), linked to
  `https://tekton.kfirs.com/#/namespaces/<namespace>/pipelineruns/<name>`, with `external_id` `<namespace>/<name>`.
- While a run of two or more tasks runs, the title reads `<done> of <n> · <running task> · <elapsed>` and the summary
  holds a task table (✅ ❌ ⏳ ⬜); it is rewritten only when the table changes.
- Conclusions: `Succeeded=True` → `success`; superseded → `skipped` ("Superseded"); cancelled or stopped → `cancelled`;
  `PipelineRunTimeout` → `timed_out`; any other failure → `failure`, with the last 50 lines of each failed step's log.
- A pipeline or task result named `check-title` or `check-summary` replaces the check's title or Markdown summary.
- Problems that prevent a run (an invalid `.switchboard.yaml`, a missing namespace or file, a template error, a
  refused Secret) are reported as failed checks: `switchboard` for configuration errors, the pipeline's name otherwise.
- Every check stores its trigger context in a hidden marker in its output, so **Re-run** works even after the
  PipelineRun was pruned. Re-running a pipeline's check always runs it (path filters are not re-applied; the requester
  needs write access); re-running `switchboard` re-evaluates the whole event.
- Comment commands get a 👀 reaction when started, a 👎 and a reply when declined, and a reply with the result when done.

## Security

- Only webhooks with a valid signature are processed; only installations on `github.allowedOwners` are served.
- Pull requests run automatically when their author is an owner, member or collaborator, or their branch is in the
  repository itself. Other pull requests need **Approve and run** (or a re-run) from someone with write access, for
  every new commit.
- A run may mount no Secret other than the token Switchboard binds (volumes, workspaces, projected sources, `env` and
  `envFrom` are checked). Tokens are scoped to the event's repository with least permissions.
- Namespaces, not pipelines, are the isolation boundary: trusted pull requests can change their own pipeline files and
  thereby use their namespace's service account.

## Server configuration

Read from `/etc/switchboard/config.yaml` (flag `--config`, env `SWITCHBOARD_CONFIG`); unknown fields are errors and the
process exits with every problem listed.

```yaml
github:
  appIDFile: /etc/switchboard/github/app-id
  privateKeyFile: /etc/switchboard/github/private-key
  webhookSecretFile: /etc/switchboard/github/webhook-secret
  allowedOwners: [arikkfir-org]        # installations on other owners are ignored
tekton:
  dashboardURL: https://tekton.kfirs.com
namespaces:
  template: "ci-{{ .Repository.Name }}" # rendered, then sanitized to a DNS label
  overrides:
    arikkfir-org/.github: ci-github
relay:                                  # verified push and pull_request deliveries are forwarded here
  urls: [http://argocd-server.argocd.svc.cluster.local/api/webhook]
retention:
  freePVCsAfter: 1h                     # PVCs of finished runs are deleted after this; runs and pods stay
```

- `namespaces.template` is rendered over `.Repository`, then sanitized: lowercased, leading dots stripped, every run of
  characters outside `[a-z0-9-]` replaced by `-`, leading and trailing `-` trimmed, cut to 63 characters. Overrides
  (`owner/name`, case-insensitive) win. A namespace that does not exist fails the check with "repository not onboarded".
- `relay.urls` receive the original body and GitHub headers (signatures included), asynchronously, with a 10 s timeout.

| Flag / environment | Default | Meaning |
| --- | --- | --- |
| `--config`, `SWITCHBOARD_CONFIG` | `/etc/switchboard/config.yaml` | server configuration |
| `--listen` | `:8080` | address of `/webhook`, `/healthz`, `/readyz`, `/metrics` |
| `--workers`, `--queue-size` | `8`, `256` | webhook worker pool |
| `SWITCHBOARD_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` (JSON logs on stdout) |
| `POD_NAMESPACE`, `POD_NAME` | service account namespace, hostname | Lease `switchboard` namespace and holder identity |
| `KUBECONFIG` | in-cluster config | used when not running in a cluster |

## GitHub App

`arikkfir-switchboard`, installed on all `arikkfir-org` repositories, webhook URL
`https://switchboard.kfirs.com/webhook`.

- Repository permissions: Checks: read and write; Contents: read; Metadata: read; Pull requests: read and write; Merge
  queues: read.
- Events: `push`, `pull_request`, `issue_comment`, `check_suite`, `check_run`, `merge_group`.

## Deployment

Switchboard is deployed by Argo CD from [`arikkfir-org/delivery`](https://github.com/arikkfir-org/delivery): namespace
`switchboard`, Deployment/ServiceAccount/Service `switchboard` (Service port 80 → container 8080), ConfigMap
`switchboard` (key `config.yaml`) at `/etc/switchboard/config.yaml`, Secret `switchboard-github` (keys `app-id`,
`private-key`, `webhook-secret`) at `/etc/switchboard/github/`. Every replica serves webhooks; the replica holding the
Lease `switchboard` in its namespace runs the reporter, scheduler, token refresher and PVC retention.

Endpoints (all on 8080): `POST /webhook`; `GET /healthz` (process up); `GET /readyz` (Kubernetes API reachable and, on
the leader, the PipelineRun informer synced); `GET /metrics`.

**RBAC Switchboard needs:**

| Scope | Permissions |
| --- | --- |
| Tenant namespaces (`ClusterRole switchboard-tenant`, RoleBinding `switchboard` in each `ci-<repository>`) | PipelineRuns: create, get, list, watch, patch, update, delete; TaskRuns: get, list, watch; Secrets: create, get, patch, update, delete; Pods: get, list; `pods/log`: get; PersistentVolumeClaims: get, list, delete |
| Cluster | PipelineRuns: get, list, watch (the reporter's informer, token refresh and retention); Namespaces: get (optional; without it a missing namespace surfaces as the creation error) |
| `switchboard` namespace | Leases: get, create, update |

**Metrics:** `switchboard_webhooks_received_total{event}`, `switchboard_webhooks_rejected_total{event,reason}`,
`switchboard_runs_created_total{result}` (`created`, `existing`, `skipped`, `action_required`, `failed`, `error`),
`switchboard_github_checkrun_errors_total{operation}`, `switchboard_reconcile_duration_seconds{result}`,
`switchboard_webhook_queue_depth`, `switchboard_leader`, plus Go and process collectors.

**Bookkeeping:** objects Switchboard creates carry the label `app.kubernetes.io/managed-by: switchboard` and labels and
annotations under `switchboard.kfirs.com/` (`pipeline`, `event`, `repository-id`, `sha`, `concurrency-group`, `done`,
`repository`, `check-run-id`, `installation-id`, `delivery-id`, `context`, `reported`, …). They are never read as
configuration.

## Development

Requirements: Go 1.27, and [ko](https://ko.build) for images.

```bash
make test      # go vet ./... && go test -race ./...
make lint      # switchboard lint . (this repository's own .switchboard.yaml)
make build     # bin/switchboard
```

Run locally against a cluster (the current `KUBECONFIG` context) with a configuration pointing at local copies of the
App ID, private key and webhook secret:

```bash
go run ./cmd/switchboard --config ./config.local.yaml --listen :8080
```

Tests use an in-process fake of the GitHub API (`internal/githubapp/githubtest`) and client-go's fake clients;
`internal/e2e` drives signed webhooks through the whole service.

Layout: `cmd/switchboard` (serve, lint, version); `internal/config` (server configuration, namespaces),
`internal/repoconfig` (`.switchboard.yaml` schema and matching), `internal/tmpl` (template context),
`internal/githubapp` (App auth and GitHub API), `internal/webhook` (signatures, dedupe, worker pool),
`internal/trigger` (events → held runs, concurrency, re-runs, comments, schedules, maintenance), `internal/tekton`
(PipelineRun rendering and client), `internal/reporter` (check-run reporting), `internal/checkrun` (trigger context
marker), `internal/relay`, `internal/lint`, `internal/metrics`.

## Releases

Switchboard builds itself: `.switchboard.yaml` runs `ci` (lint, vet, race tests, build) on pull requests and in the
merge queue, and `release` on pushes to `main` (image tags `sha-<short>` and `main`) and `v*` tags (the tag name). Images
go to `me-west1-docker.pkg.dev/arikkfir/images/switchboard`.

The first image has to come from a workstation, before Switchboard runs:

```bash
gcloud auth configure-docker me-west1-docker.pkg.dev
git tag v0.1.0 && git push origin v0.1.0
KO_DOCKER_REPO=me-west1-docker.pkg.dev/arikkfir/images/switchboard ko build --bare --tags=v0.1.0 ./cmd/switchboard
```

(`make image` does the same with the version from `git describe`.)
