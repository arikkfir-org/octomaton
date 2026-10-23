# octomaton.dev: domain, webhook and Go module

**Goal**: give Octomaton an identity of its own. `octomaton.dev` carries the Go module path and names Octomaton's
labels and configuration API group, independently of any host's domain; the hub also serves the hub's GitHub App's
webhook on it. What Octomaton needs of any App: the README's
[GitHub App](https://github.com/arikkfir-org/octomaton#github-app). The hub's App, the traffic through its gateway and
the Go import page it serves: `delivery`'s [hosting Octomaton](../../delivery/designs/octomaton.md). Migration from
Octomatron: `delivery`'s [rename runbook](../../delivery/runbooks/octomaton-rename.md).

## Go module resolution

```mermaid
sequenceDiagram
  participant U as go install
  participant P as proxy.golang.org
  participant D as octomaton.dev
  participant G as GitHub repository
  U->>P: octomaton.dev/cmd/octomaton@latest
  P->>D: GET /cmd/octomaton?go-get=1
  D-->>P: go-import tag (prefix octomaton.dev)
  P->>D: GET /?go-get=1 (the prefix must agree)
  D-->>P: same tag
  P->>G: git: tags, go.mod says module octomaton.dev
  P-->>U: module zip, checksum recorded in sum.golang.org
```

## Decisions

| Decision | Why | Rejected |
| --- | --- | --- |
| Octomaton's names live under `octomaton.dev`: webhook, Go module, labels `octomaton.dev/…`, API group `octomaton.dev/v1` | Octomaton is a product; `kfirs.com/` names belong to the hub, the way Google's labels use `google.com/` rather than Alphabet's domain | `octomaton.kfirs.com/…` labels, a webhook under `dev.kfirs.com` |
| Module path `octomaton.dev` | Short, owned, and independent of where the repository lives; `go install` works through the `go-import` tag | `github.com/arikkfir-org/octomaton` (imports follow repository renames); `octo` (not installable, and dotless paths are reserved for the standard library) |
| The hub serves the domain | Cloud DNS, cert-manager and the public gateway already exist; everything stays in Terraform and Argo CD ([hosting Octomaton](../../delivery/designs/octomaton.md)) | GitHub Pages |
| One host, two backends | The webhook stays the only path that reaches Octomaton; the rest is a static answer | A separate webhook host |
| Caddy, the hub's one `go-import` for every module domain ([design](../../delivery/designs/go-import-pages.md)) | No code to maintain; one workload answers for every hub module domain | Serving the tag from Octomaton itself; a Caddy of Octomaton's own in its namespace, which it ran until October 2026 |
| A certificate and listener of its own | A problem with the new domain can't block renewal of the `*.kfirs.com` certificate | A SAN on the wildcard certificate |
| GitHub App named `octomaton-dev`, with the repository as its homepage | `Octomaton`, the product's name, is taken on GitHub. Octomaton and the rulesets' required check identify the App by its ID, so the name only labels its checks and comments | `Octomaton` (taken); a homepage at `https://octomaton.dev` (which redirects to the repository anyway) |

## Security and failure modes

- The import page is static and public; it reveals only the repository URL, which is public anyway.
- Only `POST /github/hooks` reaches Octomaton, which verifies every delivery's HMAC signature first.
- If `octomaton.dev` stops resolving (delegation, DNSSEC at the registrar, an expired domain), webhook deliveries
  fail until it recovers (redeliver them from the App's settings) and new `go install`s fail; versions already in the
  Go proxy keep working.
- Whoever holds the domain decides where new module versions come from; keep it renewed. The checksum database pins
  every published version, so tags are never moved.

## Open questions

- A product page at `https://octomaton.dev` would replace the redirect to the repository.
