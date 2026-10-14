# octomaton.dev: domain, webhook and Go module

**Goal**: give Octomaton an identity of its own. `octomaton.dev` carries the GitHub App's webhook and the Go module
path, and names Octomaton's labels and configuration API group, independently of the hub's `kfirs.com`. Names and
wiring: [reference](../../hub/reference.md#octomaton). Migration from Octomatron:
[rename runbook](../runbooks/rename.md).

## Traffic

```mermaid
flowchart LR
  GH["GitHub App<br/>Octomaton"] -->|"POST /github/hooks"| LB
  GO["go command,<br/>proxy.golang.org"] -->|"GET /path?go-get=1"| LB
  BR["Browser"] -->|"GET /path"| LB
  LB["octomaton.dev<br/>ingress-public L4 LB"] --> GW["Traefik gateway public<br/>listener octomaton-dev"]
  GW -->|"Exact /github/hooks"| OC["octomaton/octomaton"]
  GW -->|"everything else"| GI["go-import/go-import<br/>Caddy"]
```

| Request | Answer |
| --- | --- |
| `POST /github/hooks` | Octomaton; unsigned deliveries get `401` |
| `GET /<path>?go-get=1` | `200` with `<meta name="go-import" content="octomaton.dev git https://github.com/arikkfir-org/octomaton">` |
| Anything else | `302` to `https://github.com/arikkfir-org/octomaton` |

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
| The hub serves the domain | Cloud DNS, cert-manager and the public gateway already exist; everything stays in Terraform and Argo CD | GitHub Pages |
| One host, two backends | The webhook stays the only path that reaches Octomaton; the rest is a static answer | A separate webhook host |
| Caddy, the hub's one `go-import` for every module domain ([design](../../delivery/designs/go-import-pages.md)) | No code to maintain; one workload answers for every hub module domain | Serving the tag from Octomaton itself; a Caddy of Octomaton's own in its namespace, which it ran until October 2026 |
| A certificate and listener of its own | A problem with the new domain can't block renewal of the `*.kfirs.com` certificate | A SAN on the wildcard certificate |
| GitHub App named `octomaton-dev`, with the repository as its homepage | `Octomaton`, the product's name, is taken on GitHub. Octomaton and the rulesets' required check identify the App by its ID, so the name only labels its checks and comments | `Octomaton` (taken); a homepage at `https://octomaton.dev` (which redirects to the repository anyway) |

## Security and failure modes

- The import page is static and public; it reveals only the repository URL, which is public anyway.
- Only `POST /github/hooks` reaches Octomaton, which verifies every delivery's HMAC signature first.
- Like the wildcard certificate, the `octomaton-dev` certificate is issued in the sync wave before the Gateways, so
  the Gateways wait for it: delegate the domain before the first sync.
- If `octomaton.dev` stops resolving (delegation, DNSSEC at the registrar, an expired domain), webhook deliveries
  fail until it recovers (redeliver them from the App's settings) and new `go install`s fail; versions already in the
  Go proxy keep working.
- Whoever holds the domain decides where new module versions come from; keep it renewed. The checksum database pins
  every published version, so tags are never moved.

## Open questions

- A product page at `https://octomaton.dev` would replace the redirect to the repository.
