# mcp

37signals MCP toolkit: the elements our MCP servers share, extracted from the
product instances that proved them. Sibling of the `sdk` and `cli` meta-repos.

[basecamp-mcp-server](https://github.com/basecamp/basecamp-mcp-server) and
[hey-mcp-server](https://github.com/basecamp/hey-mcp-server) are thin dispatch
layers over their product SDKs, exposing **domain gateway tools** — one MCP
tool per domain, `{"action", "params"}` dispatch, an in-band `describe` action,
read-only filtering, fail-closed domain narrowing. Building the second instance
duplicated the machinery under that surface. This repo is where the duplicated
parts live once, so the third instance (Fizzy) starts from a seed instead of a
copy.

## The two-instance proof rule

Code moves here only after **two product instances have proven it by
duplication** — the third validates it. Nothing lands speculatively: if only
one server runs it (today: hosted HTTP transport and the OAuth resource-server
plumbing, which exist only in basecamp-mcp-server), it stays in that server
until a second instance forces it to converge or diverge. Extraction is a move
of reviewed code, not a rewrite.

## Dependency direction

Products depend on `github.com/basecamp/mcp`. This module never imports a
product SDK or server — no `basecamp-sdk`, no `hey-sdk`. Anything that needs a
product client belongs in the product server, behind an interface defined here.

## Planned packages

Arriving as the extraction sequence lands (see the program board):

| Package | Extracted from | Contents |
|---------|----------------|----------|
| `mcptest` | both servers' test suites | in-memory MCP client/server wire-test harness, snapshot testing with `-update` |
| `gateway` | hey-mcp-server `internal/server`, basecamp-mcp-server `internal/tools` registry | gateway dispatch conventions, read-only and domain filtering over a small catalog interface |
| `catalog` | hey-mcp-server `internal/catalog` | Domain/Operation types and the behavior-model + OpenAPI join, product-parameterized (tool prefix, domain specs, embedded model snapshot) |

Per-product forever: domain curation and blurbs, vendored SDK model snapshots,
SDK clients and adapters, env var surfaces, product-specific tools and
resources.

## Development

The Go toolchain is pinned in `.mise.toml` and `go.mod`. Activate mise before
building:

```bash
eval "$(mise hook-env)" && make ready
```

`make ready` runs vet, the race-enabled tests, and the build — run it before
committing.
