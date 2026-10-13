# Octomaton rides out GitHub outages

Octomaton retries every GitHub request that fails for a reason that may pass, and when an event's requests still fail it says so on a failed check, which re-runs the event, and forgets the delivery, so redelivering it runs it again. No event is lost to a GitHub outage without a trace. Linear: ENG-75.

## Context

On 2026-10-03, between 21:46 and 22:03 UTC, GitHub's API answered Octomaton with 504s and timeouts. Octomaton made each request once:

- arikkfir-org/tooling#30's push, open and review request could not read `.octomaton.yaml`. Octomaton then could not open the `octomaton` check that reports that either. The pull request got no `Continuous Integration`, no `Docs` and no `AI Review`, and nothing said why.
- A review request is never reported on a configuration problem, so even a working report path would have dropped it.
- Octomaton had already answered each webhook `202`, so GitHub would not redeliver it.
- Redelivering them by hand did nothing: each replica drops a delivery ID it saw in the last hour, failed or not, and each dropped copy restarted that hour.

## Design

```mermaid
flowchart TD
  E[Webhook event] --> R1["GitHub request<br/>(config, definition, token, check)"]
  R1 -->|fails, may pass| W1[wait 1, 2, 4, 8, 16 s<br/>log a warning]
  W1 --> R1
  R1 -->|ok| Run[Run starts, its check reports it]
  R1 -->|6 attempts failed, or the job's deadline| F["Report the failure on a check<br/>octomaton or the pipeline's"]
  F --> R2[GitHub request, on a context of its own]
  R2 -->|fails, may pass| W2[wait, log a warning]
  W2 --> R2
  R2 -->|ok| C[Failed check: re-run it to try again]
  R2 -->|6 attempts failed| L[Error log]
  C -->|re-run| E2[Event evaluated again] -->|config read| OK[octomaton concluded successfully]
  F --> D[Delivery forgotten]
  D -->|redelivered| E
```

| Part | Behaviour |
| --- | --- |
| Retries | One `http.RoundTripper` under every go-github client of the App, so every call is covered: reads, checks, installation tokens (ghinstallation mints them inside the request). It retries connection errors and timeouts (30 s per attempt), 5xx but 501, 429, and GitHub's secondary rate limit (403 with `Retry-After`). It does not retry 404, 422 or a plain 403. |
| Budget | Up to 6 attempts. Waits double from 1 s to 30 s (31 s in all), or follow `Retry-After` on a 429, a 503 or a secondary rate limit, at most a minute (a primary rate limit's reset can be an hour away). A blip of about half a minute passes unseen; every attempt timing out costs 3.5 minutes. A request stops when its context ends: the webhook job's 5-minute deadline. |
| Logs | Each retry: a warning with method, path, attempt, cause and wait. A request that still fails returns its last response or error, which its caller logs once. |
| Failure report | A configuration that could not be read is Octomaton's failure, not the repository's, so every event reports it: a failed `octomaton` check; the scheduled pipeline's failed check when a schedule fires; a reply when a comment command, which has no check, is declined ("comment again to try again"). Review requests and pull request actions still leave an invalid configuration unreported. A run whose check could not be opened gets a failed check of its pipeline's name. The checks store the trigger, so re-running them tries again. Only the scheduler's periodic read of a repository's schedules just logs: its next read tries again. |
| Report budget | A failure report runs on a context detached from the job's deadline, which may be what failed, bounded at 5 minutes, and goes through the same retries. One that still fails is logged as an error. |
| Recovery | Re-running a failed `octomaton` evaluates the event again; when the configuration reads, `octomaton` is concluded successfully ("Evaluated again"), so the stale failure stops showing. |
| Redelivery | A replica drops a delivery ID it is handling or handled in the last hour. An event that a failed call left partly undone (a configuration, definition, pull request or permission not read; a run not created or cancelled; a report, reaction or reply not posted) is forgotten, panics included, so its redelivery is handled again; every step finds what an earlier copy did. Refused runs, invalid configurations and ignored events are handled. |

## Decisions

| Decision | Why | Rejected |
| --- | --- | --- |
| `hashicorp/go-retryablehttp` | Well known (Terraform, Vault, Consul), and it does what retries need: rewinds request bodies, honours `Retry-After`, stops when the context ends, and plugs in as a `RoundTripper` | `cenkalti/backoff` around each call (one wrapper per call site, and bodies to rewind by hand); retries written in Octomaton |
| Retry in the transport, not per call | One place covers every GitHub call, the check reports included, and new calls get it without thought | Retrying in the services: every call site, and the ports would learn about HTTP |
| Retry POSTs too | Creating a check or a token after a 5xx that GitHub did process leaves a duplicate check run at worst; GitHub shows the newest of a name, which is the one Octomaton tracks | Retrying only GETs: the check reports are exactly what an outage must not lose |
| Report an unreadable configuration on every event | A review request lost to an outage is the case that started this. The red `octomaton` shows only during an outage, and re-running it recovers | Keeping review requests silent; reporting only on events that report invalid configurations |
| Failure reports detached from the job's deadline | The deadline is often what failed; a report on the same context could never be sent | A longer job deadline: still a race between the failure and its report |
| Forget a delivery its handler left undone | A redelivery is the last resort when the failure report fails too, and it must run, not be dropped as a duplicate | Remembering only successes (a copy arriving while the first is handled would run twice); dropping redeliveries for an hour |
| A budget of about half a minute of waits | GitHub's blips are short; a long outage needs the re-runnable check, not workers held for minutes while the queue fills | Retrying for the outage's length (17 minutes on 2026-10-03), which would fill the webhook queue |

## Failure modes

- An outage longer than the budget: the event's failed check says what failed; re-run it once GitHub is back.
- GitHub still down for the report: only the error log is left. Redeliver the webhook from the App's Advanced → Recent deliveries; redeliver until one copy succeeds. A redelivery lands on either replica.
- A delivery redelivered after a successful one is dropped for an hour on the replica that handled it. A copy that lands on the other replica is handled, and finds the runs already there.
- A redelivered re-run (`check_run` re-requested) creates the next attempt of every pipeline it re-runs, those that started the first time included.
- A redelivery that succeeds leaves the failed `octomaton` of the first copy; re-run it to clear it.
- Many events during an outage hold webhook workers for up to their job's deadline. A full queue answers 503, which GitHub records as a failed delivery that can be redelivered.

## Rollout

Ships with arikkfir-org/octomaton's pull request for ENG-75; the merge deploys it. No manual steps.
