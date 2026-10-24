# Comment commands: allow-lists, `/rerun` and middle dots

**Decision**: every comment command names who may invoke it, and that list is the only authority: a `comment` trigger carries a required `allow` (`users`, GitHub logins of people and bots; `teams`, slugs of the owner's teams), and the write-access check that stood in for it is gone. Octomaton gains one built-in command, `/rerun [check...]`, which re-runs a pull request's failed checks, or the named ones, as **Re-run** on the check would, enabled by `commands.rerun.allow` (the organization repository's `organization.commands` applies to every repository without a `commands` section). Before matching, Octomaton drops U+00B7 (`·`) from a comment's first line, so a client whose proxy sprinkles middle dots into slash commands can still invoke them; who may is the allow-list's decision. Linear: [ENG-179](https://linear.app/arikkfir/issue/ENG-179). Contract: [README](https://github.com/arikkfir-org/octomaton#comment-commands).

## Context

Two gaps met. A comment command ran for anyone with write access, so a repository could not let a bot deploy or keep a collaborator out, and the organization's coding agents, which comment as their person, could not re-run a failed check at all: their GitHub proxy refuses the check-run re-request, so the person had to click **Re-run** for them. The same proxy also mangles the slash commands it knows (`/test` arrives as `·/·t·est`), and the owner chose to have Octomaton read through that rather than build a second syntax around it.

## Design

```mermaid
sequenceDiagram
  autonumber
  participant GH as GitHub
  participant A as adapters/github
  participant R as services/runs
  GH->>A: issue_comment (created, on a pull request)
  A->>A: first line, U+00B7 dropped, trimmed; starts with "/"?
  A->>R: CommandEvent{Line, Author, ...}
  R->>GH: the pull request (a fork gets no answer)
  R->>GH: .octomaton.yaml at the default branch (+ the organization's)
  alt /rerun
    R->>R: commands.rerun.allow? allowed? open?
    R->>GH: latest check run per name at the head commit
    R->>R: failed ones, or the named ones
    R->>R: rerunReports: next attempt per pipeline, as a check's Re-run
  else a trigger's pattern
    R->>R: on.comment.allow: user named, or a team member?
    R->>R: start the pipeline at the head commit
  end
  R->>GH: 👀, or 👎 and a reply saying why
```

| Part | Behaviour |
| --- | --- |
| Allow-list | `allow: {users: [...], teams: [...]}`, at least one entry; a login or slug holds no space or slash. Users match case-insensitively. A team is a slug of the repository owner's organization, checked with `GET /orgs/{org}/teams/{team}/memberships/{user}` (`active` only), which needs the App's organization permission Members: read; a team check that fails declines the command and says to see the logs. Users are checked first, so a named user needs no team call |
| Where it lives | Only in `.octomaton.yaml` at the default branch, read with the command's definitions: a pull request cannot widen it. Validation refuses a trigger without one, and a built-in command enabled without one |
| `/rerun` | `pipelines.RerunCommand`; a trigger's `pattern` that matches it is refused, so the built-in command cannot be shadowed. Enabled by `commands.rerun.allow`, the repository's own or else the organization repository's `organization.commands`. Declined when not enabled, when the commenter is not allowed, when the pull request is closed, when no check failed (without arguments), or when a name matches no check; drafts are fine, since a re-run runs what already ran |
| What it re-runs | `Installation.RevisionReports`: the latest check run of each name at the head commit (`filter=latest`, the App's), without their triggers. Without arguments, those concluded `failure`, `timed_out` or `cancelled`; with arguments, the checks the words name: at each word, the longest run of words that equals a check's name or its pipeline's (the part before ` / `), case-insensitively, so `/rerun Continuous Integration docs` names a check with a space and a pipeline; a word that starts no name is declined by name. The selection goes through the same `rerunReports` as a check's **Re-run**: the trigger is read from the check's marker, the pipeline runs again as the next attempt with `RerunBy` the commenter, `octomaton` re-evaluates its event, a report without a matching trigger is skipped |
| Middle dots | `firstLine` in the GitHub adapter removes every U+00B7 from the first line, then trims it; later lines and the stored `.Comment.Arguments` keep whatever the line had after the removal. A deliberate reading-through of one proxy's control, documented as such in the README; the allow-list is what stands between a commenter and a command |
| Answers | As before: 👀 when something started; 👎 and `@author `line` was not run: reason.` otherwise; nothing on a fork's pull request |

## Rollout

The `allow` field is required, so every repository with a `comment` trigger must add one in the same change that deploys this; the hub's repositories had none. A `commands` section is an unknown field to the Octomaton running before this, which refuses the whole file, so a repository enables `/rerun` only once this is deployed: Octomaton's own (`arikkfir`) in a follow-up pull request, and org-wide from the organization repository's `organization.commands`. The App needs Members: read before any `teams` entry works.
