# Versioned releases

**Decision**: besides the release of every commit on `main`, which `release` publishes tagged with the commit's short
SHA, Octomaton has versions: a [GitHub release](https://github.com/arikkfir-org/octomaton/releases) with tag
`v<version>` on a commit of `main`, titled `<version>`, whose notes list the pull requests since the previous version
by kind, with their authors, and link the [installation guide](../installation.md) at that tag. Publishing the release
pushes the tag; the same `release` pipeline, triggered by the tag, publishes the images tagged with the version, which
the binaries then report. A draft release has no tag, so it builds nothing. `ci-octomaton-release` admits version tags
through a second annotation, `octomaton.dev/tags`, which Octomaton checks as it checks `octomaton.dev/branches`. The
hub's `octomaton-release` skill, in [`tooling`](https://github.com/arikkfir-org/tooling), computes the version and
drafts the notes. Linear: [ENG-181](https://linear.app/arikkfir/issue/ENG-181).

## Context

Octomaton became a product other clusters host ([host-agnostic Octomaton](host-agnostic.md)). A host that follows
`main` gets every commit; a host that pins wants a name to pin, a list of what changed since the name it runs, and a
page that says how to install and upgrade. Until now the only names were short SHAs, the list was `git log`, and the
installation guide was the README's contract table, which says what a host provides but not in which order or with
which commands. The reviewer image's `main` tag and the hub's own Argo CD Applications keep following `main`: nothing
here changes how the hub deploys Octomaton.

## Design

```mermaid
sequenceDiagram
  autonumber
  participant S as octomaton-release skill (tooling)
  participant GH as GitHub
  participant O as Octomaton (the hub's)
  participant T as Tekton: release
  participant AR as Artifact Registry
  S->>GH: pull requests merged since the last v* tag, their commits
  S->>S: version from the commit types (major, minor, patch); notes from the template
  S->>GH: draft release: tag v1.2.3 on main, title 1.2.3, notes
  Note over GH: a draft has no tag: nothing happens
  GH->>GH: the person publishes the release: tag v1.2.3 is created
  GH->>O: push refs/tags/v1.2.3
  O->>O: .octomaton.yaml and release.yaml read at main; release matches on.push.tags v*; ci-octomaton-release allows the tag (octomaton.dev/tags: v*)
  O->>T: PipelineRun release (tag=v1.2.3)
  T->>T: the tagged commit is on main, or fail
  T->>AR: octomaton:v1.2.3, octomaton-logs:v1.2.3 (VERSION=v1.2.3)
```

| Piece | Where | What |
| --- | --- | --- |
| Trigger | `.octomaton.yaml`, pipeline `release` | `on.push` gains `tags: ["v*"]` beside `branches: [main]`; param `tag` is `{{ .Tag }}`, empty for a push to `main`. One pipeline, so a version is built exactly as a commit is |
| Build | `.tekton/release.yaml` | With a tag: refuse anything but `vX.Y.Z` (ko takes a comma-separated tag list, and a Git tag may hold a comma, so `v1.2.3,latest` would add a tag), fetch the commit and `main`, refuse a commit that is not an ancestor of `main`, and tag both images with the tag alone, as `VERSION`. Without: the short SHA alone; the floating `main` tag is gone, since a host pins a commit or a version |
| Identity | `delivery`, `ci-octomaton-release` | `octomaton.dev/tags: v*` beside `octomaton.dev/branches: main` ([CI tenants](../../delivery/platform.md#ci-tenants)) |
| Definitions | `adapters/github/events.go` | A tag push reads `.octomaton.yaml` and the PipelineRun at the default branch, like a comment command or a schedule, and runs the tagged commit. So a tag runs only reviewed definitions, in every repository |
| Check | `adapters/tekton/serviceaccounts.go` | A ServiceAccount with either annotation is restricted: a branch's run must match a branch glob, a tag push a tag glob. Branches alone still refuse every tag push, as before; tags alone refuse every branch's run |
| Release | GitHub releases | Tag `v<version>`, title `<version>`, notes from the skill's template: a section per kind of change with an emoji, each pull request as its title, link and `@author`, the compare link to the previous version, the images, and the installation guide at the tag |
| Guide | [`installation.md`](../installation.md) | From nothing to a first check: the App, the Secret and ConfigMap, `deploy/` at a version, the route, tenants, a first pipeline, the step logs server, upgrading |
| Skill | `tooling`, `claude/skills/octomaton-release/` | Finds the previous version, lists the pull requests since, picks the version from their commits, renders the notes, creates the draft, and tells the person what to publish |

## Decisions

| Decision | Why | Rejected |
| --- | --- | --- |
| The tag push triggers the build, not the release event | Octomaton has `push` with `tags`; GitHub creates a release's tag only when the release is published, so a draft builds nothing by construction, and a tag pushed by hand releases too | A `release` trigger: a new event, a new permission on the App, and a draft that has to be filtered out |
| The version tag is the image tag and the reported version, `v1.2.3` | One string names the release in Git, in the registry and in the binaries' telemetry; `v` tells a version from a short SHA at a glance | `1.2.3` as the image tag: a second spelling of the same release |
| `release` refuses a tag whose commit is not on `main` | Pushing a tag takes write access, merging into `main` a review; a version must be the latter's. The check costs a fetch of `main` | A tag ruleset that limits who creates `v*` tags: it would still let a maintainer release an unreviewed commit |
| A tag push reads its definitions at the default branch | The ancestry check lives in `release.yaml`; read at the tagged commit, a tag could carry a `release.yaml` without it, or an `.octomaton.yaml` that leaves `tag` empty, and still receive `ci-octomaton-release`. Read at `main`, the pipeline a tag runs is always a reviewed one, in every repository Octomaton serves | The check in Octomaton itself (ancestry of the default branch before creating a tag's run): a policy about one repository's releases in an application-agnostic server |
| A second annotation, not a tag syntax inside `octomaton.dev/branches` | Branch and tag globs are different namespaces (`on.push` keeps them apart too), and a ServiceAccount restricted to `main` must keep refusing every tag push, which a shared annotation could not promise | `octomaton.dev/branches: main, refs/tags/v*` |
| The notes generator and template live in the skill, not in this repository | The format (emojis, mentions, the hub's conventional commits) is the hub's convention; the product is the tag, the images and the guide, which the notes only point at | A release-notes command in `cmd/`, a `.github/release.yml`, which categorises by label, not by commit type |
| A draft first, published by a person | Publishing is the step that builds and cannot be undone; the person reads the notes and the version before it | The skill publishes at once |

## Security and failure modes

- A version is released by whoever can push a `v*` tag, which is anyone with write access, and only for a commit
  of `main`: the build, whose definition comes from `main` whatever the tag names, refuses any other, and
  `ci-octomaton-release` refuses any ref but `main` and `v*`.
- A tag push in any repository now runs the default branch's definitions, not the tagged commit's; a repository
  whose tags carried their own `.octomaton.yaml` sees the default branch's instead.
- A tag pushed twice at a different commit (deleted and recreated) rebuilds the version's images over the old ones:
  the registry keeps the digests, but the tag moves. Don't; cut the next version instead.
- A failed build leaves the GitHub release published with no images. Re-run the check from GitHub (the pipeline runs
  again at the same tag), or fix `main` and release the next version.
- The skill can only draft. The GitHub release's author is whoever runs the skill, through their `gh` login.

## Rollout

1. This pull request: the check, the pipeline, the guide and this page (`octomaton`).
2. `delivery`: `octomaton.dev/tags: v*` on `ci-octomaton-release`. Until it merges, a `v*` tag's run is refused,
   and nothing is built.
3. `tooling`: the skill, the notes template and the bundle that ships them.
4. The first version is cut by hand with the skill, as a draft, read, then published.
